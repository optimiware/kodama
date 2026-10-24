package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
)

// ModelCredentialPayload is the bounded payload format of a `kind=model` secret
// the gateway accepts: a bearer key bound to exactly one approved HTTPS origin.
// The secret store leaves payload formats to its consumers; this one is the
// gateway's.
//
//	{"version":1,"bearer":"<api key>","origin":"https://api.example.com[:port]"}
//
// The origin is the credential's audience: it must equal the origin of the
// run's endpoint, otherwise the key is refused rather than sent elsewhere.
type ModelCredentialPayload struct {
	Version int    `json:"version"`
	Bearer  string `json:"bearer"`
	Origin  string `json:"origin"`
}

// MaxPayloadBytes bounds a model credential payload.
const MaxPayloadBytes = 8 << 10

// ParsePayload strictly decodes a payload. It copies the bearer into a fresh
// buffer the caller owns; the decoded string copy Go's JSON decoder makes cannot
// be erased, which is the residual exposure of this payload format.
func ParsePayload(raw []byte) (bearer []byte, origin string, err error) {
	if len(raw) == 0 || len(raw) > MaxPayloadBytes {
		return nil, "", gateway.Failf(gateway.ErrCredential, "the credential payload is not usable")
	}
	var p ModelCredentialPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || dec.More() || p.Version != 1 || p.Bearer == "" || p.Origin == "" {
		return nil, "", gateway.Failf(gateway.ErrCredential, "the credential payload is not usable")
	}
	b := []byte(p.Bearer)
	p.Bearer = ""
	return b, p.Origin, nil
}

// SecretCatalog reads secret metadata for settings validation and the kind
// check. *secretspg.Store satisfies secrets.Reader.
type SecretCatalog interface {
	ListSecrets(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]secrets.Secret, error)
	GetSecretVersion(ctx context.Context, scope domain.Scope, secretID string, version int64) (secrets.SecretVersion, error)
}

// Credentials implements gateway.Credentials over the secrets module's exact
// version source.
type Credentials struct {
	source  secrets.VersionSource
	catalog SecretCatalog
}

var _ gateway.Credentials = (*Credentials)(nil)

// NewCredentials requires both collaborators.
func NewCredentials(source secrets.VersionSource, catalog SecretCatalog) (*Credentials, error) {
	if source == nil || catalog == nil {
		return nil, errors.New("gateway/adapters: a version source and a secret catalog are required")
	}
	return &Credentials{source: source, catalog: catalog}, nil
}

func secretsError(err error) error {
	var ke *secrets.KeyError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrNotPinned), errors.Is(err, secrets.ErrRevoked),
		errors.Is(err, secrets.ErrDeleted), errors.Is(err, secrets.ErrAttemptInactive):
		return gateway.FailWith(gateway.ErrCredential, err, "the credential is not usable")
	case errors.As(err, &ke), errors.Is(err, secrets.ErrUnavailable), errors.Is(err, secrets.ErrFinalizationUncertain),
		errors.Is(err, secrets.ErrInconsistent), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// Key-provider, audit and consistency failures: nothing was released.
		return gateway.FailWith(gateway.ErrUnavailable, err, "the credential could not be prepared")
	}
	return gateway.FailWith(gateway.ErrUnavailable, err, "the credential could not be prepared")
}

// Open implements gateway.Credentials. Order: read and decrypt exactly the pinned
// version (no newer version is ever substituted), finalize its audit and
// revocation check, then validate the payload's audience against the run's
// endpoint. The plaintext buffer is cleared before returning. Errors carry no
// credential detail.
func (c *Credentials) Open(ctx context.Context, w gateway.Workload, b gateway.RunBinding, id gateway.RequestID) (*gateway.Credential, error) {
	pending, err := c.source.Open(ctx, secrets.VersionRequest{Scope: w.Scope, RunID: w.RunID, SecretName: b.Credential.Name(), Version: b.Credential.Version})
	if err != nil {
		return nil, secretsError(err)
	}
	defer pending.Discard()
	if err := pending.Finalize(ctx, secrets.FinalizeInfo{AttemptID: w.AttemptID, RunID: w.RunID, Delivery: runtimev1.DeliveryEnv, RequestID: string(id)}); err != nil {
		return nil, secretsError(err)
	}
	bearer, origin, err := ParsePayload(pending.Value())
	if err != nil {
		return nil, err
	}
	if origin != b.Endpoint.Origin() {
		clear(bearer)
		return nil, gateway.Failf(gateway.ErrCredential, "the credential is not valid for the run's endpoint")
	}
	return gateway.NewCredential(bearer), nil
}

// lookup pages through the project's secrets for the named one. A project holds
// few model credentials; the walk is bounded.
func (c *Credentials) lookup(ctx context.Context, scope domain.Scope, name string) (secrets.Secret, error) {
	after := ""
	for page := 0; page < 50; page++ {
		list, err := c.catalog.ListSecrets(ctx, scope, after, 100)
		if err != nil {
			return secrets.Secret{}, secretsError(err)
		}
		for _, s := range list {
			if s.Name() == name {
				return s, nil
			}
		}
		if len(list) < 100 {
			break
		}
		after = list[len(list)-1].Name()
	}
	return secrets.Secret{}, gateway.Failf(gateway.ErrCredential, "the credential is not usable")
}

// Check implements gateway.Credentials: the secret must exist in the project,
// not be deleted, be of kind model, and the exact version must exist and not be
// revoked.
func (c *Credentials) Check(ctx context.Context, scope domain.Scope, ref gateway.CredentialRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	s, err := c.lookup(ctx, scope, ref.Name())
	if err != nil {
		return err
	}
	if s.Deleted() || s.Kind() != secrets.KindModel || ref.Version > s.CurrentVersion() {
		return gateway.Failf(gateway.ErrCredential, "the credential is not a usable model secret")
	}
	v, err := c.catalog.GetSecretVersion(ctx, scope, s.ID(), ref.Version)
	if err != nil {
		return secretsError(err)
	}
	if v.Revoked() {
		return gateway.Failf(gateway.ErrCredential, "the credential version is revoked")
	}
	return nil
}

// Pins implements gatewaypg.PinRegistrar over the secrets module's run pins.
type Pins struct{ store *secretspg.Store }

var _ gatewaypg.PinRegistrar = (*Pins)(nil)

// NewPins requires the secrets store.
func NewPins(store *secretspg.Store) (*Pins, error) {
	if store == nil {
		return nil, errors.New("gateway/adapters: a secrets store is required")
	}
	return &Pins{store: store}, nil
}

// Register implements gatewaypg.PinRegistrar. The pin is registered for the run
// in the caller's transaction (which already holds the execution run lock), so a
// run never has a binding without its pin and the secrets module's revocation
// accounting sees the gateway as a consumer. The binding is never added to the
// attempt's file or environment bindings.
func (p *Pins) Register(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, ref gateway.CredentialRef) error {
	err := p.store.RegisterPins(ctx, tx, secretspg.PinRequest{
		Scope: scope, Consumer: secretspg.RunConsumer(run),
		Bindings: []runtimev1.SecretBinding{{Ref: ref.Ref, Version: ref.Version, Name: "model", Delivery: runtimev1.DeliveryEnv}},
	})
	return secretsError(err)
}
