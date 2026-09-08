package remote

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

func TestSemanticDownloadScopeBoundsAndDisconnect(t *testing.T) {
	pool := newSemanticDownloads()
	owner, other := &serverConn{}, &serverConn{}
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	ref := execenv.ContentReference{Size: 3}
	id, err := pool.reserve(owner, fence, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.complete(owner, id, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	args := semanticArgs{Fence: fence, DownloadID: id, Length: 2}
	if _, err := pool.chunk(other, args); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("foreign connection read")
	}
	wrong := args
	wrong.Fence.EnvironmentEpoch = "other"
	if _, err := pool.chunk(owner, wrong); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("foreign epoch read")
	}
	body, err := pool.chunk(owner, args)
	if err != nil || !bytes.Equal(body, []byte("ab")) {
		t.Fatalf("chunk: %q %v", body, err)
	}
	body[0] = 'X'
	again, err := pool.chunk(owner, args)
	if err != nil || !bytes.Equal(again, []byte("ab")) {
		t.Fatal("retry changed immutable content")
	}
	for i := 1; i < activeProjectionUploads; i++ {
		if _, err := pool.reserve(owner, fence, ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.reserve(other, fence, ref); !errors.Is(err, execenv.ErrCapacity) {
		t.Fatal("global download quota bypass")
	}
	pool.entries[id].expires = time.Now().Add(-time.Second)
	if _, err := pool.chunk(owner, args); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("expired download resumed")
	}
	pending, err := pool.reserve(other, fence, ref)
	if err != nil {
		t.Fatal(err)
	}
	pool.close(owner)
	if len(pool.entries) != 1 {
		t.Fatal("disconnect cleared another owner")
	}
	pool.close(other)
	if err := pool.complete(other, pending, []byte("abc")); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("late open completion resurrected disconnected download")
	}
}
