package remote

import (
	"encoding/gob"
	"net"
	"sync"
)

// session is one accepted or dialed connection. A single reader goroutine
// owns dec. All writes take writeMu so gob stays sequenced while PTY and
// RPC frames share the socket.
type session struct {
	conn    net.Conn
	enc     *gob.Encoder
	dec     *gob.Decoder
	writeMu sync.Mutex
}

func newSession(conn net.Conn) *session {
	return &session{
		conn: conn,
		enc:  gob.NewEncoder(conn),
		dec:  gob.NewDecoder(conn),
	}
}

func (s *session) send(f frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.enc.Encode(f)
}

func (s *session) recv() (frame, error) {
	var f frame
	err := s.dec.Decode(&f)
	return f, err
}

func (s *session) close() error {
	return s.conn.Close()
}
