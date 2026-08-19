package ctl

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/sudosylabs/execenv/daemon"
)

// Isolation device and supervisor process names as the probe looks them up.
// Public execenv types stay hypervisor-neutral; this operator tool cannot.
const (
	defaultDevice     = "/dev/kvm"
	runtimeName       = "firecracker"
	supervisorName    = "jailer"
	runtimeVersion    = "v1.10.1"
	defaultListen     = "0.0.0.0:8443"
	defaultSlots      = 8
	defaultPrefix     = "/usr/local"
	defaultSysconf    = "/etc/execenv"
	defaultState      = "/var/lib/execenv"
	hostConfigName    = "host.json"
	unitName          = "execenv.service"
	execenvName       = "execenv"
	networkNone       = "none"
	securityTLS       = "tls"
	securityLocal     = "insecure_local"
	adapterIsolated   = "isolated"
	defaultGrace      = "30s"
)

// runtimeArchiveBase is the operator-time download for missing supervisor
// binaries. Tests may replace it. Grant time never uses this.
var runtimeArchiveBase = "https://github.com/firecracker-microvm/firecracker/releases/download"

// Options is a headless bootstrap. Empty path fields use system defaults
// and typically need root. Prefix is for tests and non-root labs.
type Options struct {
	Prefix   string
	Sysconf  string
	State    string
	Device   string
	Listen   string
	Slots    int
	Execenv    string
	NoStart    bool
	NoFetch    bool
	Insecure   bool
	ReleaseURL string
	// Reload, if set, replaces the systemd reload after catalog changes.
	Reload func() error
}

// Defaults returns the system-wide install layout.
func Defaults() Options {
	return Options{
		Prefix:  defaultPrefix,
		Sysconf: defaultSysconf,
		State:   defaultState,
		Device:  defaultDevice,
		Listen:  defaultListen,
		Slots:   defaultSlots,
	}
}

func (o Options) resolved() Options {
	out := o
	if out.Prefix == "" {
		out.Prefix = defaultPrefix
	}
	if out.Sysconf == "" {
		out.Sysconf = defaultSysconf
	}
	if out.State == "" {
		out.State = defaultState
	}
	if out.Device == "" {
		out.Device = defaultDevice
	}
	if out.Listen == "" {
		out.Listen = defaultListen
	}
	if out.Slots <= 0 {
		out.Slots = defaultSlots
	}
	return out
}

func (o Options) binDir() string    { return filepath.Join(o.Prefix, "bin") }
func (o Options) configPath() string { return filepath.Join(o.Sysconf, hostConfigName) }
func (o Options) workDir() string    { return filepath.Join(o.State, "work") }
func (o Options) unitDir() string    { return filepath.Join(filepath.Dir(o.Sysconf), "systemd", "system") }
func (o Options) unitPath() string   { return filepath.Join(o.unitDir(), unitName) }
func (o Options) runtimePath() string {
	return filepath.Join(o.binDir(), runtimeName)
}
func (o Options) supervisorPath() string {
	return filepath.Join(o.binDir(), supervisorName)
}
func (o Options) execenvPath() string {
	return filepath.Join(o.binDir(), execenvName)
}

// Bootstrap turns an empty isolation host into a configured execenv daemon.
// It does not occupy grants and does not fetch catalog disks. A missing
// isolation device fails closed; there is no container fallback.
func Bootstrap(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
	if err := checkDevice(opts.Device); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.binDir(), 0o755); err != nil {
		return wrap("bootstrap", err)
	}
	if err := os.MkdirAll(opts.workDir(), 0o700); err != nil {
		return wrap("bootstrap", err)
	}
	if err := os.MkdirAll(opts.Sysconf, 0o755); err != nil {
		return wrap("bootstrap", err)
	}
	if err := os.MkdirAll(opts.unitDir(), 0o755); err != nil {
		return wrap("bootstrap", err)
	}
	if err := installExecenv(opts); err != nil {
		return err
	}
	if err := installSupervisors(opts); err != nil {
		return err
	}
	if err := writeHostConfig(opts); err != nil {
		return err
	}
	if err := writeUnit(opts); err != nil {
		return err
	}
	if err := startUnit(opts); err != nil {
		return err
	}
	if stdout != nil {
		fmt.Fprintln(stdout, "execenv installed")
		fmt.Fprintf(stdout, "config=%s\n", opts.configPath())
		fmt.Fprintf(stdout, "image=%s\n", imageSummary(opts))
		fmt.Fprintln(stdout, "device=ok")
		fmt.Fprintln(stdout, "token=written to config")
		fmt.Fprintf(stdout, "hash=%s\n", hashSummary(opts))
		fmt.Fprintln(stdout, "ready=device+runtime+supervisor")
	}
	return nil
}

func checkDevice(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("isolation device %s is missing or not usable; this host cannot occupy grants (no container fallback)", path)
	}
	_ = f.Close()
	return nil
}

func installExecenv(opts Options) error {
	src := opts.Execenv
	if src == "" {
		src = opts.execenvPath()
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("need --execenv (linux %s binary)", execenvName)
	}
	return installFile(src, opts.execenvPath(), 0o755)
}

