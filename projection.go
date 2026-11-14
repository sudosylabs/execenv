package execenv

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	// MaxProjectionInitializationBytes accommodates a complete bounded tree,
	// including JSON escaping of every path. Incremental effects remain smaller.
	MaxProjectionInitializationBytes        = 4 << 20
	MaxProjectionMutations                  = 128
	MaxProjectionMetadataBytes              = 256 << 10
	MaxProjectionCursor              uint64 = 1<<53 - 1

	// MaxIgnoreFileBytes bounds one read of the ignore file. A larger file
	// counts as not parsing, so the fallback applies.
	MaxIgnoreFileBytes = 64 << 10
	// MaxIgnoreFallbackPatterns and MaxIgnoreFallbackBytes bound
	// IgnoreRules.Fallback.
	MaxIgnoreFallbackPatterns = 256
	MaxIgnoreFallbackBytes    = 16 << 10
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

// ProjectionInitialization installs the caller's tree once per epoch. Ignore
// names the paths whose guest changes are left out of observations; nil
// observes everything. Initialization happens once per epoch and an exact
// retry is a replay, so the rules last for the epoch.
type ProjectionInitialization struct {
	Fence           ProjectionFence   `json:"fence"`
	MutationID      string            `json:"mutation_id"`
	WorkspaceCursor uint64            `json:"workspace_cursor"`
	Entries         []ProjectionEntry `json:"entries"`
	Ignore          *IgnoreRules      `json:"ignore,omitempty"`
}

// IgnoreRules names a workspace-relative file of gitignore patterns. The
// caller chooses the file; the host hardcodes none.
//
// Patterns follow gitignore: '#' comments, blank lines, '!' negation, a
// trailing '/' for directories only, a leading or inner '/' anchors to the
// workspace root, '**' spans any depth, and '*', '?' and '[...]' match
// within one segment. The last matching pattern wins, and a path under an
// ignored directory cannot be re-included.
//
// Patterns suppress only paths the projection does not hold. A path the
// caller projected, or one the stream already reported, stays observed
// while it exists, including after a move to an ignored name. The ignore
// file and its parent directories are always observed, and the guest
// re-reads the file whenever its bytes change, whoever changed them. An
// ignored directory that is not held is never walked: its contents cost
// nothing and never count toward observation limits. A held path moved
// into such a directory therefore leaves the stream as a delete. Ignored
// paths stay present in the environment; nothing is deleted for being
// ignored. A caller projection onto an ignored path replaces the ignored
// file there, as a checkout does, and a projected directory adopts an
// ignored directory already at its path.
//
// Fallback applies while File is missing, is not a regular file, exceeds
// MaxIgnoreFileBytes, or does not parse (NUL bytes, invalid UTF-8, an empty
// segment such as "a//b", an unterminated '[' class or a trailing '\'). It
// holds at most
// MaxIgnoreFallbackPatterns single-line patterns totalling at most
// MaxIgnoreFallbackBytes, and every one must parse.
type IgnoreRules struct {
	File     string   `json:"file"`
	Fallback []string `json:"fallback,omitempty"`
}

// ValidateIgnoreRules checks the file path, the fallback bounds, and that
// every fallback pattern parses.
func ValidateIgnoreRules(rules IgnoreRules) error {
	if ValidatePath(rules.File) != nil {
		return ErrInvalid
	}
	if len(rules.Fallback) > MaxIgnoreFallbackPatterns {
		return ErrTooLarge
	}
	total := 0
	for _, p := range rules.Fallback {
		total += len(p)
	}
	if total > MaxIgnoreFallbackBytes {
		return ErrTooLarge
	}
	for _, p := range rules.Fallback {
		if !validIgnorePattern(p) {
			return ErrInvalid
		}
	}
	return nil
}

// validIgnorePattern is the syntax every adapter's matcher accepts for one
// single-line pattern. The guest-side matcher is tested to agree with it.
func validIgnorePattern(line string) bool {
	if strings.ContainsAny(line, "\x00\r\n") || !utf8.ValidString(line) {
		return false
	}
	escaped := func(s string, index int) bool {
		n := 0
		for i := index - 1; i >= 0 && s[i] == '\\'; i-- {
			n++
		}
		return n%2 == 1
	}
	for len(line) > 0 && line[len(line)-1] == ' ' && !escaped(line, len(line)-1) {
		line = line[:len(line)-1]
	}
	if line == "" || line[0] == '#' {
		return true
	}
	line = strings.TrimPrefix(line, "!")
	if strings.HasSuffix(line, "/") && !escaped(line, len(line)-1) {
		line = strings.TrimRight(line, "/")
	}
	if strings.Contains(line, "/") {
		line = strings.TrimPrefix(line, "/")
	}
	if line == "" {
		return true
	}
	for _, segment := range strings.Split(line, "/") {
		if segment == "" {
			return false
		}
		for i := 0; i < len(segment); i++ {
			switch segment[i] {
			case '\\':
				if i+1 >= len(segment) {
					return false
				}
				i++
			case '[':
				j := i + 1
				if j < len(segment) && (segment[j] == '!' || segment[j] == '^') {
					j++
				}
				closed := false
				for first := true; j < len(segment); j, first = j+1, false {
					if segment[j] == '\\' {
						if j+1 >= len(segment) {
							return false
						}
						j++
					} else if segment[j] == ']' && !first {
						closed = true
						break
					}
				}
				if !closed {
					return false
				}
				i = j
			}
		}
	}
	return true
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
	if request.Ignore != nil {
		if err := ValidateIgnoreRules(*request.Ignore); err != nil {
			return err
		}
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
