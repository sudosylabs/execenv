//go:build isolation

package isolated

import (
	"os"
	"runtime"
	"testing"

	"github.com/sudosylabs/execenv"
)

// These tests talk to a real isolation device and supervisor binaries.
// They are not part of make check. Run:
//
//	go test -tags=isolation ./isolated
func TestLiveReadyRequiresDevice(t *testing.T) {
	if os.Getenv("EXECENV_ISOLATION") != "1" {
		t.Skip("set EXECENV_ISOLATION=1 on a machine with isolation hardware")
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolation tests are linux-only")
	}
	host, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   1,
		Images:  []Image{writeCatalogImage(t, t.TempDir(), "default", "rootfs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Usable {
		t.Fatal("Ready() Usable = false on an isolation-enabled host")
	}
}
