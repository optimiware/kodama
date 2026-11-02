package process

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

// CredentialFile is the file inside a workload's identity directory that holds
// its resource admission credential.
const CredentialFile = "admission"

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

// admission makes dir/admission hold the workload's credential and registers
// it. The plaintext is generated once and persisted BEFORE it is registered,
// because admission stores only a digest and refuses another credential for the
// same resource: a lost response, a failed launch or a restart registers the
// same value again. It returns "" when no Registrar is configured.
func (r *Resources) admission(ctx context.Context, spec execution.ResourceSpec, dir string) (string, error) {
	if r.cfg.Registrar == nil {
		return "", nil
	}
	expires, err := time.Parse(time.RFC3339, spec.DeadlineAt)
	if err != nil {
		return "", fmt.Errorf("%w: the resource spec has no deadline", execution.ErrInvalid)
	}
	fp, err := spec.Fingerprint()
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, CredentialFile)
	cred, err := persistedCredential(file)
	if err != nil {
		return "", err
	}
	err = r.cfg.Registrar.RegisterResource(ctx, ResourceRegisterCommand{
		Scope: spec.Scope, RunID: spec.RunID, Key: spec.Key, Credential: cred, SpecFingerprint: fp, ExpiresAt: expires.UTC(),
	})
	switch {
	case err == nil:
		return file, nil
	case errors.Is(err, runtimeauth.ErrConflict):
		return "", fmt.Errorf("%w: runtime admission holds another credential for the resource: %w", execution.ErrConflict, err)
	}
	return "", fmt.Errorf("registering the resource admission credential: %w", err)
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
		err = writeFileDurably(filepath.Dir(file), file, []byte(cred.Reveal()), os.Link)
		if errors.Is(err, fs.ErrExist) {
			continue // a concurrent issuer stored its credential first: use that one
		}
		return cred, err
	}
}
