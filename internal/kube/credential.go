package kube

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// The admission credential of an attempt lives in one attempt-scoped Secret in
// the tenant namespace. T-019 stores only its digest, so this Secret is the only
// place the plaintext survives a lost registration or create response: it is
// generated once, stored before registration, and every retry registers the
// same value. The Secret also carries the workload record (workload.go), which
// is external-effect bookkeeping protected by resource-version checks.

const maxRecordRetries = 6

// errRecordMoved means a conditional update lost a race; the caller re-reads and
// re-decides.
var errRecordMoved = errors.New("kube: the workload record changed concurrently")

// admission is the Secret of an attempt with its parsed record.
type admission struct {
	secret kubernetes.Secret
	wl     *Workload
}

func (a *admission) credential() (runtimeauth.Credential, error) {
	v := a.secret.Data[KeyAdmission]
	c, err := runtimeauth.ParseCredential(string(v))
	if err != nil {
		return runtimeauth.Credential{}, wrap(ErrCredentialConflict, "the stored credential is malformed")
	}
	return c, nil
}

// verifySecret checks the Secret is the attempt's own: managed labels, ownership
// annotations equal to committed data, no owner references, immutable data, and
// exactly the key set the committed access allows (checkSecretKeys).
func (c *core) verifySecret(id Identity, s kubernetes.Secret) error {
	bad := func(why string) error { return wrap(ErrForeignWorkload, "admission Secret: %s", why) }
	if s.Metadata.Name != SecretName(id.JobName) || s.Metadata.Namespace != id.Namespace {
		return bad("name or namespace differs")
	}
	if len(s.Metadata.OwnerReferences) != 0 {
		return bad("has an owner")
	}
	for k, v := range id.Labels() {
		if s.Metadata.Labels[k] != v {
			return bad("label differs")
		}
	}
	for k, v := range id.Annotations() {
		if s.Metadata.Annotations[k] != v {
			return bad("annotation differs")
		}
	}
	if s.Metadata.Annotations[AnnFingerprint] != id.Fingerprint {
		return bad("fingerprint differs")
	}
	if s.Immutable == nil || !*s.Immutable {
		return bad("data is not immutable")
	}
	if err := checkSecretKeys(layoutFor(id.Access), s.Data); err != nil {
		return bad(err.Error())
	}
	return nil
}

// loadAdmission reads and verifies the attempt's Secret. It returns (nil, nil)
// when the Secret does not exist.
func (c *core) loadAdmission(ctx context.Context, id Identity) (*admission, error) {
	s, err := c.cluster.GetSecret(ctx, id.Namespace, SecretName(id.JobName))
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, wrapCause(ErrUnavailable, err, "reading the admission Secret")
	}
	return c.admissionOf(id, s)
}

// admissionOf verifies a read Secret as the attempt's own and parses its record.
func (c *core) admissionOf(id Identity, s kubernetes.Secret) (*admission, error) {
	if err := c.verifySecret(id, s); err != nil {
		return nil, err
	}
	raw, ok := s.Metadata.Annotations[AnnWorkload]
	if !ok {
		return nil, wrap(ErrForeignWorkload, "admission Secret has no workload record")
	}
	wl, err := parseWorkload(id, raw)
	if err != nil {
		return nil, err
	}
	return &admission{secret: s, wl: wl}, nil
}

