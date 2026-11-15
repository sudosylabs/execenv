package execenv

import (
	"encoding/json"
	"time"
)

// MaxObserveWait bounds ObserveRequest.Wait.
const MaxObserveWait = time.Minute

// ObserveRequest asks for the observations after a host sequence.
type ObserveRequest struct {
	Fence ProjectionFence
	// After is the host sequence the page starts after. A position before
	// the acknowledged prefix is ErrLagged; one beyond the observations
	// produced so far is ErrInvalid.
	After uint64
	// Limit is the most observations returned, 1 to MaxProjectionMutations.
	Limit int
	// Wait, when set, holds the call until the guest has produced an
	// observation after After or Wait has passed, whichever comes first,
	// and then returns what there is: an empty page once the wait passes
	// is not an error. Zero returns at once. A freeze, revoke or loss
	// during the wait ends it with that error, and so does the caller's
	// context. At most MaxObserveWait.
	Wait time.Duration
}

// ValidateObserveRequest refuses a request no host can serve.
func ValidateObserveRequest(request ObserveRequest) error {
	if ValidateProjectionFence(request.Fence) != nil || request.After > MaxProjectionCursor || request.Limit < 1 || request.Limit > MaxProjectionMutations || request.Wait < 0 || request.Wait > MaxObserveWait {
		return ErrInvalid
	}
	return nil
}

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
//
// ProcessTag is the tag of the process, started with Processes.Start, that
// the program which made the change descends from: the started process
// itself or anything it ran. A path several programs changed before one
// capture names the last of them. It is empty when the host cannot say:
// the change was made by nothing a caller started, the host could not
// trace it, or the host cannot attribute changes at all.
//
// DiscardedBy is the journal cursor of an Overwrite mutation that replaced
// this change before the caller confirmed it, or zero. A discarded change
// is gone from the environment: the caller acknowledges it without
// recording it, it cannot be confirmed, and its content reads as ErrStale.
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
	ProcessTag                 string               `json:"process_tag,omitempty"`
	DiscardedBy                uint64               `json:"discarded_by,omitempty"`
}

type ObservationPage struct {
	HostCursor             uint64                 `json:"host_cursor"`
	AcknowledgedHostCursor uint64                 `json:"acknowledged_host_cursor"`
	Observations           []ExecutionObservation `json:"observations"`
}

// Acknowledge observations only after the caller durably records each
// outcome. Acknowledgement releases retained event bytes; it never implies
// that those bytes were committed to the caller's authority, and it does not
// advance the projection baseline.

func ValidateExecutionObservation(event ExecutionObservation) error {
	if ValidateProjectionFence(event.Fence) != nil || event.HostSequence == 0 || event.HostSequence > MaxProjectionCursor || event.BasedOnWorkspaceCursor > MaxProjectionCursor || event.DiscardedBy > MaxProjectionCursor || validateName(event.NodeIdentity) != nil || ValidatePath(event.Path) != nil {
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
	if event.ProcessTag != "" && (len(event.ProcessTag) > MaxTagBytes || validateName(event.ProcessTag) != nil) {
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
// ApplyProjection later validates that exact effect and suppresses only its
// known echo.
type ObservationProjection struct {
	Fence        ProjectionFence    `json:"fence"`
	HostSequence uint64             `json:"host_sequence"`
	Mutation     ProjectionMutation `json:"mutation"`
}

func ValidateObservationProjection(binding ObservationProjection) error {
	if ValidateProjectionFence(binding.Fence) != nil || binding.HostSequence == 0 || binding.HostSequence > MaxProjectionCursor || binding.Mutation.JournalCursor == 0 || binding.Mutation.JournalCursor > MaxProjectionCursor || binding.Mutation.Overwrite {
		return ErrInvalid
	}
	return validateProjectionMutation(binding.Mutation)
}
