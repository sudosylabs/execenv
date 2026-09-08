package remote

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

func TestProjectionUploadScopeAndRetries(t *testing.T) {
	pool := newProjectionUploads()
	owner, stranger := &serverConn{}, &serverConn{}
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	id, err := pool.begin(owner, fence)
	if err != nil {
		t.Fatal(err)
	}
	args := projectionUploadArgs{UploadID: id, Fence: fence, Data: []byte("abc")}
	if err := pool.append(stranger, args); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("foreign connection: %v", err)
	}
	wrong := args
	wrong.Fence.EnvironmentEpoch = "next"
	if err := pool.append(owner, wrong); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("foreign epoch: %v", err)
	}
	wrong = args
	wrong.Offset = 1
	if err := pool.append(owner, wrong); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("offset gap: %v", err)
	}
	if err := pool.append(owner, args); err != nil {
		t.Fatal(err)
	}
	if err := pool.append(owner, args); err != nil {
		t.Fatalf("exact chunk retry: %v", err)
	}
	wrong = args
	wrong.Data = []byte("abd")
	if err := pool.append(owner, wrong); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("changed chunk retry: %v", err)
	}
	args.Data[0] = 'z'
	finish := projectionUploadArgs{UploadID: id, Fence: fence, Offset: 3}
	body, err := pool.finish(owner, finish)
	if err != nil || !bytes.Equal(body, []byte("abc")) {
		t.Fatalf("immutable chunk: %q %v", body, err)
	}
	if _, err := pool.finish(owner, finish); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("concurrent finish: %v", err)
	}
	if err := pool.abort(owner, finish); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("abort during commit: %v", err)
	}
	pool.close(owner)
	if len(pool.entries) != 1 {
		t.Fatal("in-flight commit released capacity early")
	}
	pool.remove(owner, id)
	if len(pool.entries) != 0 {
		t.Fatal("finished upload retained")
	}
}

func TestProjectionUploadBoundsAndCleanup(t *testing.T) {
	pool := newProjectionUploads()
	owner, stranger := &serverConn{}, &serverConn{}
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	var first string
	for i := 0; i < activeProjectionUploads; i++ {
		id, err := pool.begin(owner, fence)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id
		}
	}
	if _, err := pool.begin(stranger, fence); !errors.Is(err, execenv.ErrCapacity) {
		t.Fatalf("listener quota: %v", err)
	}
	args := projectionUploadArgs{UploadID: first, Fence: fence, Data: make([]byte, projectionChunkBytes+1)}
	if err := pool.append(owner, args); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("chunk bound: %v", err)
	}
	args.Data = []byte("x")
	args.Offset = execenv.MaxFileBytes
	if err := pool.append(owner, args); !errors.Is(err, execenv.ErrTooLarge) {
		t.Fatalf("file bound: %v", err)
	}
	if err := pool.abort(stranger, args); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("foreign abort: %v", err)
	}
	pool.entries[first].expires = time.Now().Add(-time.Second)
	args.Offset = 0
	if err := pool.append(owner, args); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("expired transfer: %v", err)
	}
	if _, err := pool.begin(stranger, fence); err != nil {
		t.Fatalf("expired slot not reclaimed: %v", err)
	}
	pool.close(owner)
	if len(pool.entries) != 1 {
		t.Fatal("connection cleanup changed another owner")
	}
	pool.close(stranger)
	if len(pool.entries) != 0 {
		t.Fatal("connection cleanup leaked transfers")
	}
}
