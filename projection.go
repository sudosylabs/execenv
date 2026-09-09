package execenv

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

const (
	// MaxProjectionInitializationBytes accommodates a complete bounded tree,
	// including JSON escaping of every path. Incremental effects remain smaller.
	MaxProjectionInitializationBytes        = 4 << 20
	MaxProjectionMutations                  = 128
	MaxProjectionMetadataBytes              = 256 << 10
	MaxProjectionCursor              uint64 = 1<<53 - 1
)

// ContentReference names immutable bytes retained for one grant/epoch and
// purpose. It is never a filesystem path, URL, or caller storage credential.
type ContentReference struct {
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	TransferID string `json:"transfer_id"`
}

type ProjectionKind string

const (
	ProjectionFile      ProjectionKind = "file"
	ProjectionDirectory ProjectionKind = "directory"
)

type ProjectionOperation string

const (
	ProjectionCreate ProjectionOperation = "create"
	ProjectionWrite  ProjectionOperation = "write"
	ProjectionRename ProjectionOperation = "rename"
	ProjectionMove   ProjectionOperation = "move"
	ProjectionDelete ProjectionOperation = "delete"
)

// ProjectionMutation is one consecutive mutation in the caller's durable
// journal. Versions and entry identities are opaque to the host.
type ProjectionMutation struct {
	JournalCursor           uint64              `json:"journal_cursor"`
	EntryID                 string              `json:"entry_id"`
	Operation               ProjectionOperation `json:"operation"`
	Kind                    ProjectionKind      `json:"kind"`
	Path                    string              `json:"path"`
	DestinationPath         string              `json:"destination_path,omitempty"`
	ExpectedContentVersion  Version             `json:"expected_content_version,omitempty"`
	ResultingContentVersion Version             `json:"resulting_content_version,omitempty"`
	Recursive               *bool               `json:"recursive,omitempty"`
	Content                 *ContentReference   `json:"content,omitempty"`
}

type ProjectionApply struct {
	Fence                  ProjectionFence      `json:"fence"`
	MutationID             string               `json:"mutation_id"`
	FromWorkspaceCursor    uint64               `json:"from_workspace_cursor"`
	ThroughWorkspaceCursor uint64               `json:"through_workspace_cursor"`
	ExpectedHostCursor     uint64               `json:"expected_host_cursor"`
	Mutations              []ProjectionMutation `json:"mutations"`
}

type ProjectionReceipt struct {
	Fence                  ProjectionFence `json:"fence"`
	MutationID             string          `json:"mutation_id"`
	AppliedWorkspaceCursor uint64          `json:"applied_workspace_cursor"`
	HostCursor             uint64          `json:"host_cursor"`
}

// ProjectionEntry initializes one caller-owned entry. Parent directories must
// precede their descendants; files always name immutable uploaded content.
type ProjectionEntry struct {
	EntryID        string            `json:"entry_id"`
	Kind           ProjectionKind    `json:"kind"`
	Path           string            `json:"path"`
	ContentVersion Version           `json:"content_version,omitempty"`
	Content        *ContentReference `json:"content,omitempty"`
}

type ProjectionInitialization struct {
	Fence           ProjectionFence   `json:"fence"`
	MutationID      string            `json:"mutation_id"`
	WorkspaceCursor uint64            `json:"workspace_cursor"`
	Entries         []ProjectionEntry `json:"entries"`
}

// ProjectionEnv supports the fenced projection protocol. Capability discovery
// is explicit; older or unsupported backends must return ErrUnavailable. It is
// independent of legacy Apply/Watch and does not silently upgrade their guarantees.
type ProjectionEnv interface {
	ControlledEnv
	UploadProjectionContent(context.Context, ProjectionFence, io.Reader) (ContentReference, error)
	InitializeProjection(context.Context, ProjectionInitialization) (ProjectionReceipt, error)
	ApplyProjection(context.Context, ProjectionApply) (ProjectionReceipt, error)
}

func ValidateProjectionFence(fence ProjectionFence) error {
	return ValidateControl(ControlRequest{Fence: fence, State: ControlRunning})
}

func ValidateContentReference(content ContentReference) error {
	if content.Size < 0 || content.Size > MaxFileBytes || validateName(content.TransferID) != nil || len(content.Digest) != 64 || strings.ToLower(content.Digest) != content.Digest {
		return ErrInvalid
	}
	if _, err := hex.DecodeString(content.Digest); err != nil {
		return ErrInvalid
	}
	return nil
}

