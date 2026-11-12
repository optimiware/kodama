package adaptercontract

import (
	"context"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/runtime"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// The fake adapters below are compile-only conformance consumers. They prove
// each declared port is implementable without a real database, process,
// container, broker, identity provider, registry or KMS. No fake is started by
// a test.

type fakeRows struct{}

func (fakeRows) Next() bool        { return false }
func (fakeRows) Scan(...any) error { return nil }
func (fakeRows) Err() error        { return nil }
func (fakeRows) Close() error      { return nil }

type fakeTransaction struct{}

func (fakeTransaction) Exec(context.Context, store.Statement) (store.Result, error) {
	return store.Result{}, nil
}
func (fakeTransaction) Query(context.Context, store.Query) (store.Rows, error) {
	return fakeRows{}, nil
}
func (fakeTransaction) Commit(context.Context) error   { return nil }
func (fakeTransaction) Rollback(context.Context) error { return nil }

type fakeScopedConnection struct{}

func (fakeScopedConnection) Begin(context.Context, tenancy.Scope) (store.Transaction, error) {
	return fakeTransaction{}, nil
}

// fakeScanner is a compile-only Scanner consumer. It proves the separate
// scoped read seam is implementable without a database and shares the
// declaration-level validation.
type fakeScanner struct{}

func (fakeScanner) Query(_ context.Context, _ tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	if err := query.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	return store.QueryPage{}, nil
}

type fakeMaterializer struct{}

func (fakeMaterializer) Materialize(context.Context, workspace.Materialization) (workspace.Preparation, error) {
	return workspace.Preparation{Complete: true}, nil
}

type fakeRetainedSink struct{}

func (fakeRetainedSink) Append(_ context.Context, scope tenancy.Scope, _ log.Chunk) (log.AppendResult, error) {
	if err := log.ValidateScope(scope); err != nil {
		return log.AppendResult{}, err
	}
	return log.AppendResult{}, nil
}
func (fakeRetainedSink) Read(_ context.Context, request log.ReadRequest) (log.ReadResult, error) {
	if err := request.Validate(); err != nil {
		return log.ReadResult{}, err
	}
	return log.ReadResult{}, nil
}

type fakeLivePublisher struct{}

func (fakeLivePublisher) PublishLive(_ context.Context, scope tenancy.Scope, _ log.Chunk) error {
	return log.ValidateScope(scope)
}

type fakeLiveSubscription struct{}

func (fakeLiveSubscription) Next(context.Context) (log.Chunk, error) { return log.Chunk{}, nil }
func (fakeLiveSubscription) Close(context.Context) error             { return nil }

type fakeLiveSubscriber struct{}

func (fakeLiveSubscriber) SubscribeLive(_ context.Context, scope tenancy.Scope, _ id.ExecutionID) (log.LiveSubscription, error) {
	if err := log.ValidateScope(scope); err != nil {
		return nil, err
	}
	return fakeLiveSubscription{}, nil
}

type fakeSecretResolver struct{}

func (fakeSecretResolver) Resolve(context.Context, secret.ResolveRequest) ([]secret.Resolved, error) {
	return nil, nil
}

type fakeKeyProvider struct{}

func (fakeKeyProvider) Wrap(context.Context, secret.WrapRequest) (secret.WrappedKey, error) {
	return secret.WrappedKey{}, nil
}
func (fakeKeyProvider) Unwrap(context.Context, secret.UnwrapRequest) ([]byte, error) {
	return nil, nil
}

type fakeRuntimeClient struct{}

func (fakeRuntimeClient) Capabilities(context.Context) (engine.Capabilities, error) {
	return engine.Capabilities{}, nil
}
func (fakeRuntimeClient) Prepare(context.Context, engine.PrepareRequest) (engine.Preparation, error) {
	return engine.Preparation{}, nil
}
func (fakeRuntimeClient) Start(context.Context, engine.StartRequest) (engine.StartResult, error) {
	return engine.StartResult{}, nil
}
func (fakeRuntimeClient) Observe(context.Context, engine.ObserveRequest) (kernel.Observation, error) {
	return kernel.UnknownObservation(), nil
}
func (fakeRuntimeClient) Logs(context.Context, engine.LogsRequest) (engine.LogBatch, error) {
	return engine.LogBatch{}, nil
}
func (fakeRuntimeClient) Stop(context.Context, engine.StopRequest) (kernel.StopEvidence, error) {
	return kernel.PendingStop("pending"), nil
}
func (fakeRuntimeClient) Remove(context.Context, engine.RemoveRequest) (engine.Removal, error) {
	return engine.Removal{}, nil
}
func (fakeRuntimeClient) RefreshCredential(context.Context, engine.CredentialRequest) (engine.CredentialRefresh, error) {
	return engine.CredentialRefresh{}, nil
}
func (fakeRuntimeClient) Version(context.Context) (runtime.APIVersion, error) {
	return runtime.APIVersion{}, nil
}

type fakeEventPublisher struct{}

func (fakeEventPublisher) Publish(context.Context, event.Envelope) error { return nil }

type fakeInvalidationSink struct{}

func (fakeInvalidationSink) Invalidate(context.Context, token.Invalidation) error { return nil }

type fakeNATSPublisher struct{}

func (fakeNATSPublisher) PublishEvent(context.Context, event.Envelope) error { return nil }
func (fakeNATSPublisher) PublishLive(_ context.Context, scope tenancy.Scope, chunk log.Chunk) error {
	_, err := nats.LiveLogSubject(scope, chunk)
	return err
}

type fakeNATSSubscription struct{}

func (fakeNATSSubscription) Close(context.Context) error { return nil }

type fakeNATSSubscriber struct{}

func (fakeNATSSubscriber) SubscribeInvalidations(context.Context, tenancy.Scope, nats.InvalidationHandler) (nats.Subscription, error) {
	return fakeNATSSubscription{}, nil
}

type fakeOIDCVerifier struct{}

func (fakeOIDCVerifier) Verify(context.Context, identity.OIDCRequest) (identity.Principal, error) {
	return identity.Principal{}, nil
}

type fakeHostedAuthority struct{}

func (fakeHostedAuthority) Check(_ context.Context, request authorize.HostedAuthorityRequest) (authorize.HostedAuthorityResponse, error) {
	return authorize.HostedAuthorityResponse{
		Protocol: request.Protocol,
		Decision: authorize.HostedAuthorityDecision{
			Allowed: true,
			Role:    request.Claims.Role,
			Access:  append([]string(nil), request.RequiredAccess...),
		},
	}, nil
}

type fakeImageResolver struct{}

func (fakeImageResolver) Resolve(context.Context, pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	return pipeline.ResolvedImage{}, nil
}

type fakeBackupPort struct{}

func (fakeBackupPort) Backup(context.Context, installation.BackupPlan) (installation.BackupResult, error) {
	return installation.BackupResult{}, nil
}

type fakeRestorePort struct{}

func (fakeRestorePort) Preflight(context.Context, installation.BackupIdentity) (installation.BackupEvidence, error) {
	return installation.BackupEvidence{}, nil
}

func (fakeRestorePort) Restore(context.Context, installation.RestorePlan) (installation.RestoreResult, error) {
	return installation.RestoreResult{}, nil
}

func (fakeRestorePort) Reconcile(context.Context) (installation.ReconcileResult, error) {
	return installation.ReconcileResult{}, nil
}

type fakeFencePort struct{}

func (fakeFencePort) Fence(context.Context) error { return nil }

type fakeDispatchGate struct{}

func (fakeDispatchGate) EnableDispatch(context.Context) error { return nil }

var (
	_ store.ScopedConnection    = fakeScopedConnection{}
	_ store.Transaction         = fakeTransaction{}
	_ store.Scanner             = fakeScanner{}
	_ store.Rows                = fakeRows{}
	_ workspace.Materializer    = fakeMaterializer{}
	_ log.RetainedSink          = fakeRetainedSink{}
	_ log.LivePublisher         = fakeLivePublisher{}
	_ log.LiveSubscriber        = fakeLiveSubscriber{}
	_ log.LiveSubscription      = fakeLiveSubscription{}
	_ secret.Resolver           = fakeSecretResolver{}
	_ secret.KeyProvider        = fakeKeyProvider{}
	_ runtime.Client            = fakeRuntimeClient{}
	_ engine.Runtime            = (runtime.Client)(nil)
	_ event.Publisher           = fakeEventPublisher{}
	_ token.InvalidationPort    = fakeInvalidationSink{}
	_ nats.Publisher            = fakeNATSPublisher{}
	_ nats.Subscriber           = fakeNATSSubscriber{}
	_ nats.Subscription         = fakeNATSSubscription{}
	_ identity.OIDCVerifier     = fakeOIDCVerifier{}
	_ authorize.HostedAuthority = fakeHostedAuthority{}
	_ pipeline.Resolver         = fakeImageResolver{}
	_ installation.BackupPort   = fakeBackupPort{}
	_ installation.RestorePort  = fakeRestorePort{}
	_ installation.FencePort    = fakeFencePort{}
	_ installation.DispatchGate = fakeDispatchGate{}
)
