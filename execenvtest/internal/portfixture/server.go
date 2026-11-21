package portfixture

import (
	"bytes"
	"io"
	"net"
	"net/http"
)

// Serve runs the same listener behavior in the local and isolated fixtures.
func Serve(listener *net.TCPListener, mode string) {
	if mode == "http" {
		_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello")
		}))
		return
	}
	var accepted int
	received := make(chan []byte, 1)
	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			return
		}
		if mode == "accept-one" {
			_ = listener.Close()
			defer conn.Close()
			_, _ = io.WriteString(conn, "accepted")
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		accepted++
		first := accepted == 1
		go func() {
			defer conn.Close()
			switch mode {
			case "echo":
				data, err := io.ReadAll(conn)
				if err == nil {
					_, _ = conn.Write(data)
				}
			case "send-fin":
				if first {
					_, _ = io.WriteString(conn, "ready")
					_ = conn.CloseWrite()
					data, _ := io.ReadAll(conn)
					received <- data
				} else {
					_, _ = conn.Write(<-received)
				}
			case "reset":
				_, _ = io.WriteString(conn, "prefix")
				var ack [1]byte
				_, _ = io.ReadFull(conn, ack[:])
				_ = conn.SetLinger(0)
			case "flood":
				data := bytes.Repeat([]byte{'x'}, 32<<10)
				for {
					if _, err := conn.Write(data); err != nil {
						break
					}
				}
			case "hold":
				_, _ = io.Copy(io.Discard, conn)
			}
		}()
	}
}
