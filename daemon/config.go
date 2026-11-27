// Package daemon loads operator config and runs the execenv host process.
//
// The daemon is a thin wrapper around remote.Serve. Local development may
// use the in-memory adapter. TLS (production) refuses that adapter.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/capacity"
	"github.com/sudosylabs/execenv/isolated"
	"github.com/sudosylabs/execenv/remote"
)

const (
	adapterMemory   = "memory"
	adapterIsolated = "isolated"
	securityTLS     = "tls"
	securityLocal   = "insecure_local"
	networkNone     = "none"
	networkAllow    = "allowlist"
)

// Resources is a size in the host configuration: a host's capacity, or one
// grant's.
type Resources struct {
	CPUMillis   int64 `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
	DiskBytes   int64 `json:"disk_bytes"`
}

func (r Resources) contract() execenv.Resources {
	return execenv.Resources{CPUMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes, DiskBytes: r.DiskBytes}
}

// Config is the operator document that turns a machine into a host.
// Token, file bodies, and PTY octets must never be logged.
type Config struct {
	Listen      string `json:"listen"`
	Token       string `json:"token"`
	Security    string `json:"security"`
	Adapter     string `json:"adapter"`
	TLSCert     string `json:"tls_cert,omitempty"`
	TLSKey      string `json:"tls_key,omitempty"`
	TLSClientCA string `json:"tls_client_ca,omitempty"`
	WorkDir     string `json:"work_dir,omitempty"`
	Device      string `json:"device,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Supervisor  string `json:"supervisor,omitempty"`
	// Mkfs is the ext4 formatter for per-grant workspace disks. Empty
	// uses mkfs.ext4 from PATH.
	Mkfs string `json:"mkfs,omitempty"`
	// JailUIDBase is the first of 65536 host uids reserved for jailed
	// microVM monitors (default 2000000000). Nothing else may use them.
	JailUIDBase int     `json:"jail_uid_base,omitempty"`
	Images      []Image `json:"images"`
	// Shapes are the machine sizes the isolated adapter saves templates
	// in, for every image: a grant of exactly a shape's memory starts
	// from its template instead of booting. Empty builds none.
	Shapes []isolated.Shape `json:"shapes,omitempty"`
	// Capacity is what the host gives its grants in all. Every grant
	// reserves its whole size from it. CPUOvercommit multiplies the CPU
	// total (1 when unset); memory and disk are never overcommitted.
	Capacity      Resources `json:"capacity"`
	CPUOvercommit float64   `json:"cpu_overcommit,omitempty"`
	// GrantDefaults is what a grant gets when its Spec leaves a size unset
	// (execenv.DefaultGrant when unset); GrantMax the most one grant may ask for
	// (the capacity when unset). A grant's disk is a sparse file: only
	// written blocks use host storage.
	GrantDefaults Resources `json:"grant_defaults,omitzero"`
	GrantMax      Resources `json:"grant_max,omitzero"`
	Network       string    `json:"network,omitempty"`
	Allow         []string  `json:"allow,omitempty"`
	// OperatorSocket is the absolute path of the Unix socket execenvctl
	// uses to drain this daemon before a restart. Empty serves none, and
	// execenvctl then cannot tell whether grants are live.
	OperatorSocket string `json:"operator_socket,omitempty"`
}

// Image is one catalog entry. Memory only needs ID. Isolated grants use
// Kernel and Rootfs. JSON may say "rootfs" or the older "path" key.
type Image struct {
	ID     string `json:"id"`
	Kernel string `json:"kernel"`
	Rootfs string `json:"rootfs"`
	Hash   string `json:"hash"`
}

type imageJSON struct {
	ID     string `json:"id"`
	Kernel string `json:"kernel"`
	Rootfs string `json:"rootfs"`
	Path   string `json:"path"`
	Hash   string `json:"hash"`
}

func (img *Image) UnmarshalJSON(data []byte) error {
	var raw imageJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	img.ID = raw.ID
	img.Kernel = raw.Kernel
	img.Hash = raw.Hash
	img.Rootfs = raw.Rootfs
	if img.Rootfs == "" {
		img.Rootfs = raw.Path
	}
	return nil
}

func (img Image) MarshalJSON() ([]byte, error) {
	type canonical struct {
		ID     string `json:"id"`
		Kernel string `json:"kernel,omitempty"`
		Rootfs string `json:"rootfs,omitempty"`
		Hash   string `json:"hash,omitempty"`
	}
	return json.Marshal(canonical{ID: img.ID, Kernel: img.Kernel, Rootfs: img.Rootfs, Hash: img.Hash})
}

