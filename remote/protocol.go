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
	methodHangup                 = "hangup"
	methodResize                 = "resize"
)

// statusEOF ends a terminal stream whose shell exited. It is a PTY end
// status only, never an RPC status.
const statusEOF = "eof"

// maxClientPtyBytes bounds the PTY output one client Terminal buffers
// before its reader takes it. Past it the Terminal fails with ErrLagged
// and detaches, so the caller resumes from Offset instead of the client
// growing without bound.
const maxClientPtyBytes = 1 << 20

var (
	encodeExtra     = mux.EncodeExtra
	decodeExtra     = mux.DecodeExtra
	statusOf        = mux.StatusOf
	errorFromStatus = mux.ErrorFromStatus
)

// protocolVersion is the wire revision shared by New and Serve. Both refuse
// a peer on any other revision. Bump it whenever a method, its arguments or
// its meaning changes.
const protocolVersion = 5

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

// attachArgs carries an execenv.AttachRequest and the connection's new
// stream identity. Server PTY frames on that stream carry, in Seq, the
// absolute terminal offset of their first byte.
type attachArgs struct {
	Window   execenv.Window
	Stream   uint64
	Resume   *execenv.TerminalPosition
	Takeover bool
}

type attachResult struct {
	Terminal execenv.TerminalID
	Start    uint64
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
