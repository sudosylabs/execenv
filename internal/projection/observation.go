package projection

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/sudosylabs/execenv"
)

type observationProof struct {
	digest   [32]byte
	event    execenv.ExecutionObservation
	mutation execenv.ProjectionMutation
}

func normalizedMutation(m execenv.ProjectionMutation) [32]byte {
	if m.Content != nil {
		content := *m.Content
		content.TransferID = ""
		m.Content = &content
	}
	raw, _ := json.Marshal(m)
	return sha256.Sum256(raw)
}

// RegisterObservation records only a caller-confirmed durable journal outcome.
// The original event remains the proof of identity, topology and captured bytes;
// registering cannot mutate files, advance cursors or consume content uploads.
func (s *State) RegisterObservation(binding execenv.ObservationProjection, lookup func(uint64) (execenv.ExecutionObservation, error)) error {
	if err := execenv.ValidateObservationProjection(binding); err != nil {
		return err
	}
	digest := normalizedMutation(binding.Mutation)
	if old, ok := s.observationBindings[binding.HostSequence]; ok {
		if old.digest != digest {
			return execenv.ErrConflict
		}
		return nil
	}
	if len(s.observationBindings) >= retainedReceipts {
		return execenv.ErrTooLarge
	}
	if _, exists := s.proofs[binding.Mutation.JournalCursor]; exists {
		return execenv.ErrConflict
	}
	if binding.Mutation.JournalCursor <= s.cursor {
		return execenv.ErrConflict
	}
	event, err := lookup(binding.HostSequence)
	if err != nil {
		return err
	}
	if event.Fence.ExecutionGrantID != binding.Fence.ExecutionGrantID || event.Fence.EnvironmentEpoch != binding.Fence.EnvironmentEpoch {
		return execenv.ErrConflict
	}
	m := binding.Mutation
	if event.Kind != m.Kind || event.Path != m.Path || event.DestinationPath != m.DestinationPath {
		return execenv.ErrConflict
	}
	operation := execenv.ProjectionOperation("")
	switch event.Operation {
	case execenv.ObservationCreateFile, execenv.ObservationCreateDirectory:
		operation = execenv.ProjectionCreate
	case execenv.ObservationWriteFile:
		operation = execenv.ProjectionWrite
	case execenv.ObservationMove:
		operation = execenv.ProjectionMove
	case execenv.ObservationDelete:
		operation = execenv.ProjectionDelete
	}
	if m.Operation != operation && !(operation == execenv.ProjectionMove && m.Operation == execenv.ProjectionRename) {
		return execenv.ErrConflict
	}
	if (event.Recursive != nil) != (m.Recursive != nil) || event.Recursive != nil && *event.Recursive != *m.Recursive {
		return execenv.ErrConflict
	}
	if (event.Content != nil) != (m.Content != nil) || event.Content != nil && (event.Content.Size != m.Content.Size || event.Content.Digest != m.Content.Digest) {
		return execenv.ErrConflict
	}
	// Expected versions are the durable caller outcome's precondition. Successive
	// accepted edits of one guest identity may advance it beyond the original
	// capture version; Apply still checks the actual consecutive journal baseline.
	proof := observationProof{digest: digest, event: event, mutation: m}
	s.proofs[m.JournalCursor] = proof
	s.observationBindings[binding.HostSequence] = proof
	return nil
}