// Load reads and validates a JSON config file. It does not listen and does
// not occupy grants. Errors never include the token value.
func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, execenv.Error("config", execenv.ErrInvalid)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, execenv.Error("config", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// json.Unmarshal errors quote snippets. Rebuild a snippet-free
		// failure so a token sitting next to a syntax error cannot leak.
		return Config{}, execenv.Error("config", execenv.ErrInvalid)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save validates cfg and writes it as Host configuration. Canonical keys
// only (rootfs, not path). Invalid cfg leaves path unchanged. Mode 0600.
func Save(path string, cfg Config) error {
	if path == "" {
		return execenv.Error("config", execenv.ErrInvalid)
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if cfg.Images == nil {
		cfg.Images = []Image{}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return execenv.Error("config", execenv.ErrInvalid)
	}
	raw = append(raw, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), ".host-config-")
	if err != nil {
		return execenv.Error("config", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return execenv.Error("config", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return execenv.Error("config", err)
	}
	if err := file.Close(); err != nil {
		return execenv.Error("config", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return execenv.Error("config", err)
	}
	return nil
}

func (cfg *Config) validate() error {
	if cfg.Listen == "" || cfg.Security == "" || cfg.Adapter == "" {
		return execenv.Error("config", execenv.ErrInvalid)
	}
	cfg.Security = strings.ToLower(cfg.Security)
	cfg.Adapter = strings.ToLower(cfg.Adapter)
	if cfg.Network == "" {
		cfg.Network = networkNone
	}
	cfg.Network = strings.ToLower(cfg.Network)
	switch cfg.Security {
	case securityTLS:
		if cfg.TLSCert == "" || cfg.TLSKey == "" {
			return execenv.Error("config", execenv.ErrInvalid)
		}
		if cfg.Token == "" && cfg.TLSClientCA == "" {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	case securityLocal:
		host, _, err := net.SplitHostPort(cfg.Listen)
		if err != nil || !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() ||
			cfg.Token == "" || cfg.TLSClientCA != "" {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	default:
		return execenv.Error("config", execenv.ErrInvalid)
	}
	switch cfg.Adapter {
	case adapterMemory:
		// Production isolation cannot be the in-memory adapter.
		if cfg.Security == securityTLS {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	case adapterIsolated:
		if cfg.JailUIDBase < 0 || cfg.JailUIDBase > 1<<31-1-65536 {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	default:
		return execenv.Error("config", execenv.ErrInvalid)
	}
	switch cfg.Network {
	case networkNone:
		if len(cfg.Allow) > 0 {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	case networkAllow:
		if len(cfg.Allow) == 0 {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	default:
		return execenv.Error("config", execenv.ErrInvalid)
	}
	// An empty catalog is a host that cannot occupy grants yet. install
	// adds disks later; Ensure of a missing id stays unknown image.
	for _, image := range cfg.Images {
		if err := execenv.ValidateSpec(execenv.Spec{ID: "ok", Image: execenv.Image(image.ID)}); err != nil {
			return execenv.Error("config", execenv.ErrInvalid)
		}
		if cfg.Adapter == adapterIsolated {
			// Isolated grants boot these files. Hash is SHA-256 of kernel
			// then rootfs, 64 hex digits, so Ready never advertises junk.
			if image.Kernel == "" || image.Rootfs == "" || !isolated.ValidDigest(image.Hash) {
				return execenv.Error("config", execenv.ErrInvalid)
			}
		}
	}
	if cfg.CPUOvercommit < 0 {
		return execenv.Error("config", execenv.ErrInvalid)
	}
	// The pool checks the sizes against each other and the minimums.
	if _, err := capacity.New(cfg.totals(), cfg.GrantDefaults.contract(), cfg.GrantMax.contract()); err != nil {
		return execenv.Error("config", execenv.ErrInvalid)
	}
	// The daemon's working directory is not the operator's, so the socket
	// path must be absolute. A path too long to bind fails at startup.
	if cfg.OperatorSocket != "" && !filepath.IsAbs(cfg.OperatorSocket) {
		return execenv.Error("config", execenv.ErrInvalid)
	}
	return nil
}

// totals is the capacity the host gives grants, CPU overcommit included.
func (cfg Config) totals() execenv.Resources {
	total := cfg.Capacity.contract()
	if cfg.CPUOvercommit > 0 {
		total.CPUMillis = int64(float64(total.CPUMillis) * cfg.CPUOvercommit)
	}
	return total
}

// isolation is the boundary class the configured adapter provides, for logs.
func (cfg Config) isolation() execenv.Isolation {
	if cfg.Adapter == adapterMemory {
		return execenv.IsolationNone
	}
	return execenv.IsolationVM
}

func (cfg Config) security() remote.Security {
	if cfg.Security == securityLocal {
		return remote.SecurityInsecureLocal
	}
	return remote.SecurityTLS
}

func (cfg Config) imageIDs() []execenv.Image {
	out := make([]execenv.Image, 0, len(cfg.Images))
	for _, image := range cfg.Images {
		out = append(out, execenv.Image(image.ID))
	}
	return out
}

// redacted is safe for operator logs. It never includes Token.
func (cfg Config) redacted() string {
	total := cfg.totals()
	return fmt.Sprintf("listen=%s adapter=%s isolation=%s security=%s cpu_millis=%d memory_bytes=%d disk_bytes=%d images=%d",
		cfg.Listen, cfg.Adapter, cfg.isolation(), cfg.Security, total.CPUMillis, total.MemoryBytes, total.DiskBytes, len(cfg.Images))
}
