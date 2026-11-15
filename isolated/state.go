package isolated

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sudosylabs/execenv"
)

// stateName is a grant's state file in its grant directory. It is what a
// restarted host needs to adopt the grant: who it is, where its control
// stands, and how to find its machine.
const stateName = "state.json"

// grantState is written whole, atomically, each time it changes.
type grantState struct {
	Spec    execenv.Spec           `json:"spec"`
	Epoch   string                 `json:"epoch"`
	Receipt execenv.ControlReceipt `json:"receipt"`
	Started time.Time              `json:"started"`
	// Paused says the machine is stopped under a confirmed freeze.
	Paused  bool    `json:"paused,omitempty"`
	Machine machine `json:"machine"`
	// Lost marks a grant whose machine ended without a revoke; it stays
	// lost across restarts until the caller revokes it.
	Lost bool `json:"lost,omitempty"`
	// Suspended says the grant's machine is saved in its grant directory
	// and not running.
	Suspended bool `json:"suspended,omitempty"`
}

// machine finds a grant's machine again. PID and Start name the monitor
// process for good; the rest is what releasing it needs.
type machine struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
	UID   int    `json:"uid"`
	// Jail is the jail directory; Exec names the cgroups the supervisor
	// made.
	Jail string `json:"jail"`
	Exec string `json:"exec"`
	Sock string `json:"sock"`
	API  string `json:"api"`
	// Net is the grant's link, when it has one.
	Net *netRecord `json:"net,omitempty"`
}

type netRecord struct {
	Dev     string   `json:"dev"`
	MAC     string   `json:"mac"`
	GuestIP string   `json:"guest_ip"`
	HostIP  string   `json:"host_ip"`
	Prefix  int      `json:"prefix"`
	Allow   []string `json:"allow"`
}

func netRecordOf(att *netAttach) *netRecord {
	if att == nil {
		return nil
	}
	rec := &netRecord{Dev: att.Dev, MAC: att.MAC, GuestIP: att.GuestIP, HostIP: att.HostIP, Prefix: att.Prefix}
	for _, n := range att.Allow {
		rec.Allow = append(rec.Allow, n.String())
	}
	return rec
}

func (r *netRecord) attach() (*netAttach, error) {
	if r == nil {
		return nil, nil
	}
	allow, err := parseAllow(r.Allow)
	if err != nil {
		return nil, err
	}
	att := &netAttach{Dev: r.Dev, MAC: r.MAC, GuestIP: r.GuestIP, HostIP: r.HostIP, Prefix: r.Prefix, Allow: allow}
	att.cleanup = func() { releaseAttach(att) }
	return att, nil
}

func statePath(workDir string, id execenv.ID) string {
	return filepath.Join(workDir, "grants", string(id), stateName)
}

// saveState replaces the grant's state file: a temporary file, synced,
// renamed over the old one, then the directory synced.
func saveState(workDir string, s grantState) error {
	path := statePath(workDir, s.Spec.ID)
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// loadStates reads every grant's state file. A grant directory without a
// readable one is a grant that never finished starting.
func loadStates(workDir string) (map[execenv.ID]grantState, []string, error) {
	dir := filepath.Join(workDir, "grants")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	states := map[execenv.ID]grantState{}
	var orphans []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name(), stateName))
		var s grantState
		if err != nil || json.Unmarshal(body, &s) != nil || string(s.Spec.ID) != entry.Name() || execenv.ValidateSpec(s.Spec) != nil {
			orphans = append(orphans, filepath.Join(dir, entry.Name()))
			continue
		}
		states[s.Spec.ID] = s
	}
	return states, orphans, nil
}
