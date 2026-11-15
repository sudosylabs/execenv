package unixsock

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A socket deeper than an address holds is still bound and dialed.
func TestLongPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("long socket paths are Linux only")
	}
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "long.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen(%d bytes) = %v", len(path), err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			_, _ = conn.Write([]byte("ok"))
			_ = conn.Close()
		}
	}()
	conn, err := Dial(t.Context(), path)
	if err != nil {
		t.Fatalf("Dial(%d bytes) = %v", len(path), err)
	}
	defer conn.Close()
	buf := make([]byte, 2)
	if _, err := conn.Read(buf); err != nil || string(buf) != "ok" {
		t.Fatalf("Read() = %q, %v", buf, err)
	}
}
