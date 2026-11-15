package ring

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

func readAll(r *Ring) []byte {
	var out []byte
	for at := r.First(); at < r.Total(); {
		chunk := r.Read(at, 3)
		if len(chunk) == 0 {
			break
		}
		out = append(out, chunk...)
		at += uint64(len(chunk))
	}
	return out
}

// Any sequence of writes holds exactly the newest size bytes at their
// absolute offsets, from any starting offset.
func TestRingHoldsTheNewestBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 200 {
		size := 1 + rng.IntN(40)
		start := uint64(rng.IntN(1000))
		r := New(size, start)
		var all []byte
		for range rng.IntN(20) {
			p := make([]byte, rng.IntN(60))
			for i := range p {
				p[i] = byte(rng.IntN(256))
			}
			r.Write(p)
			all = append(all, p...)
			want := all[max(0, len(all)-size):]
			if r.Total() != start+uint64(len(all)) || r.First() != r.Total()-uint64(len(want)) {
				t.Fatalf("trial %d: first=%d total=%d, held %d of %d", trial, r.First(), r.Total(), len(want), len(all))
			}
			if got := readAll(r); !bytes.Equal(got, want) {
				t.Fatalf("trial %d: held %q, want %q", trial, got, want)
			}
		}
	}
}

func TestReadOutsideIsEmpty(t *testing.T) {
	r := New(4, 10)
	r.Write([]byte("abcdef"))
	if r.Read(11, 8) != nil || r.Read(16, 8) != nil {
		t.Fatal("read outside the held bytes returned data")
	}
	if got := string(r.Read(12, 8)); got != "cdef" {
		t.Fatalf("Read(12) = %q", got)
	}
}
