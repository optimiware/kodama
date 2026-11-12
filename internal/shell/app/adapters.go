package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// identitySource adds the verification identity the operation bindings mint to
// the shared secure allocator.
type identitySource struct {
	*bridges.IDs
}

func (source identitySource) NewVerificationID() (verify.VerificationID, error) {
	raw, err := source.NewRowID()
	if err != nil {
		return "", err
	}
	return verify.VerificationID("verification-" + raw), nil
}

// tenantKeySource mints the encrypted tenant derivation key through the
// envelope cipher. It never returns plaintext key material.
type tenantKeySource struct {
	cipher     secret.EnvelopeCipher
	associated secret.AssociatedData
	keyVersion secret.KeyVersion
}

var _ opsapp.TenantKeySource = tenantKeySource{}

func (source tenantKeySource) NewTenantKey(ctx context.Context, tenantID id.TenantID, version uint64) (token.TenantKey, error) {
	if source.cipher == nil {
		return token.TenantKey{}, fault.New(fault.InternalFailure, "tenant key source is not configured")
	}
	if version == 0 {
		return token.TenantKey{}, fault.New(fault.InvalidInput, "tenant key version is zero")
	}
	var material [32]byte
	if _, err := rand.Read(material[:]); err != nil {
		return token.TenantKey{}, fault.Wrap(fault.InternalFailure, "tenant key material generation failed", err)
	}
	associated := source.associated
	associated.TenantID = tenantID
	associated.ProjectID = ""
	associated.KeyVersion = max(source.keyVersion, secret.MinKeyVersion)
	envelope, err := source.cipher.Seal(ctx, associated, material[:])
	if err != nil {
		return token.TenantKey{}, fault.Wrap(fault.DependencyUnavailable, "tenant key sealing failed", err)
	}
	key, err := token.NewTenantKey(tenantID, version, token.TenantKeyMaterial{
		Ciphertext: append(append([]byte(nil), envelope.Nonce...), envelope.Ciphertext...),
		WrappedKey: envelope.WrappedKey,
		WrapNonce:  envelope.WrapNonce,
		KeyRef:     string(envelope.KeyRef),
		KeyVersion: uint64(envelope.KeyVersion),
	})
	if err != nil {
		return token.TenantKey{}, err
	}
	return key, nil
}

// eventPublication binds the already authenticated public principal to the
// event ingress credential boundary for one call.
type eventPublication struct {
	ingress     *event.Ingress
	credentials *bindingAuthenticator
}

var _ opsapp.EventPublication = (*eventPublication)(nil)

func (publication *eventPublication) Publish(ctx context.Context, principal authorize.Principal, attempt event.PublishAttempt) (event.IngressResult, error) {
	var (
		result event.IngressResult
		err    error
	)
	bindErr := publication.credentials.withPrincipal(principal, func(credential string) error {
		attempt.Credential = credential
		result, err = publication.ingress.Publish(ctx, attempt)
		return nil
	})
	if bindErr != nil {
		return event.IngressResult{}, bindErr
	}
	return result, err
}

// disabledModels is the fail-closed optional model surface.
type disabledModels struct{}

var _ opsapp.ModelInvocation = disabledModels{}

func (disabledModels) Invoke(context.Context, authorize.Principal, id.ProjectID, api.ChatCompletionRequest) (api.ChatCompletionResponse, error) {
	return api.ChatCompletionResponse{}, fault.New(fault.NotFound, "the optional model gateway is not enabled")
}

// disabledTools is the fail-closed optional external tool surface.
type disabledTools struct{}

var _ opsapp.ToolInvocation = disabledTools{}

func (disabledTools) Call(context.Context, authorize.Principal, id.ProjectID, string, json.RawMessage) (mcpproxy.CallResult, error) {
	return mcpproxy.CallResult{}, fault.New(fault.NotFound, "the optional MCP tool proxy is not enabled")
}

// actorActions resolves one forwarded actor credential to the current running
// container action of its execution. It is the composition-side implementation
// of the actor.verify action seam: the credential is authenticated server-side,
// a human or service actor names no action, and an execution actor with no
// running container action is refused instead of inheriting an empty target.
type actorActions struct {
	principals authorize.CurrentPrincipal
	identities bridges.ExecutionIdentityResolver
}

var _ opsapp.ActorAction = actorActions{}

func (resolver actorActions) ResolveActorAction(ctx context.Context, credential string) (id.ActionID, error) {
	actor, err := resolver.principals.Resolve(ctx, credential)
	if err != nil {
		return "", err
	}
	if actor.Kind != identity.KindExecution {
		// A human membership or a service row holds no execution, so its
		// verification attempt names no action.
		return "", nil
	}
	resolved, err := resolver.identities.ResolveExecutionIdentity(ctx, actor.ExecutionID)
	if err != nil {
		// An execution whose credential names no current running container
		// action is refused: its authority is that action and its deadline
		// together, and there is nothing else to guard.
		return "", err
	}
	return resolved.ActionID, nil
}

// executionIdentities resolves the current running workload identity of one
// execution through the cross-scope controller seam. It is composition glue for
// post-commit credential delivery, never a durable permission.
type executionIdentities struct {
	controller store.Controller
}

var _ bridges.ExecutionIdentityResolver = (*executionIdentities)(nil)

func (resolver *executionIdentities) ResolveExecutionIdentity(ctx context.Context, executionID id.ExecutionID) (kernelengine.EffectIdentity, error) {
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return kernelengine.EffectIdentity{}, fault.Wrap(fault.InvalidInput, "execution identity is invalid", err)
	}
	page, err := resolver.controller.Scan(ctx, store.ControllerScan{
		Table: "actions",
		Equals: map[string]any{
			"execution_id": string(executionID),
			"state":        "running",
			"kind":         "container",
		},
		OrderBy: []store.Order{{Column: "index"}, {Column: "id"}},
		Limit:   1})
	if err != nil {
		return kernelengine.EffectIdentity{}, err
	}
	if len(page.Rows) == 0 {
		return kernelengine.EffectIdentity{}, fault.New(fault.DependencyUnavailable, "execution has no running container workload")
	}
	row := page.Rows[0]
	actionID, err := rowText(row, "id")
	if err != nil {
		return kernelengine.EffectIdentity{}, err
	}
	workload, err := rowText(row, "workload_id")
	if err != nil {
		return kernelengine.EffectIdentity{}, err
	}
	launchKey, err := rowText(row, "launch_key")
	if err != nil {
		return kernelengine.EffectIdentity{}, err
	}
	return kernelengine.EffectIdentity{
		ExecutionID:    executionID,
		ActionID:       id.ActionID(actionID),
		WorkloadID:     id.WorkloadID(workload),
		IdempotencyKey: launchKey,
	}, nil
}

func rowText(row store.Row, column string) (string, error) {
	value, ok := row.Values[column]
	if !ok || value == nil {
		return "", fault.New(fault.DependencyUnavailable, fmt.Sprintf("row %s has no %s column", row.Table, column))
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.DependencyUnavailable, fmt.Sprintf("row %s %s column has an unexpected type", row.Table, column))
	}
	return text, nil
}
