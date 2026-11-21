package portfixture

import (
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
	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			switch mode {
			case "echo":
				data, err := io.ReadAll(conn)
				if err == nil {
					_, _ = conn.Write(data)
				}
			case "send-fin":
				_, _ = io.WriteString(conn, "ready")
				_ = conn.CloseWrite()
				_, _ = io.Copy(io.Discard, conn)
			case "hold":
				_, _ = io.Copy(io.Discard, conn)
			}
		}()
	}
}
