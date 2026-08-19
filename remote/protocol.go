package remote

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
	kindWatch    = mux.KindWatch
)

const (
	methodAuth    = "auth"
	methodReady   = "ready"
	methodEnsure  = "ensure"
	methodRevoke  = "revoke"
	methodAttach  = "attach"
	methodDetach  = "detach"
	methodResize  = "resize"
	methodFreeze  = "freeze"
	methodThaw    = "thaw"
	methodReplace = "replace"
	methodApply   = "apply"
	methodWatch   = "watch"
	methodUnwatch = "unwatch"
	methodOpen    = "open"
)

var (
	encodeExtra     = mux.EncodeExtra
	decodeExtra     = mux.DecodeExtra
	statusOf        = mux.StatusOf
	errorFromStatus = mux.ErrorFromStatus
)

type authArgs struct {
	Token []byte
}

type ensureArgs struct {
	Spec execenv.Spec
}

type replaceArgs struct {
	Tree execenv.Tree
}

type applyArgs struct {
	Batch execenv.Batch
}

type openArgs struct {
	Path string
}

type attachArgs struct {
	Window execenv.Window
}

type resizeArgs struct {
	Window execenv.Window
}

func newSession(conn net.Conn) *mux.Session {
	return mux.NewSession(conn)
}
