package auth

import (
	"context"
	"errors"

	"github.com/zalando/go-keyring"
)

// DefaultKeyringService is the keyring namespace used by the CLI.
const DefaultKeyringService = "kodama-cli"

// Keyring is the minimal OS keyring surface (github.com/zalando/go-keyring).
type Keyring interface {
	Set(service, user, secret string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type systemKeyring struct{}

func (systemKeyring) Set(service, user, secret string) error {
	return keyring.Set(service, user, secret)
}
func (systemKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}
func (systemKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

// PromptProber is implemented by a Keyring that can tell, without prompting,
// whether Get, Set and Delete would run without user interaction. A
// non-interactive invocation uses a keyring only when its probe returns nil;
// a keyring without a probe is treated as possibly prompting and refused.
type PromptProber interface {
	// PromptFree returns nil when operations cannot prompt right now,
	// ErrInteractionRequired when they could (for example a locked
	// collection), and another error when that cannot be determined.
	PromptFree(ctx context.Context) error
}

// SystemKeyring returns the real OS keyring.
func SystemKeyring() Keyring { return systemKeyring{} }

// KeyringStore is the default credential store. An unavailable keyring is an
// error: there is no silent fallback to a file or to memory.
type KeyringStore struct {
	kr      Keyring
	service string
}

var _ CredentialStore = (*KeyringStore)(nil)

// NewKeyringStore uses kr (nil selects the OS keyring) under service (empty
// selects DefaultKeyringService).
func NewKeyringStore(kr Keyring, service string) *KeyringStore {
	if kr == nil {
		kr = systemKeyring{}
	}
	if service == "" {
		service = DefaultKeyringService
	}
	return &KeyringStore{kr: kr, service: service}
}

// isCtxFailure reports a timeout or interruption produced by bounded.
func isCtxFailure(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func account(key string) string { return "credential:" + key }

// bounded runs a backend operation without letting it outlive ctx. The
// backends take no context and may block on a prompt, so the call runs on its
// own goroutine; when ctx ends first the caller gets the timeout or
// interruption immediately and the abandoned call finishes (or dies with the
// process) in the background. An abandoned Set may still complete, so a login
// that timed out while saving may leave a usable credential; it never reports
// success.
func bounded[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, ctxFailure(err)
	}
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctxFailure(ctx.Err())
	}
}

// guard refuses an operation that might prompt when ctx forbids prompting. It
// runs before any Get, Set or Delete reaches the backend.
func (s *KeyringStore) guard(ctx context.Context) error {
	if !NonInteractive(ctx) {
		return nil
	}
	p, ok := s.kr.(PromptProber)
	if !ok {
		return storeErr(ErrInteractionRequired, nil,
			"the OS keyring cannot be used without possibly prompting; unlock it and run interactively, or use --credential-store file")
	}
	_, err := bounded(ctx, func() (struct{}, error) { return struct{}{}, p.PromptFree(ctx) })
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrInteractionRequired):
		return storeErr(ErrInteractionRequired, err,
			"the OS keyring may prompt (it is locked, or cannot be shown to be unlocked); unlock it and run interactively, or use --credential-store file")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return storeErr(ErrStoreUnavailable, err, "the OS keyring could not be checked without prompting; use --credential-store file")
	}
}

func (s *KeyringStore) Load(ctx context.Context, key string) (Credential, error) {
	if err := validateKey(key); err != nil {
		return Credential{}, err
	}
	if err := s.guard(ctx); err != nil {
		return Credential{}, err
	}
	raw, err := bounded(ctx, func() (string, error) { return s.kr.Get(s.service, account(key)) })
	if isCtxFailure(err) {
		return Credential{}, err
	}
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return Credential{}, storeErr(ErrCredentialNotFound, err, "no credential in the OS keyring")
	case err != nil:
		return Credential{}, storeErr(ErrStoreUnavailable, err, "the OS keyring could not be read; unlock it or use --credential-store file")
	}
	c, err := decodeCredential([]byte(raw))
	if err != nil {
		return Credential{}, storeErr(ErrStoreUnavailable, err, "the stored credential is unreadable; run `kodama auth login` again")
	}
	return c, nil
}

func (s *KeyringStore) Save(ctx context.Context, key string, c Credential) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := s.guard(ctx); err != nil {
		return err
	}
	b, err := encodeCredential(c)
	if err != nil {
		return storeErr(ErrPersistenceFailed, err, "the credential cannot be encoded")
	}
	_, err = bounded(ctx, func() (struct{}, error) { return struct{}{}, s.kr.Set(s.service, account(key), string(b)) })
	if isCtxFailure(err) {
		return err
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrSetDataTooBig):
		return storeErr(ErrPersistenceFailed, err, "the credential is too large for the OS keyring")
	default:
		return storeErr(ErrStoreUnavailable, err, "the OS keyring is unavailable; unlock it or use --credential-store file")
	}
}

func (s *KeyringStore) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := s.guard(ctx); err != nil {
		return err
	}
	_, err := bounded(ctx, func() (struct{}, error) { return struct{}{}, s.kr.Delete(s.service, account(key)) })
	if isCtxFailure(err) {
		return err
	}
	switch {
	case err == nil, errors.Is(err, keyring.ErrNotFound):
		return nil
	default:
		return storeErr(ErrStoreUnavailable, err, "the OS keyring is unavailable")
	}
}
