// Package mux is the shared frame, session, and status map used by the
// agent hop and the remote hop. It is not a hop of its own.
//
// Agent and remote keep their own method tables. Closing a PTY is hangup,
// not connection teardown. Status is a sentinel name, never octets.
// Grant callers never import this package.
package mux

import (
	"bytes"
	"encoding/gob"
	"net"
	"sync"
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
	Status string
	Extra  []byte
}

// Session is one accepted or dialed connection. One reader owns Decode.
// All writes take a lock so PTY and RPC frames stay sequenced.
type Session struct {
	conn    net.Conn
	enc     *gob.Encoder
	dec     *gob.Decoder
	writeMu sync.Mutex
}

// NewSession takes an already-dialed connection.
func NewSession(conn net.Conn) *Session {
	return &Session{
		conn: conn,
		enc:  gob.NewEncoder(conn),
		dec:  gob.NewDecoder(conn),
	}
}

func (s *Session) Send(f Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.enc.Encode(f)
}

func (s *Session) Recv() (Frame, error) {
	var f Frame
	err := s.dec.Decode(&f)
	return f, err
}

func (s *Session) Close() error {
	return s.conn.Close()
}

// EncodeExtra gob-encodes a method payload. Nil is an empty Extra.
func EncodeExtra(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeExtra gob-decodes Extra into dest.
func DecodeExtra(data []byte, dest any) error {
	if len(data) == 0 {
		return nil
	}
	return gob.NewDecoder(bytes.NewReader(data)).Decode(dest)
}
