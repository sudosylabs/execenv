// Package ring keeps the newest bytes of one stream by absolute offset.
package ring

// Ring holds at most its size of the newest bytes written. Its buffer grows
// as bytes arrive, so a quiet stream costs little; once full it wraps.
type Ring struct {
	size  int
	total uint64
	// Until full, buf holds the bytes from base to total in order. Once
	// full, buf is size bytes long and offset o lives at o mod size.
	buf  []byte
	base uint64
	full bool
}

// New returns an empty ring of size bytes whose next byte is at start.
func New(size int, start uint64) *Ring {
	return &Ring{size: max(size, 1), total: start, base: start}
}

// Total is the offset just past the newest byte.
func (r *Ring) Total() uint64 { return r.total }

// First is the offset of the oldest byte still held.
func (r *Ring) First() uint64 {
	if r.full {
		return r.total - uint64(r.size)
	}
	return r.base
}

// Write appends p, dropping the oldest bytes beyond the size.
func (r *Ring) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	if !r.full {
		r.buf = append(r.buf, p...)
		r.total += uint64(len(p))
		if len(r.buf) < r.size {
			return
		}
		// Lay the newest size bytes onto their modular positions.
		newest := r.buf[len(r.buf)-r.size:]
		first := r.total - uint64(r.size)
		laid := make([]byte, r.size)
		for i, b := range newest {
			laid[(first+uint64(i))%uint64(r.size)] = b
		}
		r.buf, r.full = laid, true
		return
	}
	size := uint64(r.size)
	if uint64(len(p)) > size {
		r.total += uint64(len(p)) - size
		p = p[uint64(len(p))-size:]
	}
	for len(p) > 0 {
		at := r.total % size
		n := copy(r.buf[at:], p)
		p = p[n:]
		r.total += uint64(n)
	}
}

// Read copies up to max bytes from offset. Outside what is held it
// returns nothing.
func (r *Ring) Read(offset uint64, max int) []byte {
	if offset < r.First() || offset >= r.total || max <= 0 {
		return nil
	}
	if !r.full {
		i := int(offset - r.base)
		n := min(len(r.buf)-i, max)
		return append([]byte(nil), r.buf[i:i+n]...)
	}
	size := uint64(r.size)
	at := offset % size
	end := min(size, at+(r.total-offset))
	n := min(int(end-at), max)
	return append([]byte(nil), r.buf[at:at+uint64(n)]...)
}
