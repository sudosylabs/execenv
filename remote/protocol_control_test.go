package remote

import (
	"errors"
	"github.com/sudosylabs/execenv"
	"testing"
)

func TestAuthRejectsOldWireEvenWithMatchingRelease(t *testing.T) {
	sc := &serverConn{cfg: ServerConfig{Token: []byte("token")}, release: "dev"}
	for _, version := range []int{0, 1, protocolVersion + 1} {
		extra, err := encodeExtra(authArgs{Token: []byte("token"), Release: "dev", Protocol: version})
		if err != nil {
			t.Fatal(err)
		}
		if err := sc.auth(frame{Extra: extra}); !errors.Is(err, execenv.ErrUnavailable) {
			t.Fatalf("version %d: %v", version, err)
		}
	}
}
