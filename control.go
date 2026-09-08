package execenv

import "context"

// ProjectionFence identifies one authorized effect on an opaque grant/epoch.
// Revision is caller-issued and monotonic within the grant. It carries no
// interpretation of the caller's authorization or lifecycle model.
type ProjectionFence struct {
	ExecutionGrantID ID     `json:"execution_grant_id"`
	EnvironmentEpoch string `json:"environment_epoch"`
	FenceRevision    uint64 `json:"fence_revision"`
}

// ControlState is the requested host execution state.
type ControlState string

const (
	ControlRunning ControlState = "running"
	ControlFrozen  ControlState = "frozen"
	ControlRevoked ControlState = "revoked"
)

// ControlRequest binds an exact state to a monotonically increasing fence.
type ControlRequest struct {
	Fence ProjectionFence `json:"fence"`
	State ControlState    `json:"state"`
}

// ControlReceipt records the highest accepted intent. Confirmed is true only
// after the actual effect succeeds. Status may return revision zero before the
// first fenced intent. An unsuccessful intent still fences every older request.
type ControlReceipt struct {
	Fence     ProjectionFence `json:"fence"`
	State     ControlState    `json:"state"`
	Confirmed bool            `json:"confirmed"`
}

// ControlledEnv supports acknowledged, ordered execution control. An epoch is
// never reused after reconstruction. Implementations that lose fence state must
// make the old epoch permanently unusable. Revoke is terminal for that epoch.
// Once Control is used, unfenced Freeze/Thaw are refused with ErrConflict.
type ControlledEnv interface {
	Env
	Epoch() string
	Control(context.Context, ControlRequest) (ControlReceipt, error)
	ControlStatus(context.Context) (ControlReceipt, error)
}

// ValidateControl rejects malformed requests, including integers that cannot
// be represented exactly by interoperating JSON clients.
func ValidateControl(request ControlRequest) error {
	if validateName(string(request.Fence.ExecutionGrantID)) != nil || validateName(request.Fence.EnvironmentEpoch) != nil || request.Fence.FenceRevision == 0 || request.Fence.FenceRevision > 1<<53-1 {
		return ErrInvalid
	}
	switch request.State {
	case ControlRunning, ControlFrozen, ControlRevoked:
		return nil
	default:
		return ErrInvalid
	}
}
