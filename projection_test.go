package execenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func validProjectionApply() ProjectionApply {
	return ProjectionApply{Fence: ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}, MutationID: "batch", ThroughWorkspaceCursor: 1, Mutations: []ProjectionMutation{{JournalCursor: 1, EntryID: "entry", Operation: ProjectionCreate, Kind: ProjectionFile, Path: "file", ResultingContentVersion: "v1", Content: &ContentReference{Size: 3, Digest: strings.Repeat("a", 64), TransferID: "transfer"}}}}
}
func TestProjectionMutationFieldApplicability(t *testing.T) {
	falseValue := false
	for _, test := range []struct {
		name   string
		change func(*ProjectionMutation)
	}{
		{"create precondition", func(m *ProjectionMutation) { m.ExpectedContentVersion = "v0" }},
		{"create recursion", func(m *ProjectionMutation) { m.Recursive = &falseValue }},
		{"directory content", func(m *ProjectionMutation) { m.Kind = ProjectionDirectory }},
		{"write without version", func(m *ProjectionMutation) { m.Operation = ProjectionWrite }},
		{"file move without expected version", func(m *ProjectionMutation) {
			m.Operation = ProjectionMove
			m.DestinationPath = "other"
			m.Content = nil
			m.ResultingContentVersion = ""
		}},
		{"delete with content", func(m *ProjectionMutation) { m.Operation = ProjectionDelete; m.ExpectedContentVersion = "v1" }},
		{"unconfined path", func(m *ProjectionMutation) { m.Path = "../file" }},
		{"arbitrary content URL", func(m *ProjectionMutation) { m.Content.TransferID = "https://host/body" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validProjectionApply()
			test.change(&request.Mutations[0])
			if err := ValidateProjectionApply(request); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid fields accepted: %v", err)
			}
		})
	}
	if err := ValidateProjectionApply(validProjectionApply()); err != nil {
		t.Fatal(err)
	}
}
func TestProjectionConsecutiveAndBoundedMetadata(t *testing.T) {
	for _, count := range []int{0, 1, 128, 129} {
		request := validProjectionApply()
		request.Mutations = nil
		request.ThroughWorkspaceCursor = uint64(count)
		for index := range count {
			request.Mutations = append(request.Mutations, ProjectionMutation{JournalCursor: uint64(index + 1), EntryID: fmt.Sprintf("dir-%d", index), Operation: ProjectionCreate, Kind: ProjectionDirectory, Path: fmt.Sprintf("dir-%d", index)})
		}
		err := ValidateProjectionApply(request)
		if (count == 1 || count == 128) != (err == nil) {
			t.Fatalf("count %d: %v", count, err)
		}
	}
	request := validProjectionApply()
	request.FromWorkspaceCursor = 1
	request.ThroughWorkspaceCursor = 2
	if err := ValidateProjectionApply(request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nonconsecutive cursor accepted: %v", err)
	}
	request = validProjectionApply()
	request.ExpectedHostCursor = MaxProjectionCursor + 1
	if err := ValidateProjectionApply(request); !errors.Is(err, ErrInvalid) {
		t.Fatal("unsafe integer accepted")
	}
	request = validProjectionApply()
	request.Mutations = nil
	request.ThroughWorkspaceCursor = 128
	long := strings.Repeat("a", 250) + "/" + strings.Repeat("b", 250) + "/" + strings.Repeat("c", 250) + "/" + strings.Repeat("d", 250)
	for index := range 128 {
		request.Mutations = append(request.Mutations, ProjectionMutation{JournalCursor: uint64(index + 1), EntryID: fmt.Sprintf("node-%d", index), Operation: ProjectionMove, Kind: ProjectionDirectory, Path: long, DestinationPath: "x" + long})
	}
	if err := ValidateProjectionApply(request); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized metadata accepted: %v", err)
	}
}

func TestProjectionInitializationAcceptsMaximumEscapedTree(t *testing.T) {
	request := ProjectionInitialization{Fence: ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}, MutationID: "initial"}
	path := ""
	for i := 0; i < 3; i++ {
		if path != "" {
			path += "/"
		}
		path += strings.Repeat("&", MaxSegmentBytes)
		request.Entries = append(request.Entries, ProjectionEntry{EntryID: fmt.Sprintf("parent-%d", i), Kind: ProjectionDirectory, Path: path})
	}
	for i := len(request.Entries); i < MaxTreeEntries; i++ {
		request.Entries = append(request.Entries, ProjectionEntry{EntryID: fmt.Sprintf("entry-%d", i), Kind: ProjectionDirectory, Path: path + "/" + strings.Repeat("&", MaxSegmentBytes-4) + fmt.Sprintf("%04d", i)})
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 1<<20 {
		t.Fatalf("fixture metadata too small: %d", len(raw))
	}
	if err := ValidateProjectionInitialization(request); err != nil {
		t.Fatalf("valid maximum tree rejected (%d metadata bytes): %v", len(raw), err)
	}
	request.Entries = append(request.Entries, ProjectionEntry{EntryID: "excess", Kind: ProjectionDirectory, Path: "excess"})
	if err := ValidateProjectionInitialization(request); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("tree entry limit changed: %v", err)
	}
}
