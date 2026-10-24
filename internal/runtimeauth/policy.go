package runtimeauth

import (
	"fmt"
	"time"
)

// Hard ceilings for the configurable lifetimes. A configuration beyond them is
// rejected at construction; there is no default and no environment lookup in
// this module (T-027 supplies installation configuration).
const (
	maxAdmissionTTL = 24 * time.Hour
	maxTokenTTL     = time.Hour
	maxSessionTTL   = 8 * 24 * time.Hour // above the 7 day step timeout limit of the runtime contract
	minTTL          = time.Second
)

// Policy holds the lifetime decisions the runtime contract leaves open: how
// long an unused admission credential stays redeemable, how long a runtime
// token lives, and the maximum lifetime of a session fixed at first admission.
type Policy struct {
	// AdmissionTTL bounds the time between registration and first admission.
	AdmissionTTL time.Duration
	// TokenTTL is the lifetime of each issued or renewed token, clamped to the
	// session maximum.
	TokenTTL time.Duration
	// MaxSession bounds all renewals and replays from the first admission. It
	// is persisted and never extended.
	MaxSession time.Duration
}

// Validate rejects zero, negative and unreasonable values.
func (p Policy) Validate() error {
	check := func(name string, v, hi time.Duration) error {
		if v < minTTL || v > hi {
			return fmt.Errorf("runtimeauth: %s must be between %s and %s, got %s", name, minTTL, hi, v)
		}
		return nil
	}
	if err := check("admission TTL", p.AdmissionTTL, maxAdmissionTTL); err != nil {
		return err
	}
	if err := check("token TTL", p.TokenTTL, maxTokenTTL); err != nil {
		return err
	}
	if err := check("maximum session lifetime", p.MaxSession, maxSessionTTL); err != nil {
		return err
	}
	if p.TokenTTL > p.MaxSession {
		return fmt.Errorf("runtimeauth: token TTL %s exceeds the maximum session lifetime %s", p.TokenTTL, p.MaxSession)
	}
	return nil
}

// WrapperPolicy decides whether an entrypoint version may run an attempt.
// pinnedWrapper is the wrapper version the profile pins, or "" for none.
type WrapperPolicy interface {
	Check(clientVersion, pinnedWrapper string) error
}

// ExactWrapperPolicy is the conservative first policy: the client version must
// be explicitly supported and, for a pinned profile, equal the pinned wrapper
// version. A nil or empty Supported list supports nothing.
type ExactWrapperPolicy struct{ Supported []string }

// Check implements WrapperPolicy.
func (p ExactWrapperPolicy) Check(clientVersion, pinnedWrapper string) error {
	supported := false
	for _, v := range p.Supported {
		if v == clientVersion {
			supported = true
			break
		}
	}
	if !supported {
		return fail(ErrProfileUnsupported, "client version is not supported")
	}
	if pinnedWrapper != "" && pinnedWrapper != clientVersion {
		return fail(ErrProfileUnsupported, "client version does not match the profile's wrapper")
	}
	return nil
}
