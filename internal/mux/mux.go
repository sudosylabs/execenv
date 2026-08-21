// Package mux is the shared frame, session, and status map used by the
// agent hop and the remote hop. It is not a hop of its own.
//
// Agent and remote keep their own method tables. Closing a PTY is hangup,
// not connection teardown. Status is a sentinel name, never octets.
// Grant callers never import this package.
package mux

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sudosylabs/execenv"
)

const (
	// MaxFrameBytes bounds allocation before structured decoding. It is larger
	// than MaxTreeBytes to leave room for base64 bodies and node metadata.
	MaxFrameBytes       = 72 << 20
	MaxPtyBytes         = 64 << 10
	defaultWriteTimeout = 30 * time.Second
	fixedFrameBytes     = 35
)

// Kind is the only on-wire discriminant.
type Kind uint8

const (
	KindRequest Kind = iota + 1
	KindResponse
	KindPty
	KindWatch
)

// Frame is the only encoded value. Status is a sentinel name.
type Frame struct {
	Seq    uint64
	Kind   Kind
	Method string
	Grant  string
	Stream uint64
	Status string
	Extra  []byte
	// Deadline is the caller's Unix-nanosecond operation deadline. Servers
	// cap it with their own configured maximum.
	Deadline int64
}

// Session is one accepted or dialed connection. One reader owns Decode.
// All writes take a lock so PTY and RPC frames stay sequenced.
type Session struct {
	conn         net.Conn
	writeMu      sync.Mutex
	writeTimeout time.Duration
}

// NewSession takes an already-dialed connection.
func NewSession(conn net.Conn) *Session {
	return &Session{
		conn:         conn,
		writeTimeout: defaultWriteTimeout,
	}
}

func (s *Session) Send(f Frame) error {
	if err := validateFrame(f); err != nil {
		return err
	}
	payload, err := encodeFrame(f)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrameBytes {
		return execenv.ErrTooLarge
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
		defer s.conn.SetWriteDeadline(time.Time{})
	}
	if err := writeAll(s.conn, header[:]); err != nil {
		return err
	}
	return writeAll(s.conn, payload)
}

func (s *Session) Recv() (Frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(s.conn, header[:]); err != nil {
		return Frame{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return Frame{}, execenv.ErrInvalid
	}
	if size > MaxFrameBytes {
		return Frame{}, execenv.ErrTooLarge
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(s.conn, payload); err != nil {
		return Frame{}, err
	}
	f, err := decodeFrame(payload)
	if err != nil {
		return Frame{}, err
	}
	if err := validateFrame(f); err != nil {
		return Frame{}, err
	}
	return f, nil
}

func (s *Session) Close() error {
	return s.conn.Close()
}

// EncodeExtra JSON-encodes a method payload. Nil is an empty Extra.
func EncodeExtra(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFrameBytes-fixedFrameBytes {
		return nil, execenv.ErrTooLarge
	}
	return data, nil
}

// DecodeExtra JSON-decodes one bounded method payload into dest.
func DecodeExtra(data []byte, dest any) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > MaxFrameBytes {
		return execenv.ErrTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return execenv.ErrInvalid
	}
	return nil
}

func encodeFrame(f Frame) ([]byte, error) {
	size := fixedFrameBytes + len(f.Method) + len(f.Grant) + len(f.Status) + len(f.Extra)
	if size > MaxFrameBytes {
		return nil, execenv.ErrTooLarge
	}
	payload := make([]byte, size)
	binary.BigEndian.PutUint64(payload[0:8], f.Seq)
	payload[8] = byte(f.Kind)
	binary.BigEndian.PutUint64(payload[9:17], f.Stream)
	binary.BigEndian.PutUint64(payload[17:25], uint64(f.Deadline))
	binary.BigEndian.PutUint16(payload[25:27], uint16(len(f.Method)))
	binary.BigEndian.PutUint16(payload[27:29], uint16(len(f.Grant)))
	binary.BigEndian.PutUint16(payload[29:31], uint16(len(f.Status)))
	binary.BigEndian.PutUint32(payload[31:35], uint32(len(f.Extra)))
	offset := fixedFrameBytes
	offset += copy(payload[offset:], f.Method)
	offset += copy(payload[offset:], f.Grant)
	offset += copy(payload[offset:], f.Status)
	copy(payload[offset:], f.Extra)
	return payload, nil
}

func decodeFrame(payload []byte) (Frame, error) {
	if len(payload) < fixedFrameBytes {
		return Frame{}, execenv.ErrInvalid
	}
	methodLen := int(binary.BigEndian.Uint16(payload[25:27]))
	grantLen := int(binary.BigEndian.Uint16(payload[27:29]))
	statusLen := int(binary.BigEndian.Uint16(payload[29:31]))
	rawExtraLen := binary.BigEndian.Uint32(payload[31:35])
	if rawExtraLen > MaxFrameBytes-fixedFrameBytes {
		return Frame{}, execenv.ErrTooLarge
	}
	extraLen := int(rawExtraLen)
	if fixedFrameBytes+methodLen+grantLen+statusLen+extraLen != len(payload) {
		return Frame{}, execenv.ErrInvalid
	}
	offset := fixedFrameBytes
	method := string(payload[offset : offset+methodLen])
	offset += methodLen
	grant := string(payload[offset : offset+grantLen])
	offset += grantLen
	status := string(payload[offset : offset+statusLen])
	offset += statusLen
	f := Frame{
		Seq:      binary.BigEndian.Uint64(payload[0:8]),
		Kind:     Kind(payload[8]),
		Stream:   binary.BigEndian.Uint64(payload[9:17]),
		Deadline: int64(binary.BigEndian.Uint64(payload[17:25])),
		Method:   method,
		Grant:    grant,
		Status:   status,
		Extra:    append([]byte(nil), payload[offset:]...),
	}
	return f, nil
}

func validateFrame(f Frame) error {
	if f.Kind < KindRequest || f.Kind > KindWatch || len(f.Method) > 64 || len(f.Grant) > 64 || len(f.Status) > 64 {
		return execenv.ErrInvalid
	}
	if !utf8.ValidString(f.Method) || !utf8.ValidString(f.Grant) || !utf8.ValidString(f.Status) {
		return execenv.ErrInvalid
	}
	if len(f.Extra) > MaxFrameBytes {
		return execenv.ErrTooLarge
	}
	if f.Kind == KindPty && len(f.Extra) > MaxPtyBytes {
		return execenv.ErrTooLarge
	}
	return nil
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[n:]
	}
	return nil
}
