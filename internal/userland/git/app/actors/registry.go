// Package actors implements the Git smart-HTTP actor-handle boundary: an
// in-memory, per-tenant, process-local registry that turns one authenticated
// actor credential into an opaque request-bound handle and resolves that handle
// back to the credential only while the issuing request is live.
//
// The registry is the production implementation of git.ActorHandleIssuer and
// git.ActorSource. The smart-HTTP handler places the handle, never the raw
// credential, in KODAMA_ACTOR_REF; the native hook resolves that handle through
// the same registry. A credential is never persisted, logged or exported.
package actors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

const (
	// DefaultMaxHandles bounds the number of live handles when the
	// configuration leaves the bound unset.
	DefaultMaxHandles = 1024
	// DefaultTTL bounds one handle when the configuration leaves the TTL unset.
	DefaultTTL = 5 * time.Minute
	// MaxTTL is the hard upper bound on a configured handle TTL. A handle never
	// outlives the issuing request, and it never outlives this bound either.
	MaxTTL = 15 * time.Minute

	// handleBytes is the raw random handle material: 128 bits, the minimum
	// unguessable size. The encoded handle is 36 bytes, well inside the declared
	// smart-HTTP handle bound and inside the validActorHandle alphabet.
	handleBytes = 16
	// handlePrefix keeps a handle recognisable without revealing its source.
	handlePrefix = "kah_"
	// maxMintAttempts bounds collision retries against a hostile random source.
	maxMintAttempts = 8
)

// Stable refusal sentinels. Every exported error also carries a fault
// class so a caller can map it to the public error vocabulary without matching
// message text.
var (
	// ErrCredential reports an empty or over-bound actor credential.
	ErrCredential = errors.New("actor credential is empty or exceeds the bound")
	// ErrCapacity reports that the bounded live-handle set is full.
	ErrCapacity = errors.New("actor handle capacity is exhausted")
	// ErrClosed reports use of a closed registry.
	ErrClosed = errors.New("actor handle registry is closed")
	// ErrUnauthenticated is the single uniform handle refusal. Unknown,
	// expired, revoked, malformed, forged and cross-registry handles all return
	// exactly this class and message so the resolver is not an oracle.
	ErrUnauthenticated = errors.New("actor handle is not valid")
)

// Config configures one tenant-bound, process-local registry. Every field has a
// safe default except TenantID, which is required.
type Config struct {
	// TenantID is the tenant this registry instance serves. A handle issued by
	// another registry is never resolved here.
	TenantID id.TenantID
	// Clock is the time boundary used for the hard TTL. A nil value uses the
	// system clock.
	Clock git.Clock
	// MaxHandles bounds live handles. Zero or negative selects
	// DefaultMaxHandles.
	MaxHandles int
	// TTL bounds one handle. Zero or negative selects DefaultTTL; a value
	// above MaxTTL is refused.
	TTL time.Duration
	// Rand is the handle randomness source. A nil value uses crypto/rand.
	Rand io.Reader
}

// Registry is one tenant-bound in-memory actor-handle registry. It implements
// both Git actor ports and is safe for concurrent use.
type Registry struct {
	mu         sync.Mutex
	tenant     id.TenantID
	clock      git.Clock
	maxHandles int
	ttl        time.Duration
	random     io.Reader
	entries    map[string]*entry
	callbacks  sync.WaitGroup
	closed     bool
}

// entry is one live handle. secret is the only copy of the credential held by
// the process beyond the caller's own value.
type entry struct {
	tenant  id.TenantID
	handle  string
	secret  []byte
	expires time.Time
	ctx     context.Context
	stop    func() bool
}

var (
	_ git.ActorHandleIssuer = (*Registry)(nil)
	_ git.ActorSource       = (*Registry)(nil)
)

