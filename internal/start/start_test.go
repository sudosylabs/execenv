package start

import (
	"net"
	"testing"
)

// A probe is answered and the guest waits on; a start is returned once,
// to be acknowledged after it is applied.
func TestProbeThenStart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan Message, 1)
	go func() {
		m, conn, err := Wait(ln)
		if err != nil {
			return
		}
		got <- m
		_ = Acknowledge(conn)
		_ = conn.Close()
	}()
	probe, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := Probe(probe); err != nil {
		t.Fatalf("Probe() = %v", err)
	}
	_ = probe.Close()
	m, err := New("epoch-1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := Send(conn, m); err != nil {
		t.Fatalf("Send() = %v", err)
	}
	if r := <-got; r.Epoch != "epoch-1" || r.MachineID != m.MachineID || string(r.Entropy) != string(m.Entropy) {
		t.Fatalf("received %+v", r)
	}
}

// Each start is fresh: no two share entropy or a machine id.
func TestNewIsFresh(t *testing.T) {
	a, _ := New("e")
	b, _ := New("e")
	if string(a.Entropy) == string(b.Entropy) || a.MachineID == b.MachineID || len(a.Entropy) != EntropyBytes {
		t.Fatal("two starts share their randomness")
	}
}
