// Package daemon loads operator config and runs the execenv host process.
//
// The daemon is a thin wrapper around remote.Serve. Local development may
// use the in-memory adapter. TLS (production) refuses that adapter.
package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
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
	// Isolation classes for the isolated adapter. vm is the default and
	// fails closed without virtualization hardware. user-kernel is an
	// explicit operator choice for hosts without it; it is never chosen
	// automatically.
	isolationVM         = string(execenv.IsolationVM)
	isolationUserKernel = string(execenv.IsolationUserKernel)
	minDiskBytes        = 64 << 20
)

// Config is the operator document that turns a machine into a host.
// Token, file bodies, and PTY octets must never be logged.
type Config struct {
	RequireJournal bool   `json:"require_journal,omitempty"`
	Listen         string `json:"listen"`
	Token          string `json:"token"`
	Security       string `json:"security"`
	Adapter        string `json:"adapter"`
	// Isolation selects the isolated adapter's boundary: "vm" (default)
	// or "user-kernel". Memory hosts must leave it empty.
	Isolation   string `json:"isolation,omitempty"`
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
	Slots       int     `json:"slots"`
	// Isolated grants use these as machine defaults. Memory ignores them.
	// DiskBytes sizes each grant's sparse workspace disk (default 2 GiB,
	// minimum 64 MiB); only written blocks use host storage.
	CPUMillis   int           `json:"cpu_millis,omitempty"`
	MemoryBytes int64         `json:"memory_bytes,omitempty"`
	DiskBytes   int64         `json:"disk_bytes,omitempty"`
	Network     string        `json:"network,omitempty"`
	Allow       []string      `json:"allow,omitempty"`
	Grace       time.Duration `json:"-"`
	GraceText   string        `json:"grace,omitempty"`
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
	tmp := path + ".new"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return execenv.Error("config", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return execenv.Error("config", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
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
	cfg.Isolation = strings.ToLower(cfg.Isolation)
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
		if cfg.Token == "" || cfg.TLSClientCA != "" {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	default:
		return execenv.Error("config", execenv.ErrInvalid)
	}
	switch cfg.Adapter {
	case adapterMemory:
		// Production isolation cannot be the in-memory adapter.
		if cfg.Security == securityTLS || cfg.Isolation != "" {
			return execenv.Error("config", execenv.ErrInvalid)
		}
	case adapterIsolated:
		switch cfg.Isolation {
		case "", isolationVM:
		case isolationUserKernel:
			// The native journal is certified only against a guest kernel's
			// own workload barrier.
			if cfg.RequireJournal {
				return execenv.Error("config", execenv.ErrInvalid)
			}
		default:
			return execenv.Error("config", execenv.ErrInvalid)
		}
		if cfg.JailUIDBase < 0 || cfg.JailUIDBase > 1<<31-1-65536 {
			return execenv.Error("config", execenv.ErrInvalid)
		}
		if cfg.DiskBytes < 0 || (cfg.DiskBytes > 0 && cfg.DiskBytes < minDiskBytes) {
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
	if cfg.Slots <= 0 {
		return execenv.Error("config", execenv.ErrInvalid)
	}
	if cfg.GraceText != "" {
		grace, err := time.ParseDuration(cfg.GraceText)
		if err != nil || grace <= 0 {
			return execenv.Error("config", execenv.ErrInvalid)
		}
		cfg.Grace = grace
	}
	return nil
}

// isolation is the effective boundary class, for logs and the adapter.
func (cfg Config) isolation() string {
	switch {
	case cfg.Adapter == adapterMemory:
		return string(execenv.IsolationNone)
	case cfg.Isolation == "":
		return isolationVM
	default:
		return cfg.Isolation
	}
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
	return fmt.Sprintf("listen=%s adapter=%s isolation=%s security=%s slots=%d images=%d",
		cfg.Listen, cfg.Adapter, cfg.isolation(), cfg.Security, cfg.Slots, len(cfg.Images))
}