// createAdmission generates the credential and stores it with a credentialed
// record. A source-consuming attempt's checkout material is resolved here, once,
// before anything is created, and stored in the same immutable Secret: a
// refusal or outage of the provider creates nothing and registers nothing. A
// concurrent winner is read back and used instead (its stored material, never
// ours); a lost response is resolved by reading. A credential that was generated
// but never stored is discarded: nothing was registered with it.
func (c *core) createAdmission(ctx context.Context, exp Expectation) (*admission, error) {
	checkout, err := c.resolveCheckout(ctx, exp)
	if err != nil {
		return nil, err
	}
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		return nil, wrapCause(ErrUnavailable, err, "no randomness for the credential")
	}
	wl := NewWorkload(exp.Identity)
	if err := wl.Credentialed(); err != nil {
		return nil, err
	}
	ann := exp.Identity.Annotations()
	ann[AnnFingerprint] = exp.Identity.Fingerprint
	ann[AnnWorkload] = wl.marshal()
	imm := true
	s := kubernetes.Secret{
		Metadata:  kubernetes.ObjectMeta{Name: SecretName(exp.Identity.JobName), Namespace: exp.Identity.Namespace, Labels: exp.Identity.Labels(), Annotations: ann},
		Type:      "Opaque",
		Immutable: &imm,
		Data:      exp.Recipe.BootstrapSecretData(cred.Reveal(), checkout),
	}
	_, err = c.cluster.CreateSecret(ctx, exp.Identity.Namespace, s)
	switch {
	case err == nil, errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
		a, lerr := c.loadAdmission(ctx, exp.Identity)
		if lerr != nil {
			return nil, lerr
		}
		if a == nil {
			return nil, wrapCause(ErrUnavailable, errors.New("secret not readable after create"), "admission Secret")
		}
		return a, nil
	case errors.Is(err, kubernetes.ErrForbidden), errors.Is(err, kubernetes.ErrInvalid):
		return nil, wrapCause(ErrPreflight, err, "the admission Secret was refused")
	}
	return nil, wrapCause(ErrUnavailable, err, "creating the admission Secret")
}

// save persists the record with a resource-version precondition. A lost race is
// errRecordMoved. A lost response is resolved by reading the Secret back.
func (c *core) save(ctx context.Context, a *admission) error {
	id := a.wl.Identity
	want := a.wl.marshal()
	if a.secret.Metadata.Annotations[AnnWorkload] == want {
		return nil
	}
	next := a.secret
	next.Metadata.Annotations = map[string]string{}
	for k, v := range a.secret.Metadata.Annotations {
		next.Metadata.Annotations[k] = v
	}
	next.Metadata.Annotations[AnnWorkload] = want
	out, err := c.cluster.UpdateSecret(ctx, id.Namespace, next)
	switch {
	case err == nil:
		a.secret = out
		a.secret.Data = next.Data
		return nil
	case errors.Is(err, kubernetes.ErrConflict), errors.Is(err, kubernetes.ErrNotFound):
		return errRecordMoved
	case errors.Is(err, kubernetes.ErrUnknownOutcome):
		cur, lerr := c.loadAdmission(ctx, id)
		if lerr != nil || cur == nil {
			return wrapCause(ErrUnavailable, errors.Join(err, lerr), "saving the workload record")
		}
		if cur.secret.Metadata.Annotations[AnnWorkload] == want {
			*a = *cur
			return nil
		}
		return errRecordMoved
	}
	return wrapCause(ErrUnavailable, err, "saving the workload record")
}

// register idempotently registers the stored credential with runtime admission.
// A transport failure is retried with the identical credential by the caller; a
// contradicting registration means the credential cannot be reproduced.
func (c *core) register(ctx context.Context, exp Expectation, cred runtimeauth.Credential) error {
	actions, err := c.policy.Actions(exp.Recipe.Access)
	if err != nil {
		return err
	}
	_, err = c.registrar.Register(ctx, runtimeauth.RegisterCommand{
		Scope: exp.Identity.Scope, AttemptID: exp.Identity.AttemptID, Credential: cred, Actions: actions, Context: exp.Context,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		return wrap(ErrNotDispatchable, "the attempt is no longer authorized")
	case errors.Is(err, runtimeauth.ErrConflict):
		return wrap(ErrCredentialConflict, "a different registration exists for the attempt")
	case errors.Is(err, runtimeauth.ErrForbidden), errors.Is(err, runtimeauth.ErrInvalidRequest):
		return wrapCause(ErrPolicy, err, "registration refused")
	}
	return wrapCause(ErrUnavailable, err, "registration")
}

func (c *core) nowUTC() time.Time { return c.now().UTC() }

// checkCA refuses an admission Secret whose trust roots are not the policy's.
func (c *core) checkCA(exp Expectation, a *admission) error {
	if !bytes.Equal(a.secret.Data[KeyCA], exp.Recipe.policy.RuntimeCA()) {
		return wrap(ErrPolicyDrift, "the runtime CA changed")
	}
	return nil
}
