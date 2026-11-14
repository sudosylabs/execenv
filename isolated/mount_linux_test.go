//go:build linux

package isolated

import (
	"os"
	"testing"
)

func TestUnescapeMount(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`/var/lib/a`:          `/var/lib/a`,
		`/var/lib/with\040sp`: `/var/lib/with sp`,
		`/x\134y`:             `/x\y`,
		`/trailing\04`:        `/trailing\04`,
	} {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProcUIDReadsRealUID(t *testing.T) {
	t.Parallel()
	uid, ok := procUID("/proc/self/status")
	if !ok || uid != os.Getuid() {
		t.Fatalf("procUID = %d, %v", uid, ok)
	}
}
