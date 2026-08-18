package agent

import (
	"bytes"
	"encoding/gob"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/sudosylabs/execenv"
)

type kind uint8

const (
	kindRequest kind = iota + 1
	kindResponse
	kindPty
	kindWatch
)

type frame struct {
	Seq    uint64
	Kind   kind
	Method string
	Status string
	Extra  []byte
}

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

func encodeExtra(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeExtra(data []byte, dest any) error {
	if len(data) == 0 {
		return nil
	}
	return gob.NewDecoder(bytes.NewReader(data)).Decode(dest)
}

func statusOf(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, execenv.ErrInvalid):
		return "invalid"
	case errors.Is(err, execenv.ErrConflict):
		return "conflict"
	case errors.Is(err, execenv.ErrFrozen):
		return "frozen"
	case errors.Is(err, execenv.ErrRevoked):
		return "revoked"
	case errors.Is(err, execenv.ErrBusy):
		return "busy"
	case errors.Is(err, execenv.ErrClosed):
		return "closed"
	case errors.Is(err, execenv.ErrNotFound):
		return "not_found"
	case errors.Is(err, execenv.ErrLagged):
		return "lagged"
	case errors.Is(err, execenv.ErrTooLarge):
		return "too_large"
	default:
		return "unavailable"
	}
}

func errorFromStatus(status string) error {
	switch status {
	case "":
		return nil
	case "invalid":
		return execenv.ErrInvalid
	case "conflict":
		return execenv.ErrConflict
	case "frozen":
		return execenv.ErrFrozen
	case "revoked":
		return execenv.ErrRevoked
	case "busy":
		return execenv.ErrBusy
	case "closed":
		return execenv.ErrClosed
	case "not_found":
		return execenv.ErrNotFound
	case "lagged":
		return execenv.ErrLagged
	case "too_large":
		return execenv.ErrTooLarge
	default:
		return execenv.ErrUnavailable
	}
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

type session struct {
	conn    net.Conn
	enc     *gob.Encoder
	dec     *gob.Decoder
	writeMu sync.Mutex
}

func newSession(conn net.Conn) *session {
	return &session{conn: conn, enc: gob.NewEncoder(conn), dec: gob.NewDecoder(conn)}
}

func (s *session) send(f frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.enc.Encode(f)
}

func (s *session) recv() (frame, error) {
	var f frame
	err := s.dec.Decode(&f)
	if err == io.EOF {
		return f, err
	}
	return f, err
}

func (s *session) close() error {
	return s.conn.Close()
}
