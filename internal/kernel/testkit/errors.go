package testkit

import "errors"

var (
	// ErrNotPrepared reports an effect against a workload identity the fake
	// runtime has no prepared resource for. Callers must prepare before they
	// start, stop, remove, read logs or refresh credentials.
	ErrNotPrepared = errors.New("testkit: workload is not prepared")

	// ErrReplay reports a second start of the same stable workload identity.
	// A lost start response is not permission to launch again; the caller must
	// observe the persisted identity instead.
	ErrReplay = errors.New("testkit: workload was already started; observe the stable identity instead of replaying")

	// ErrStopUnconfirmed reports cleanup of a started workload before
	// affirmative termination evidence. A stop acknowledgment alone never
	// satisfies this guard; a prepared but never started workload needs no
	// stop because it has no live process.
	ErrStopUnconfirmed = errors.New("testkit: workload stop is not confirmed")

	// ErrOwnershipHeld reports a workspace release while a workload that is not
	// affirmatively removed still owns the workspace without a confirmed stop.
	ErrOwnershipHeld = errors.New("testkit: workspace ownership is still held")

	// ErrWorkspaceLost reports a retain of a workspace that disappeared.
	// Scratch state is never reconstructed.
	ErrWorkspaceLost = errors.New("testkit: workspace was lost")
)
