package agent

import (
	"net"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
)

type frame = mux.Frame

const (
	kindRequest  = mux.KindRequest
	kindResponse = mux.KindResponse
	kindPty      = mux.KindPty
)

const (
	methodHello  = "hello"
	methodAttach = "attach"
	methodDetach = "detach"
	methodResize = "resize"
	methodFreeze = "freeze"
	methodThaw   = "thaw"
)

var (
	encodeExtra     = mux.EncodeExtra
	decodeExtra     = mux.DecodeExtra
	statusOf        = mux.StatusOf
	errorFromStatus = mux.ErrorFromStatus
)

type attachArgs struct {
	Window execenv.Window
	Stream uint64
}

type resizeArgs struct {
	Window execenv.Window
	Stream uint64
}

type streamArgs struct {
	Stream uint64
}

func newSession(conn net.Conn) *mux.Session {
	return mux.NewSession(conn)
}

// protocolVersion is the guest helper revision. The host and the helper
// refuse each other unless both speak exactly this revision. Bump it
// whenever a method, its arguments or its meaning changes.
const protocolVersion = 5
