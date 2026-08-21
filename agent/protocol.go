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
	kindWatch    = mux.KindWatch
)

const (
	methodReplace = "replace"
	methodApply   = "apply"
	methodOpen    = "open"
	methodWatch   = "watch"
	methodUnwatch = "unwatch"
	methodAttach  = "attach"
	methodDetach  = "detach"
	methodResize  = "resize"
	methodFreeze  = "freeze"
	methodThaw    = "thaw"
)

var (
	encodeExtra     = mux.EncodeExtra
	decodeExtra     = mux.DecodeExtra
	statusOf        = mux.StatusOf
	errorFromStatus = mux.ErrorFromStatus
)

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
	Stream uint64
}

type resizeArgs struct {
	Window execenv.Window
	Stream uint64
}

type streamArgs struct {
	Stream uint64
}

type watchArgs struct {
	After  execenv.Cursor
	Stream uint64
}

type watchResult struct {
	Cursor execenv.Cursor
}

func newSession(conn net.Conn) *mux.Session {
	return mux.NewSession(conn)
}