func ValidateProjectionApply(request ProjectionApply) error {
	if err := ValidateProjectionFence(request.Fence); err != nil {
		return err
	}
	if validateName(request.MutationID) != nil || request.FromWorkspaceCursor > MaxProjectionCursor || request.ThroughWorkspaceCursor > MaxProjectionCursor || request.ExpectedHostCursor > MaxProjectionCursor {
		return ErrInvalid
	}
	if len(request.Mutations) == 0 {
		return ErrInvalid
	}
	if len(request.Mutations) > MaxProjectionMutations {
		return ErrTooLarge
	}
	if request.ThroughWorkspaceCursor <= request.FromWorkspaceCursor || request.ThroughWorkspaceCursor-request.FromWorkspaceCursor != uint64(len(request.Mutations)) {
		return ErrInvalid
	}
	var total int64
	for index, mutation := range request.Mutations {
		if mutation.JournalCursor != request.FromWorkspaceCursor+uint64(index)+1 {
			return ErrInvalid
		}
		if err := validateProjectionMutation(mutation); err != nil {
			return err
		}
		if mutation.Content != nil {
			total += mutation.Content.Size
		}
	}
	if total > MaxTreeBytes {
		return ErrTooLarge
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return ErrInvalid
	}
	if len(raw) > MaxProjectionMetadataBytes {
		return ErrTooLarge
	}
	return nil
}

func validateProjectionMutation(m ProjectionMutation) error {
	if validateName(m.EntryID) != nil || ValidatePath(m.Path) != nil {
		return ErrInvalid
	}
	if m.Kind != ProjectionFile && m.Kind != ProjectionDirectory {
		return ErrInvalid
	}
	for _, version := range []Version{m.ExpectedContentVersion, m.ResultingContentVersion} {
		if version != "" && validateName(string(version)) != nil {
			return ErrInvalid
		}
	}
	if m.Content != nil {
		if err := ValidateContentReference(*m.Content); err != nil {
			return err
		}
	}
	switch m.Operation {
	case ProjectionCreate:
		if m.ExpectedContentVersion != "" || m.DestinationPath != "" || m.Recursive != nil {
			return ErrInvalid
		}
		if m.Kind == ProjectionFile {
			if m.Content == nil || m.ResultingContentVersion == "" {
				return ErrInvalid
			}
		} else if m.Content != nil || m.ResultingContentVersion != "" {
			return ErrInvalid
		}
	case ProjectionWrite:
		if m.Kind != ProjectionFile || m.ExpectedContentVersion == "" || m.ResultingContentVersion == "" || m.Content == nil || m.DestinationPath != "" || m.Recursive != nil {
			return ErrInvalid
		}
	case ProjectionMove, ProjectionRename:
		if ValidatePath(m.DestinationPath) != nil || m.DestinationPath == m.Path || m.Content != nil || m.ResultingContentVersion != "" || m.Recursive != nil {
			return ErrInvalid
		}
		if (m.Kind == ProjectionFile) != (m.ExpectedContentVersion != "") {
			return ErrInvalid
		}
	case ProjectionDelete:
		if m.Content != nil || m.ResultingContentVersion != "" || m.DestinationPath != "" {
			return ErrInvalid
		}
		if m.Kind == ProjectionFile {
			if m.ExpectedContentVersion == "" || m.Recursive != nil {
				return ErrInvalid
			}
		} else if m.ExpectedContentVersion != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func ValidateProjectionInitialization(request ProjectionInitialization) error {
	if ValidateProjectionFence(request.Fence) != nil || validateName(request.MutationID) != nil || request.WorkspaceCursor > MaxProjectionCursor {
		return ErrInvalid
	}
	if len(request.Entries) > MaxTreeEntries {
		return ErrTooLarge
	}
	var total int64
	for _, entry := range request.Entries {
		m := ProjectionMutation{EntryID: entry.EntryID, Path: entry.Path, Kind: entry.Kind, Operation: ProjectionCreate, ResultingContentVersion: entry.ContentVersion, Content: entry.Content}
		if err := validateProjectionMutation(m); err != nil {
			return err
		}
		if entry.Content != nil {
			total += entry.Content.Size
		}
	}
	if total > MaxTreeBytes {
		return ErrTooLarge
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return ErrInvalid
	}
	if len(raw) > MaxProjectionInitializationBytes {
		return ErrTooLarge
	}
	return nil
}
