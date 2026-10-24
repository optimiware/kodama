package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	mcppg "github.com/optimiware/kodama/internal/mcpproxy/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
)

// PinRegistrar is the secrets store's pin registration, as the proxy consumes
// it: *secretspg.Store satisfies it.
type PinRegistrar interface {
	RegisterPins(ctx context.Context, tx pgx.Tx, req secretspg.PinRequest) error
}

// Pins adapts the secrets module's RegisterPins to the proxy store's PinFunc.
// The pin is proxy-owned: it retains the exact credential version for the run
// so the secrets module's revocation accounting sees the consumer, and it is
// never added to the secret bindings delivered to the workload.
func Pins(r PinRegistrar) (mcppg.PinFunc, error) {
	if r == nil {
		return nil, errors.New("mcpproxy/adapters: a pin registrar is required")
	}
	return func(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, name string, version int64) error {
		err := r.RegisterPins(ctx, tx, secretspg.PinRequest{
			Scope: scope, Consumer: secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(run)},
			Bindings: []runtimev1.SecretBinding{{Ref: secrets.FormatSecretRef(name), Version: version, Name: name, Delivery: runtimev1.DeliveryEnv}},
		})
		return mapSecretErr(err)
	}, nil
}

func mapSecretErr(err error) error {
	var ke *secrets.KeyError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrNotPinned), errors.Is(err, secrets.ErrRevoked),
		errors.Is(err, secrets.ErrDeleted), errors.Is(err, secrets.ErrAttemptInactive), errors.Is(err, secrets.ErrUndeclared):
		return mcpproxy.ErrCredential
	case errors.Is(err, secrets.ErrInvalid):
		return mcpproxy.ErrCredential
	case errors.As(err, &ke), errors.Is(err, secrets.ErrUnavailable), errors.Is(err, secrets.ErrFinalizationUncertain), errors.Is(err, secrets.ErrInconsistent):
		return mcpproxy.Unavailablef(err, "the credential could not be released")
	}
	return mcpproxy.Unavailablef(err, "the credential could not be released")
}

// Credential payload bounds. The payload is an MCP bearer credential with an
// explicit intended recipient. It is integration-owned credential
// configuration: it does not prove a JWT audience and is not Platform
// delegation.
const (
	maxPayloadBytes = 8 << 10
	maxTokenBytes   = 4 << 10
	payloadVersion  = 1
)

type payload struct {
	Version   int    `json:"version"`
	Recipient string `json:"recipient"`
	Token     string `json:"token"`
}

// DecodeCredential strictly decodes an MCP credential payload.
func DecodeCredential(raw []byte) (recipient string, token []byte, err error) {
	if len(raw) == 0 || len(raw) > maxPayloadBytes {
		return "", nil, mcpproxy.ErrCredential
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p payload
	if err := dec.Decode(&p); err != nil {
		return "", nil, mcpproxy.ErrCredential
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", nil, mcpproxy.ErrCredential
	}
	if p.Version != payloadVersion || p.Recipient == "" || p.Token == "" || len(p.Token) > maxTokenBytes {
		return "", nil, mcpproxy.ErrCredential
	}
	for _, r := range p.Token {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII {
			return "", nil, mcpproxy.ErrCredential
		}
	}
	return p.Recipient, []byte(p.Token), nil
}

// EncodeCredential builds a payload (used by tools and tests that create the
// MCP secret).
func EncodeCredential(recipient, token string) []byte {
	b, _ := json.Marshal(payload{Version: payloadVersion, Recipient: recipient, Token: token})
	return b
}

// Credentials implements mcpproxy.CredentialSource over the secrets module's
// version source. It opens exactly the pinned version of the run binding,
// decodes the payload, requires the declared recipient to equal the approved
// destination's origin, and finalizes the resolution (which rechecks
// revocation and the run pin and commits the audit entry) before releasing
// anything. A denied, revoked, missing, mis-addressed or uncertain resolution
// releases nothing, and buffers are cleared after use.
type Credentials struct{ source secrets.VersionSource }

var _ mcpproxy.CredentialSource = (*Credentials)(nil)

// NewCredentials requires the version source.
func NewCredentials(s secrets.VersionSource) (*Credentials, error) {
	if s == nil {
		return nil, errors.New("mcpproxy/adapters: a secret version source is required")
	}
	return &Credentials{source: s}, nil
}

type credential struct {
	token     []byte
	recipient string
}

func (c *credential) Bearer() []byte    { return c.token }
func (c *credential) Recipient() string { return c.recipient }
func (c *credential) Discard()          { clear(c.token) }

// Open implements mcpproxy.CredentialSource.
func (c *Credentials) Open(ctx context.Context, req mcpproxy.CredentialRequest) (mcpproxy.Credential, error) {
	reg := req.Registration
	if reg.Scope != req.Caller.Scope {
		return nil, mcpproxy.ErrCredential
	}
	_, u, err := mcpproxy.NormalizeEndpoint(reg.Endpoint)
	if err != nil {
		return nil, mcpproxy.ErrCredential
	}
	pending, err := c.source.Open(ctx, secrets.VersionRequest{Scope: reg.Scope, RunID: req.Caller.RunID, SecretName: reg.Secret.Name, Version: reg.Secret.Version})
	if err != nil {
		return nil, mapSecretErr(err)
	}
	defer pending.Discard()
	recipient, token, err := DecodeCredential(pending.Value())
	if err != nil {
		return nil, err
	}
	if !sameOrigin(recipient, mcpproxy.Origin(u)) {
		clear(token)
		return nil, mcpproxy.ErrCredential
	}
	if err := pending.Finalize(ctx, secrets.FinalizeInfo{AttemptID: req.Caller.AttemptID, RunID: req.Caller.RunID, Delivery: runtimev1.DeliveryEnv, RequestID: req.RequestID}); err != nil {
		clear(token)
		return nil, mapSecretErr(err)
	}
	return &credential{token: token, recipient: mcpproxy.Origin(u)}, nil
}

func sameOrigin(a, b string) bool { return strings.EqualFold(strings.TrimSuffix(a, "/"), b) }