// NewRegistry validates the configuration and returns an empty registry. It
// starts no goroutine; a handle registers a context callback only when issued.
func NewRegistry(config Config) (*Registry, error) {
	tenant, err := id.ParseTenantID(string(config.TenantID))
	if err != nil {
		return nil, fmt.Errorf("actor registry tenant: %w", err)
	}
	maxHandles := config.MaxHandles
	if maxHandles <= 0 {
		maxHandles = DefaultMaxHandles
	}
	ttl := config.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		return nil, fmt.Errorf("actor handle TTL %s exceeds the maximum %s", ttl, MaxTTL)
	}
	clock := config.Clock
	if clock == nil {
		clock = systemClock{}
	}
	random := config.Rand
	if random == nil {
		random = rand.Reader
	}
	return &Registry{
		tenant:     tenant,
		clock:      clock,
		maxHandles: maxHandles,
		ttl:        ttl,
		random:     random,
		entries:    make(map[string]*entry),
	}, nil
}

// IssueActor mints one opaque handle for a live request context. The credential
// is copied into process memory only; the returned handle is random, unguessable
// and independent of the credential. The handle is revoked when ctx ends, when
// the hard TTL elapses, or when the registry is closed.
func (registry *Registry) IssueActor(ctx context.Context, credential string) (string, error) {
	if registry == nil {
		return "", closedError()
	}
	if ctx == nil {
		return "", fault.New(fault.InvalidInput, "actor handle issuance context is missing")
	}
	if err := ctx.Err(); err != nil {
		return "", issuanceContextError(err)
	}
	if credential == "" || len(credential) > api.MaxCredentialBytes {
		return "", credentialError()
	}

	secret := []byte(credential)
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		zero(secret)
		return "", closedError()
	}
	registry.pruneLocked()
	if len(registry.entries) >= registry.maxHandles {
		registry.mu.Unlock()
		zero(secret)
		return "", capacityError()
	}
	handle, err := registry.mintLocked()
	if err != nil {
		registry.mu.Unlock()
		zero(secret)
		return "", err
	}
	entry := &entry{
		tenant:  registry.tenant,
		handle:  handle,
		secret:  secret,
		expires: registry.clock.Now().Add(registry.ttl),
		ctx:     ctx,
	}
	registry.entries[handle] = entry
	// Register the request-bound expiry while the registry lock is held so the
	// callback cannot observe a half-built entry. The callback is tracked so
	// Close joins every callback that actually started.
	registry.callbacks.Add(1)
	entry.stop = context.AfterFunc(ctx, func() {
		defer registry.callbacks.Done()
		registry.expire(entry)
	})
	registry.mu.Unlock()

	// context.AfterFunc is asynchronous. Re-check the context so an already
	// ended request never yields a usable handle.
	if err := ctx.Err(); err != nil {
		registry.expire(entry)
		return "", issuanceContextError(err)
	}
	return handle, nil
}

// ResolveActor returns the forwarded credential for a live handle. A handle is
// not single-use: one hook invocation may resolve it several times (update,
// pre-receive, post-receive) while the issuing request is live. Every refusal
// is the same unauthenticated fault, so an attacker cannot distinguish an
// unknown handle from an expired, revoked, forged or cross-registry one.
func (registry *Registry) ResolveActor(ctx context.Context, handle string) (string, error) {
	if registry == nil {
		return "", unauthenticatedError()
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return "", unauthenticatedError()
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closed {
		return "", unauthenticatedError()
	}
	entry, ok := registry.entries[handle]
	if !ok {
		return "", unauthenticatedError()
	}
	if entry.tenant != registry.tenant ||
		entry.ctx == nil ||
		entry.ctx.Err() != nil ||
		!registry.clock.Now().Before(entry.expires) {
		registry.revokeLocked(entry)
		return "", unauthenticatedError()
	}
	return string(entry.secret), nil
}

// Revoke invalidates one handle immediately. It is safe to call for an unknown
// handle and safe to call concurrently with IssueActor and ResolveActor.
func (registry *Registry) Revoke(handle string) {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry, ok := registry.entries[handle]
	if !ok {
		return
	}
	registry.revokeLocked(entry)
}

// Close revokes every live handle and refuses later issuance. The registry
// starts no long-lived goroutine; the context callbacks it registered are
// stopped here. Close is idempotent.
func (registry *Registry) Close() error {
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		return nil
	}
	registry.closed = true
	for _, entry := range registry.entries {
		registry.revokeLocked(entry)
	}
	registry.mu.Unlock()
	// A callback already running may be waiting for the lock. Join it after
	// releasing the lock; callbacks prevented by stop are accounted for in
	// revokeLocked.
	registry.callbacks.Wait()
	return nil
}

