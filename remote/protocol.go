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
)

const (
	methodProjectionInitialize   = "projection_initialize"
	methodProjectionApply        = "projection_apply"
	methodProjectionUploadBegin  = "projection_upload_begin"
	methodProjectionUploadChunk  = "projection_upload_chunk"
	methodProjectionUploadFinish = "projection_upload_finish"
	methodProjectionUploadAbort  = "projection_upload_abort"
	methodRevokeEpoch            = "revoke_epoch"
	methodControl                = "control"
	methodControlStatus          = "control_status"
	methodAuth                   = "auth"
	methodReady                  = "ready"
	methodEnsure                 = "ensure"
	methodRevoke                 = "revoke"
	methodAttach                 = "attach"
	methodDetach                 = "detach"
	methodResize                 = "resize"
)

var (
	encodeExtra     = mux.EncodeExtra
	decodeExtra     = mux.DecodeExtra
	statusOf        = mux.StatusOf
	errorFromStatus = mux.ErrorFromStatus
)

// protocolVersion is the wire revision shared by New and Serve. Both refuse
// a peer on any other revision. Bump it whenever a method, its arguments or
// its meaning changes.
const protocolVersion = 4

type authResult struct {
	Protocol     int
	Capabilities execenv.Capabilities
}

type authArgs struct {
	Protocol int
	Token    []byte
	Release  string
}

type ensureArgs struct {
	Spec execenv.Spec
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

func newSession(conn net.Conn) *mux.Session {
	return mux.NewSession(conn)
}

type ensureResult struct{ Epoch string }
