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
	kindCredit   = mux.KindCredit
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

// ptyWindow is the flow-control window each side grants per terminal
// stream and direction in the attach exchange: the client for output in
// attachArgs.Credit, the host for input in attachResult.Credit. A sender
// never has more than the window of unacknowledged PTY bytes on a stream;
// the receiver returns credit in kindCredit frames as its caller consumes,
// once at least half the window is consumed.
const ptyWindow = 256 << 10

// maxPtyWindow bounds the window a peer may announce.
const maxPtyWindow = 16 << 20

var (
	encodeExtra     = mux.EncodeExtra
	decodeExtra     = mux.DecodeExtra
	statusOf        = mux.StatusOf
	errorFromStatus = mux.ErrorFromStatus
)

// protocolVersion is the wire revision shared by New and Serve. Both refuse
// a peer on any other revision. Bump it whenever a method, its arguments or
// its meaning changes.
const protocolVersion = 9

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
// absolute terminal offset of their first byte. Credit is the output
// window the client grants the host on the stream.
type attachArgs struct {
	Window   execenv.Window
	Stream   uint64
	Resume   *execenv.TerminalPosition
	Takeover bool
	Credit   uint64
}

// attachResult names the attached terminal. Credit is the input window
// the host grants the client on the stream.
type attachResult struct {
	Terminal execenv.TerminalID
	Start    uint64
	Credit   uint64
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