func installSupervisors(opts Options) error {
	if err := ensureBinary(opts.runtimePath(), runtimeName); err != nil && !opts.NoFetch {
		if ferr := fetchSupervisors(opts); ferr != nil {
			return ferr
		}
	}
	if err := ensureBinary(opts.supervisorPath(), supervisorName); err != nil && !opts.NoFetch {
		if ferr := fetchSupervisors(opts); ferr != nil {
			return ferr
		}
	}
	if err := ensureBinary(opts.runtimePath(), runtimeName); err != nil {
		return fmt.Errorf("runtime and supervisor binaries are required")
	}
	if err := ensureBinary(opts.supervisorPath(), supervisorName); err != nil {
		return fmt.Errorf("runtime and supervisor binaries are required")
	}
	return nil
}

func ensureBinary(dest, name string) error {
	if executable(dest) {
		return nil
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	return installFile(found, dest, 0o755)
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func fetchSupervisors(opts Options) error {
	arch, err := releaseArch()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/firecracker-%s-%s.tgz", runtimeArchiveBase, runtimeVersion, runtimeVersion, arch)
	tgz := filepath.Join(opts.State, "runtime.tgz")
	if err := fetchFile(url, tgz); err != nil {
		return wrap("bootstrap", err)
	}
	defer os.Remove(tgz)
	if err := extractNamed(tgz, opts.State, runtimeName+"-"+runtimeVersion+"-"+arch, opts.runtimePath()); err != nil {
		return err
	}
	if err := extractNamed(tgz, opts.State, supervisorName+"-"+runtimeVersion+"-"+arch, opts.supervisorPath()); err != nil {
		return err
	}
	return nil
}

func releaseArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("unsupported machine %s", runtime.GOARCH)
	}
}

func fetchFile(url, dest string) error {
	cmd := exec.Command("curl", "-fsSL", url, "-o", dest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fetch %s: %w", filepath.Base(dest), errTrim(out, err))
	}
	return nil
}

func extractNamed(tgz, dir, name, dest string) error {
	cmd := exec.Command("tar", "-xzf", tgz, "-C", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract runtime archive: %w", errTrim(out, err))
	}
	var found string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if info.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return fmt.Errorf("runtime archive is missing a required binary")
	}
	return installFile(found, dest, 0o755)
}

func writeHostConfig(opts Options) error {
	existing, err := loadHost(opts.configPath())
	first := errors.Is(err, os.ErrNotExist)
	if err != nil && !first {
		return err
	}
	token := existing.Token
	if token == "" {
		var buf [32]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return wrap("bootstrap", err)
		}
		token = hex.EncodeToString(buf[:])
	}
	security := securityTLS
	certPath := filepath.Join(opts.Sysconf, "tls.crt")
	keyPath := filepath.Join(opts.Sysconf, "tls.key")
	// Re-run must keep TLS files even if the operator passes --insecure.
	if existing.TLSCert != "" && existing.TLSKey != "" && fileExists(existing.TLSCert) && fileExists(existing.TLSKey) {
		certPath = existing.TLSCert
		keyPath = existing.TLSKey
		if err := ensureTLS(certPath, keyPath); err != nil {
			return err
		}
	} else if opts.Insecure {
		security = securityLocal
		certPath = ""
		keyPath = ""
	} else {
		if err := ensureTLS(certPath, keyPath); err != nil {
			return err
		}
	}
	doc := existing
	doc.Listen = opts.Listen
	doc.Token = token
	doc.Security = security
	doc.TLSCert = certPath
	doc.TLSKey = keyPath
	doc.WorkDir = opts.workDir()
	doc.Device = opts.Device
	doc.Runtime = opts.runtimePath()
	doc.Supervisor = opts.supervisorPath()
	doc.Slots = opts.Slots
	if doc.Adapter == "" {
		doc.Adapter = adapterIsolated
	}
	if first {
		doc.Network = networkNone
		doc.Allow = nil
		doc.GraceText = defaultGrace
	}
	return daemon.Save(opts.configPath(), doc)
}

func imageSummary(opts Options) string {
	existing, err := loadHost(opts.configPath())
	if err != nil || len(existing.Images) == 0 {
		return "none"
	}
	return existing.Images[0].ID
}

func hashSummary(opts Options) string {
	existing, err := loadHost(opts.configPath())
	if err != nil || len(existing.Images) == 0 || existing.Images[0].Hash == "" {
		return "none"
	}
	return "written to config"
}

func writeUnit(opts Options) error {
	body := "[Unit]\n" +
		"Description=execenv host\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=" + opts.execenvPath() + " -config " + opts.configPath() + "\n" +
		"Restart=on-failure\n" +
		"NoNewPrivileges=true\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
	if err := os.WriteFile(opts.unitPath(), []byte(body), 0o644); err != nil {
		return wrap("bootstrap", err)
	}
	return nil
}

func startUnit(opts Options) error {
	if opts.NoStart {
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", errTrim(out, err))
	}
	if out, err := exec.Command("systemctl", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w", errTrim(out, err))
	}
	return nil
}

func installFile(src, dest string, mode os.FileMode) error {
	if sameFile(src, dest) {
		return os.Chmod(dest, mode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return wrap("bootstrap", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return wrap("bootstrap", err)
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return wrap("bootstrap", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return wrap("bootstrap", err)
	}
	return os.Chmod(dest, mode)
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("execenvctl %s: %w", op, err)
}

func errTrim(out []byte, err error) error {
	msg := string(bytes.TrimSpace(out))
	if msg == "" {
		return err
	}
	return fmt.Errorf("%s", msg)
}