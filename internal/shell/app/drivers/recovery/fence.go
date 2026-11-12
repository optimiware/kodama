package recovery

import (
	"context"
	"errors"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
)

// FenceConfig binds one fence driver to its libpod client and its explicit
// inventory. No actor outside Inventory is inspected or stopped.
type FenceConfig struct {
	Libpod    runtimepodman.Libpod
	Inventory []Actor
}

// FenceDriver stops exactly the inventoried live actors. It never discovers
// host containers and never retries a stop.
type FenceDriver struct {
	libpod    runtimepodman.Libpod
	inventory []Actor
}

var _ interface {
	Fence(context.Context) error
} = (*FenceDriver)(nil)

// NewFenceDriver validates one fence configuration. It contacts no daemon.
func NewFenceDriver(config FenceConfig) (*FenceDriver, error) {
	if config.Libpod == nil {
		return nil, fault.New(fault.InvalidInput, "fence driver requires a libpod client")
	}
	// Fencing nothing is never evidence that old actors are stopped, so an
	// empty inventory is a configuration defect rather than a no-op success.
	if len(config.Inventory) == 0 {
		return nil, fault.New(fault.InvalidInput, "fence driver requires at least one inventoried actor")
	}
	inventory := make([]Actor, 0, len(config.Inventory))
	for _, actor := range config.Inventory {
		if strings.TrimSpace(actor.Name) == "" {
			return nil, fault.New(fault.InvalidInput, "fence inventory actor name is empty")
		}
		switch actor.Kind {
		case ActorController, ActorService, ActorConsumer:
		default:
			return nil, fault.New(fault.InvalidInput, "fence inventory actor kind is not declared")
		}
		inventory = append(inventory, actor)
	}
	return &FenceDriver{libpod: config.Libpod, inventory: inventory}, nil
}

// Fence inspects only the inventoried actors and stops a live actor exactly
// once. A missing actor or an ambiguous/unconfirmed stop is refused; a created
// or already-exited actor is accepted without a stop.
func (d *FenceDriver) Fence(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, actor := range d.inventory {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := d.libpod.InspectContainer(ctx, actor.Name)
		if err != nil {
			if errors.Is(err, runtimepodman.ErrNotFound) {
				return fault.New(fault.NotFound, "fence inventory actor is missing")
			}
			return fault.Wrap(fault.DependencyUnavailable, "fence actor state is unreachable", err)
		}
		if !actorNeedsStop(state) {
			if err := verifyActorStopped(state); err != nil {
				return err
			}
			continue
		}
		if err := d.stopActor(ctx, actor.Name); err != nil {
			return err
		}
	}
	return nil
}

// actorNeedsStop reports whether the actor may still effect changes. A created
// container that is not starting has never run; an exited container has
// already terminated.
func actorNeedsStop(state runtimepodman.ContainerState) bool {
	if state.StartPending {
		return true
	}
	switch state.Status {
	case runtimepodman.ContainerStatusRunning:
		return true
	case runtimepodman.ContainerStatusCreated:
		return false
	case runtimepodman.ContainerStatusExited:
		return false
	default:
		return false
	}
}

// verifyActorStopped requires affirmative whole-container stop evidence for an
// actor that is not running.
func verifyActorStopped(state runtimepodman.ContainerState) error {
	switch state.Status {
	case runtimepodman.ContainerStatusCreated:
		if state.StartPending || state.Started {
			return fault.New(fault.UncertainExternalEffect, "fence actor start outcome is ambiguous")
		}
		return nil
	case runtimepodman.ContainerStatusExited:
		if !state.WholeStopped || state.StartPending {
			return fault.New(fault.UncertainExternalEffect, "fence actor has no whole-container stop evidence")
		}
		return nil
	default:
		return fault.New(fault.UncertainExternalEffect, "fence actor state is unrecognized")
	}
}

// stopActor requests termination once and requires affirmative whole-container
// stop evidence. A stop acknowledgment is not evidence, and a lost response is
// never retried.
func (d *FenceDriver) stopActor(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.libpod.StopContainer(ctx, name); err != nil {
		return fault.Wrap(fault.UncertainExternalEffect, "fence stop outcome is unknown", err)
	}
	state, err := d.libpod.InspectContainer(ctx, name)
	if err != nil {
		if errors.Is(err, runtimepodman.ErrNotFound) {
			return fault.New(fault.UncertainExternalEffect, "fence actor disappeared without stop evidence")
		}
		return fault.Wrap(fault.UncertainExternalEffect, "fence stop evidence is unreachable", err)
	}
	if !state.WholeStopped || state.StartPending {
		return fault.New(fault.UncertainExternalEffect, "fence actor has no whole-container stop evidence")
	}
	return nil
}
