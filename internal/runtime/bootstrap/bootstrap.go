// Package bootstrap provisions the admission material of a workload that runs
// the real step entrypoint outside Kubernetes (the Kubernetes runtime keeps its
// credential in the attempt's admission Secret instead).
//
// Runtime admission stores only a credential's digest and refuses a second
// registration with another credential, so the plaintext is the one thing a
// lost response must not lose: it is generated once, persisted before it is
// registered, and every retry, including one by a restarted controller,
// registers the same value again.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// CredentialFile is the entrypoint's default admission credential file name
// inside its identity directory.
const CredentialFile = "admission"

// Registrar registers an attempt's admission credential. runtimeauth.Service
// implements it.
type Registrar interface {
	Register(ctx context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error)
}

// Issuer persists and registers admission credentials.
type Issuer struct {
	Registrar Registrar
	// Actions is the action ceiling per workspace access; nil selects
	// runtimeauth.DefaultActions. It cannot be combined with Ceiling.
	Actions func(execution.Access) (runtimeauth.ActionSet, error)
	// Ceiling selects the ceiling from the attempt's step kind and pinned
	// facts (model.invoke, tool.invoke, deployment.call under operator
	// configuration); nil keeps Actions or the defaults. The attempt authority
	// the grant is registered against must allow the same actions
	// (a grant above them is refused as forbidden).
	Ceiling *runtimeauth.CeilingPolicy
	// StepKind reports the step kind of an attempt, which its spec does not
	// carry. nil, an error or an empty answer derives it from the pinned
	// context (a pinned profile is an agent, anything else a container), so a
	// failure never widens the ceiling.
	StepKind func(ctx context.Context, spec execution.AttemptSpec) (runtimeauth.StepKind, error)
}

// Issue makes dir/admission hold the attempt's admission credential and
// registers it with the attempt's pinned context. The file is created once
// (mode 0600); a later Issue registers the credential it already holds.
// ErrConflict means runtime admission holds another credential for the
// attempt, which cannot be recovered.
func (i Issuer) Issue(ctx context.Context, spec execution.AttemptSpec, dir string) error {
	if i.Registrar == nil {
		return fmt.Errorf("%w: no admission registrar is configured", execution.ErrNotConfigured)
	}
	c, err := spec.DecodeContext()
	if err != nil {
		return err
	}
	cred, err := credential(filepath.Join(dir, CredentialFile))
	if err != nil {
		return err
	}
	actions, err := i.actions(ctx, spec, c)
	if err != nil {
		return err
	}
	_, err = i.Registrar.Register(ctx, runtimeauth.RegisterCommand{
		Scope: spec.Scope, AttemptID: spec.AttemptID, Credential: cred, Actions: actions, Context: c,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimeauth.ErrConflict):
		return fmt.Errorf("%w: runtime admission holds another credential: %w", execution.ErrConflict, err)
	}
	return fmt.Errorf("registering the admission credential: %w", err)
}

func (i Issuer) actions(ctx context.Context, spec execution.AttemptSpec, c runtimev1.Context) (runtimeauth.ActionSet, error) {
	switch {
	case i.Ceiling != nil && i.Actions != nil:
		return runtimeauth.ActionSet{}, fmt.Errorf("%w: an action ceiling policy and an actions function are both configured", execution.ErrNotConfigured)
	case i.Ceiling != nil:
		return i.Ceiling.Actions(ctx, i.ceilingInput(ctx, spec, c)), nil
	case i.Actions != nil:
		return i.Actions(spec.Access)
	}
	return runtimeauth.NewActionSet(runtimeauth.DefaultActions(spec.Access == execution.AccessWrite)...)
}

// ceilingInput gathers the pinned facts the ceiling is chosen from. The kind
// is the reported one when available; otherwise it is derived, and a failing
// report leaves it unknown only if the context itself has no agent pin.
func (i Issuer) ceilingInput(ctx context.Context, spec execution.AttemptSpec, c runtimev1.Context) runtimeauth.CeilingInput {
	kind := runtimeauth.StepContainer
	if c.Profile != nil {
		kind = runtimeauth.StepAgent
	}
	if i.StepKind != nil {
		switch k, err := i.StepKind(ctx, spec); {
		case err != nil:
			kind = "" // the report failed: an unknown kind keeps the defaults
		case k != "":
			kind = k
		}
	}
	return runtimeauth.CeilingInput{
		Scope: spec.Scope, Kind: kind, Writer: spec.Access == execution.AccessWrite,
		Profile: c.Profile, Deployment: c.Environment != nil,
	}
}

// credential reads the credential stored in file, or generates one and stores
// it before anyone can register it.
func credential(file string) (runtimeauth.Credential, error) {
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
		err = writeOnce(file, []byte(cred.Reveal()))
		if errors.Is(err, fs.ErrExist) {
			continue // a concurrent issuer stored its credential first: use that one
		}
		return cred, err
	}
}

// writeOnce durably creates file with data, mode 0600, and never replaces an
// existing file: the content is written and synced under a temporary name and
// then linked into place, which fails if the name exists.
func writeOnce(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), file); err != nil {
		return err
	}
	return syncDir(filepath.Dir(file))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ErrNoCheckout refuses a source-consuming attempt for which no checkout
// credential can be issued: the runtime has no CheckoutProvider (or, for the
// Process runtime, no checkout support at all). The entrypoint must not clone
// without the credential the attempt is entitled to.
var ErrNoCheckout = fmt.Errorf("%w: source checkout needs a configured checkout provider", execution.ErrNotConfigured)

// RequireNoCheckout refuses a context whose attempt consumes the workspace, for
// runtimes that issue no checkout credential.
func RequireNoCheckout(c runtimev1.Context) error {
	if ConsumesSource(c) {
		return ErrNoCheckout
	}
	return nil
}

// Identity is the workload identity the step entrypoint claims at admission.
// Outside Kubernetes one workload instance stands for both the Job and its
// Pod: both carry the workload name and its instance UID.
func Identity(w execution.Workload) runtimev1.JobIdentity {
	return runtimev1.JobIdentity{Name: w.Name, UID: w.UID, PodName: w.Name, PodUID: w.UID}
}

// Evidence is what a runtime reports to runtime admission for a workload it
// verified as the attempt's running instance, in namespace.
func Evidence(namespace string, w execution.Workload) runtimeauth.JobEvidence {
	return runtimeauth.JobEvidence{
		Namespace: namespace, JobName: w.Name, JobUID: w.UID,
		PodNamespace: namespace, PodName: w.Name, PodUID: w.UID, PodOwnerUID: w.UID,
	}
}

// Env returns the entrypoint's identity environment for w.
func Env(w execution.Workload) []string {
	id := Identity(w)
	return []string{
		"KODAMA_JOB_NAME=" + id.Name, "KODAMA_JOB_UID=" + id.UID,
		"KODAMA_POD_NAME=" + id.PodName, "KODAMA_POD_UID=" + id.PodUID,
	}
}