// String renders safe registry metadata. It never renders a credential.
func (registry *Registry) String() string {
	if registry == nil {
		return "actors.Registry(nil)"
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return fmt.Sprintf("actors.Registry{tenant:%q handles:%d}", registry.tenant, len(registry.entries))
}

// GoString keeps %#v credential-free.
func (registry *Registry) GoString() string { return registry.String() }

// mintLocked returns one unused random handle. The caller holds registry.mu.
func (registry *Registry) mintLocked() (string, error) {
	for range maxMintAttempts {
		raw := make([]byte, handleBytes)
		if _, err := io.ReadFull(registry.random, raw); err != nil {
			return "", randomnessError(err)
		}
		handle := handlePrefix + hex.EncodeToString(raw)
		if _, exists := registry.entries[handle]; !exists {
			return handle, nil
		}
	}
	return "", randomnessError(errors.New("actor handle collision"))
}

// pruneLocked revokes entries whose request ended or whose TTL elapsed. The
// caller holds registry.mu.
func (registry *Registry) pruneLocked() {
	now := registry.clock.Now()
	for _, entry := range registry.entries {
		if entry.ctx == nil || entry.ctx.Err() != nil || !now.Before(entry.expires) {
			registry.revokeLocked(entry)
		}
	}
}

// expire is the context callback. It revokes only the exact entry it was
// registered for, so a stale callback can never invalidate a later handle.
func (registry *Registry) expire(target *entry) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, ok := registry.entries[target.handle]
	if !ok || current != target {
		return
	}
	registry.revokeLocked(target)
}

// revokeLocked removes one entry, overwrites its credential bytes where Go
// permits and stops the pending context callback. The caller holds registry.mu.
func (registry *Registry) revokeLocked(entry *entry) {
	delete(registry.entries, entry.handle)
	zero(entry.secret)
	entry.secret = nil
	entry.ctx = nil
	if entry.stop != nil {
		if entry.stop() {
			// The callback was prevented, so it will never call Done.
			registry.callbacks.Done()
		}
		entry.stop = nil
	}
}

// zero overwrites a byte slice in place. It is best effort: a string copy the
// caller already made cannot be reached by Go.
func zero(secret []byte) {
	for index := range secret {
		secret[index] = 0
	}
}

func credentialError() error {
	return fault.Wrap(fault.InvalidInput, ErrCredential.Error(), ErrCredential)
}

func capacityError() error {
	return fault.Wrap(fault.InternalFailure, ErrCapacity.Error(), ErrCapacity)
}

func closedError() error {
	return fault.Wrap(fault.InternalFailure, ErrClosed.Error(), ErrClosed)
}

func unauthenticatedError() error {
	return fault.Wrap(fault.Unauthenticated, ErrUnauthenticated.Error(), ErrUnauthenticated)
}

func issuanceContextError(cause error) error {
	return fault.Wrap(fault.DeadlineExpired, "actor handle issuance context ended", cause)
}

func randomnessError(cause error) error {
	return fault.Wrap(fault.InternalFailure, "actor handle randomness is unavailable", cause)
}

// systemClock is the production time boundary.
type systemClock struct{}

// Now returns the current time.
func (systemClock) Now() time.Time { return time.Now() }
