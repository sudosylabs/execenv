package execenv

import (
	"context"
	"encoding/json"
	"io"
)

type ObservationOperation string

const (
	ObservationCreateFile      ObservationOperation = "create_file"
	ObservationCreateDirectory ObservationOperation = "create_directory"
	ObservationWriteFile       ObservationOperation = "write_file"
	ObservationMove            ObservationOperation = "move"
	ObservationDelete          ObservationOperation = "delete"
)

// ExecutionObservation retains the original projected baseline and captured
// content of one guest effect. It never means reading the current path later.
type ExecutionObservation struct {
	Fence                      ProjectionFence      `json:"fence"`
	HostSequence               uint64               `json:"host_sequence"`
	BasedOnWorkspaceCursor     uint64               `json:"based_on_workspace_cursor"`
	Operation                  ObservationOperation `json:"operation"`
	NodeIdentity               string               `json:"node_identity"`
	Kind                       ProjectionKind       `json:"kind"`
	Path                       string               `json:"path"`
	DestinationPath            string               `json:"destination_path,omitempty"`
	ExpectedContentVersion     Version              `json:"expected_content_version,omitempty"`
	Recursive                  *bool                `json:"recursive,omitempty"`
	Content                    *ContentReference    `json:"content,omitempty"`
	OriginProjectionMutationID string               `json:"origin_projection_mutation_id,omitempty"`
}

type ObservationPage struct {
	HostCursor             uint64                 `json:"host_cursor"`
	AcknowledgedHostCursor uint64                 `json:"acknowledged_host_cursor"`
	Observations           []ExecutionObservation `json:"observations"`
}

// SemanticEnv is a separate, explicit capability from journal projection.
// Acknowledgement is allowed only after the caller durably records each outcome.
// It releases retained event bytes and never implies that those bytes were
// committed to the caller's authority or advances the projection baseline.
type SemanticEnv interface {
	ProjectionEnv
	ConfirmObservationProjection(context.Context, ObservationProjection) error
	Observe(context.Context, ProjectionFence, uint64, int) (ObservationPage, error)
	OpenObservationContent(context.Context, ProjectionFence, ContentReference) (io.ReadCloser, error)
	AcknowledgeObservations(context.Context, ProjectionFence, uint64) error
}

func ValidateExecutionObservation(event ExecutionObservation) error {
	if ValidateProjectionFence(event.Fence) != nil || event.HostSequence == 0 || event.HostSequence > MaxProjectionCursor || event.BasedOnWorkspaceCursor > MaxProjectionCursor || validateName(event.NodeIdentity) != nil || ValidatePath(event.Path) != nil {
		return ErrInvalid
	}
	if event.Kind != ProjectionFile && event.Kind != ProjectionDirectory {
		return ErrInvalid
	}
	if event.ExpectedContentVersion != "" && validateName(string(event.ExpectedContentVersion)) != nil {
		return ErrInvalid
	}
	if event.OriginProjectionMutationID != "" && validateName(event.OriginProjectionMutationID) != nil {
		return ErrInvalid
	}
	if event.Content != nil && ValidateContentReference(*event.Content) != nil {
		return ErrInvalid
	}
	switch event.Operation {
	case ObservationCreateFile:
		if event.Kind != ProjectionFile || event.Content == nil || event.ExpectedContentVersion != "" || event.DestinationPath != "" || event.Recursive != nil {
			return ErrInvalid
		}
	case ObservationCreateDirectory:
		if event.Kind != ProjectionDirectory || event.Content != nil || event.ExpectedContentVersion != "" || event.DestinationPath != "" || event.Recursive != nil {
			return ErrInvalid
		}
	case ObservationWriteFile:
		if event.Kind != ProjectionFile || event.Content == nil || event.DestinationPath != "" || event.Recursive != nil {
			return ErrInvalid
		}
	case ObservationMove:
		if ValidatePath(event.DestinationPath) != nil || event.Path == event.DestinationPath || event.Content != nil || event.Recursive != nil {
			return ErrInvalid
		}
		if event.Kind == ProjectionDirectory && event.ExpectedContentVersion != "" {
			return ErrInvalid
		}
	case ObservationDelete:
		if event.Content != nil || event.DestinationPath != "" {
			return ErrInvalid
		}
		if event.Kind == ProjectionFile && event.Recursive != nil || event.Kind == ProjectionDirectory && event.ExpectedContentVersion != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	// An unacknowledged guest-created identity has no projected content version.
	// Its later writes/moves/deletes retain that fact; the caller reconciles the
	// preceding create outcome instead of substituting its newest live version.
	raw, err := json.Marshal(event)
	if err != nil {
		return ErrInvalid
	}
	if len(raw) > MaxProjectionMetadataBytes {
		return ErrTooLarge
	}
	return nil
}

// ObservationProjection binds a durably accepted guest outcome to the exact
// resulting caller journal entry. Register it before acknowledging the retained
// observation. It does not itself advance the projection or alter guest files.
// Apply later validates that exact effect and suppresses only its known echo.
type ObservationProjection struct {
	Fence        ProjectionFence    `json:"fence"`
	HostSequence uint64             `json:"host_sequence"`
	Mutation     ProjectionMutation `json:"mutation"`
}

func ValidateObservationProjection(binding ObservationProjection) error {
	if ValidateProjectionFence(binding.Fence) != nil || binding.HostSequence == 0 || binding.HostSequence > MaxProjectionCursor || binding.Mutation.JournalCursor == 0 || binding.Mutation.JournalCursor > MaxProjectionCursor {
		return ErrInvalid
	}
	return validateProjectionMutation(binding.Mutation)
}
