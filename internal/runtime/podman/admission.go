package podman

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// ResourceCredentialFile is the file inside a workload's bootstrap directory
// that holds its resource admission credential.
const ResourceCredentialFile = "admission"

// ResourceRegisterCommand registers the admission credential of one run
// resource workload (a preparation or a service): seam S5. It mirrors the
// runtime-admission command so the composition bridges them with a field copy
// (runtimeauth.Service.RegisterResource); the adapter owns only this shape.
type ResourceRegisterCommand struct {
	Scope domain.Scope
	RunID domain.RunID
	// Key is the resource key (preparation or service.NAME).
	Key        string
	Credential runtimeauth.Credential
	// SpecFingerprint is the resource spec's fingerprint the credential is
	// bound to.
	SpecFingerprint string
	// ExpiresAt is the resource's deadline.
	ExpiresAt time.Time
}

// ResourceRegistrar registers a resource workload's credential. It must be
// idempotent for the same credential and fail with an error matching
// runtimeauth.ErrConflict for a different one.
type ResourceRegistrar interface {
	RegisterResource(ctx context.Context, cmd ResourceRegisterCommand) error
}

// admit makes dir/admission hold the workload's credential and registers it.
// The plaintext is generated once and persisted BEFORE it is registered,
// because admission stores only a digest and refuses another credential for the
// same resource: a lost response, a failed create or a restart registers the
// same value again. It is a no-op without a Registrar.
func (r *Resources) admit(ctx context.Context, spec execution.ResourceSpec, dir string) (registered bool, err error) {
	if r.cfg.Registrar == nil {
		return false, nil
	}
	expires, err := time.Parse(time.RFC3339, spec.DeadlineAt)
	if err != nil {
		return false, fmt.Errorf("%w: the resource spec has no deadline", execution.ErrInvalid)
	}
	fp, err := spec.Fingerprint()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	cred, err := persistedCredential(filepath.Join(dir, ResourceCredentialFile))
	if err != nil {
		return false, err
	}
	err = r.cfg.Registrar.RegisterResource(ctx, ResourceRegisterCommand{
		Scope: spec.Scope, RunID: spec.RunID, Key: spec.Key, Credential: cred, SpecFingerprint: fp, ExpiresAt: expires.UTC(),
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, runtimeauth.ErrConflict):
		return false, fmt.Errorf("%w: runtime admission holds another credential for the resource: %w", execution.ErrConflict, err)
	}
	return false, fmt.Errorf("registering the resource admission credential: %w", err)
}

// persistedCredential reads the credential stored in file or generates one and
// stores it (mode 0600) before anyone can register it.
func persistedCredential(file string) (runtimeauth.Credential, error) {
	for {
		raw, err := os.ReadFile(file)
		if err == nil {
			return runtimeauth.ParseCredential(string(raw))
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return runtimeauth.Credential{}, err
		}
		cred, err := runtimeauth.GenerateCredential(nil)
		if err != nil {
			return runtimeauth.Credential{}, err
		}
		err = writeDurably(filepath.Dir(file), file, []byte(cred.Reveal()), os.Link)
		if errors.Is(err, fs.ErrExist) {
			continue // a concurrent issuer stored its credential first: use that one
		}
		return cred, err
	}
}
