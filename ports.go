package execenv

import "io"

// PortConn carries bytes to and from a port in one grant. A reader and a
// writer may use it concurrently. Close interrupts both directions.
type PortConn interface {
	io.ReadWriteCloser
	// CloseWrite sends EOF to the listener while leaving the read side open.
	CloseWrite() error
	// Port is the port that was requested.
	Port() int
}
