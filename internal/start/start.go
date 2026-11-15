// Package start is the message that begins a grant's guest. Guest init
// waits for it before it mounts the workspace or starts the helper, so a
// machine restored from a saved template becomes one grant's: fresh
// entropy, the host's clock, its own machine id and the grant's epoch.
//
// The host dials Port on the guest link. "PING" is answered "READY" and
// the guest waits on, which is how a template build knows the guest is
// idle and may be saved. "START" and the message, as JSON on one line, is
// answered "OK" once the guest has applied it.
package start

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Port is the guest-side port init listens on until it has started.
const Port = 5001

// EntropyBytes is how much fresh randomness each start carries.
const EntropyBytes = 64

// maxLine bounds one line of the exchange.
const maxLine = 4096

// Message is one guest's start.
type Message struct {
	// Entropy reseeds the guest's random number generator: a restored
	// machine otherwise carries the template's state.
	Entropy []byte `json:"entropy"`
	// Time sets the guest clock, which a restored machine has stopped.
	Time time.Time `json:"time"`
	// MachineID is the guest's own, as /etc/machine-id holds one.
	MachineID string `json:"machine_id"`
	// Epoch is the grant occupancy the guest now serves.
	Epoch string `json:"epoch"`
}

// New is a fresh start for epoch.
func New(epoch string) (Message, error) {
	entropy := make([]byte, EntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return Message{}, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Message{}, err
	}
	return Message{Entropy: entropy, Time: time.Now().UTC(), MachineID: hex.EncodeToString(id), Epoch: epoch}, nil
}

// ErrProtocol is an exchange that did not follow this package's lines.
var ErrProtocol = errors.New("start: unexpected reply")

func readLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for b.Len() <= maxLine {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if c == '\n' {
			return b.String(), nil
		}
		b.WriteByte(c)
	}
	return "", ErrProtocol
}

// Probe asks a guest whether it is waiting to start. It reads the reply a
// byte at a time, so nothing after it is consumed.
func Probe(conn net.Conn) error {
	if _, err := io.WriteString(conn, "PING\n"); err != nil {
		return err
	}
	line, err := readLine(bufio.NewReaderSize(oneByte{conn}, 16))
	if err != nil {
		return err
	}
	if line != "READY" {
		return ErrProtocol
	}
	return nil
}

// Send starts the guest with m and waits until it has applied it.
func Send(conn net.Conn, m Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(conn, "START %s\n", body); err != nil {
		return err
	}
	line, err := readLine(bufio.NewReaderSize(oneByte{conn}, 16))
	if err != nil {
		return err
	}
	if line != "OK" {
		return ErrProtocol
	}
	return nil
}

// Wait accepts host connections on ln, answering probes, until one starts
// the guest. It returns the message and the connection to answer once the
// message is applied.
func Wait(ln net.Listener) (Message, net.Conn, error) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return Message{}, nil, err
		}
		r := bufio.NewReader(conn)
		for {
			line, err := readLine(r)
			if err != nil {
				_ = conn.Close()
				break
			}
			if line == "PING" {
				if _, err := io.WriteString(conn, "READY\n"); err != nil {
					_ = conn.Close()
					break
				}
				continue
			}
			body, ok := strings.CutPrefix(line, "START ")
			var m Message
			if !ok || json.Unmarshal([]byte(body), &m) != nil || len(m.Entropy) < EntropyBytes || m.MachineID == "" {
				_ = conn.Close()
				break
			}
			return m, conn, nil
		}
	}
}

// Acknowledge tells the host the start was applied.
func Acknowledge(conn net.Conn) error {
	_, err := io.WriteString(conn, "OK\n")
	return err
}

// oneByte reads a byte at a time, so a reply line never takes bytes meant
// for whoever reads the connection next.
type oneByte struct{ r io.Reader }

func (o oneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}
