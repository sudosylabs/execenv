package content

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// Verify reads r as body b. It hashes what it returns, and where r ends it
// returns io.EOF only when exactly b was read; otherwise it returns stale.
// More bytes than b holds are stale as soon as they arrive.
func Verify(r io.ReadCloser, b Blob, stale error) io.ReadCloser {
	return &verified{r: r, want: b, stale: stale, sum: sha256.New()}
}

type verified struct {
	r     io.ReadCloser
	want  Blob
	stale error
	sum   hash.Hash
	n     int64
}

func (v *verified) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.n += int64(n)
	if v.n > v.want.Size {
		return 0, v.stale
	}
	v.sum.Write(p[:n])
	if err == io.EOF && (v.n != v.want.Size || hex.EncodeToString(v.sum.Sum(nil)) != v.want.Digest) {
		return n, v.stale
	}
	return n, err
}

func (v *verified) Close() error { return v.r.Close() }
