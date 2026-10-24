package agents

import (
	"fmt"
	"slices"
)

// Expansion is the pure mapping of one profile version onto the existing
// container runtime: the image, entrypoint and canonical payload arguments of
// the step, the profile's wall-time ceiling and its run-wide allowance claims.
// Nothing here reads a clock, a database or the network.
type Expansion struct {
	Image   string
	Command []string
	Args    []string
	// WallSeconds is the profile's wall-time limit. The execution module takes
	// the smaller of it and the step's own timeout.
	WallSeconds int64
	// MaxRequests and MaxTokens are the profile's run-wide allowance claims
	// (zero when not claimed).
	MaxRequests int64
	MaxTokens   int64
	Config      WrapperConfig
}

// Expand maps a registered version onto a container command.
func Expand(p ProfileVersion) (Expansion, error) {
	cfg, err := NewWrapperConfig(p)
	if err != nil {
		return Expansion{}, err
	}
	args, err := cfg.Args()
	if err != nil {
		return Expansion{}, err
	}
	d := cfg.Content
	e := Expansion{Image: d.Image, Command: slices.Clone(d.Wrapper.Command), Args: args, WallSeconds: d.Limits.WallSeconds, Config: cfg}
	if d.Limits.MaxRequests != nil {
		e.MaxRequests = *d.Limits.MaxRequests
	}
	if d.Limits.MaxTokens != nil {
		e.MaxTokens = *d.Limits.MaxTokens
	}
	// A frozen step must decode back to exactly what was pinned.
	back, present, err := DecodeArgs(e.Args)
	if err != nil || !present || back.Profile != cfg.Profile {
		return Expansion{}, fmt.Errorf("%w: the expanded payload does not round-trip", ErrInvalid)
	}
	return e, nil
}
