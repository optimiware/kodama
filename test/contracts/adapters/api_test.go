// Package adaptercontract pins the adapter and persistence conformance
// contract.
//
// This file is the human-readable API pin for the declaration-only adapter
// packages. Tests in this package are compile-time consumers plus
// deterministic table/vector checks; they never start a process, container,
// database, broker, network call, identity provider, registry, cloud KMS or
// any other external service. Only the Go standard library is used.
//
// # Pinned declaration layout
//
//	internal/shell/store/adapter_contract.go
//	    ScopedConnection, Transaction, Statement, Query, Result, Rows,
//	    Column, ForeignKey, UniqueConstraint, Index, Check, Table, Schema,
//	    SchemaDesign() Schema
//	internal/shell/workspace/adapter_contract.go
//	    File.Validate, Materialization.Validate, Preparation.StartAllowed,
//	    Materializer, ValidatePath, IsReservedPath, InputPath, OutputPath,
//	    SecretPath, and the /kodama roots plus the reserved credential path
//	internal/shell/log/adapter_contract.go
//	    Retention, DefaultRetention, AppendResult, RetainedSink, ReadRequest,
//	    ReadResult, LivePublisher, LiveSubscriber, LiveSubscription
//	internal/shell/secret/adapter_contract.go
//	    Suite.Valid, Suites, KeyRef, KeyVersion, AssociatedData.Bytes,
//	    Envelope.Validate, Envelope.ValidateFor, ResolveRequest.Validate,
//	    Resolved.Validate, Resolver, WrapRequest.Validate, WrappedKey.Validate,
//	    UnwrapRequest, KeyProvider, MountPath
//	internal/shell/runtime/adapter_contract.go
//	    AdapterKind, AdapterKinds, APIVersion, PinnedAPIVersions,
//	    Capability, CapabilitiesFor, Client
//	internal/shell/event/adapter_contract.go
//	    Publisher
//	internal/shell/token/adapter_contract.go
//	    InvalidationReason, InvalidationReasons, Invalidation, InvalidationPort
//	internal/shell/identity/adapter_contract.go
//	    OIDCConfig, OIDCRequest, OIDCVerifier, OAuthProtocol,
//	    OAuthProtocols, OAuthClientConfig, OAuthClaims
//	internal/shell/authorize/adapter_contract.go
//	    RBACTable, HostedAuthorityRequest, HostedAuthorityDecision,
//	    HostedAuthorityResponse, HostedAuthority
//	internal/shell/pipeline/adapter_contract.go
//	    ImageReference, ParseImageReference, ResolveRequest, ResolvedImage,
//	    Resolver
//	internal/shell/integration/nats/adapter_contract.go
//	    Subject, EventSubject, LogSubject, InvalidationSubject, IngressSubject,
//	    ParseScope, Publisher, InvalidationHandler, Subscriber, Subscription,
//	    Client, Connect, Clock, BrokerConfig, SubscriptionRequest,
//	    AuthenticatedSubscriber, ObservationSink, Observation,
//	    ObservationRequest
//	internal/installation/types.go, ports.go, adapter_contract.go
//	    Environment, StateAdapter, MessagingAdapter, RuntimeAdapter, KeyAdapter,
//	    Config, Preflight, BackupScope, DefaultBackupScope, BackupPlan,
//	    RestoreStep, RestoreOrder, RestorePlan, BackupPort, RestorePort,
//	    FencePort, DispatchGate
//
// # Pinned value rules
//
// These values are part of the adapter contract, not a capability or adapter
// implementation. The adapters implement behavior; the contract fixes the
// shapes and deterministic value rules every adapter must share.
//
//   - The SQL seam is one scoped transaction with transaction-local scope and
//     Commit/Rollback. There is no generic Save/Update/SetState/Persist/Store/
//     Repository method and no split business write.
//   - The schema design is declaration data: every scoped table is under forced
//     RLS with distinct runtime and owner roles; tenant/project ownership is
//     enforced by composite foreign keys; nullable project scope uses
//     NULLS NOT DISTINCT uniqueness; state, kind, one-active-action and
//     nonce/expiry-pair constraints exist. resource_bindings carries the
//     required resource_id link with a composite resource foreign key and a
//     (tenant_id, project_id, resource_id, service_id) unique identity. The lock
//     order is exactly tenants, projects, memberships, services,
//     resource_bindings, pipelines, tasks, task_messages, executions, actions,
//     cleanup, verifications, receipts, audit, events. Migration names match
//     ^\d{4}_[a-z0-9_]+\.sql$ starting at 0001.
//   - Preparation materializes only under /kodama/inputs, /kodama/outputs,
//     /kodama/task.json and /kodama/secrets. Paths are absolute, confined, free
//     of traversal/duplicate separators, and never inside the writable
//     /workspace. /kodama/secrets/KODAMA_TOKEN is the sole reserved credential
//     mount and stored secret declarations cannot shadow it.
//   - The secret envelope suite is AES-256-GCM with a 12-byte content nonce, a
//     12-byte key-wrap nonce, a 32-byte data key and a 48-byte wrapped key.
//     Associated data is length-prefixed installation id, tenant id, project
//     id (zero length when tenant-scoped), secret id, secret name, then a
//     big-endian uint32 key version. Golden and tamper vectors live in
//     testdata/envelope.json.
//   - Runtime adapters expose exactly the Kernel runtime operations plus a
//     pinned external API version. Process is "process/v1", Podman is
//     "libpod/v4" and Kubernetes Jobs are "batch/v1". Start outcomes and
//     observations are the Kernel vocabulary; absent/unknown never
//     prove nonexecution and Job deletion alone is not stop confirmation.
//   - NATS subjects are scope-qualified: kodama.v1.<tenant>.<project|_>.<kind>
//     (events, invalidations, ingress) with the execution id appended for logs.
//     The ingress kind is the separate authenticated observation-ingress
//     subject and is never the committed-event subject.
//   - OIDC is an access-token verifier seam. Platform is two OAuth applications
//     behind Auth0 SSO: each has an explicit OAuth client config (protocol,
//     HTTPS issuer, client id, secret reference, audience, scopes, optional
//     HTTPS redirect), matches a tenant claim plus role string, and owns its own
//     RBACTable role -> access strings. No shared RBAC, vendor endpoint or
//     policy engine is pinned; exact Auth0 tenant/client values are installation
//     configuration. Registry resolution must return an immutable sha256 digest.
//   - Environment composition is explicit: development memory/memory/process/
//     ephemeral, test postgres/nats/podman/keyfile, production postgres/nats/
//     kubernetes/one cloud KMS. Production refuses every development/test
//     adapter and incomplete required settings. Test/production require
//     postgres://, nats:// or tls://, https:// public URL and a key ref.
//     Restore fences old actors and reconciles external effects before
//     dispatch; workspaces are excluded from backup and restore.
//
// # Build state
//
// Every declaration above exists, so a missing package or an undefined symbol
// is a contract break: the tests are the contract, not a malformed fixture.
package adaptercontract

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/installation"
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

// TestPortsTakeContextFirst proves every blocking adapter port takes
// context.Context as its first parameter. A port that accepts a bare value or
// hides cancellation in a later argument cannot be composed safely.
func TestPortsTakeContextFirst(t *testing.T) {
	ports := []struct {
		name  string
		iface reflect.Type
	}{
		{"store.ScopedConnection", reflect.TypeOf((*store.ScopedConnection)(nil)).Elem()},
		{"store.Transaction", reflect.TypeOf((*store.Transaction)(nil)).Elem()},
		{"workspace.Materializer", reflect.TypeOf((*workspace.Materializer)(nil)).Elem()},
		{"log.RetainedSink", reflect.TypeOf((*log.RetainedSink)(nil)).Elem()},
		{"log.LivePublisher", reflect.TypeOf((*log.LivePublisher)(nil)).Elem()},
		{"log.LiveSubscriber", reflect.TypeOf((*log.LiveSubscriber)(nil)).Elem()},
		{"secret.Resolver", reflect.TypeOf((*secret.Resolver)(nil)).Elem()},
		{"secret.KeyProvider", reflect.TypeOf((*secret.KeyProvider)(nil)).Elem()},
		{"runtime.Client", reflect.TypeOf((*runtime.Client)(nil)).Elem()},
		{"event.Publisher", reflect.TypeOf((*event.Publisher)(nil)).Elem()},
		{"token.InvalidationPort", reflect.TypeOf((*token.InvalidationPort)(nil)).Elem()},
		{"nats.Publisher", reflect.TypeOf((*nats.Publisher)(nil)).Elem()},
		{"nats.Subscriber", reflect.TypeOf((*nats.Subscriber)(nil)).Elem()},
		{"identity.OIDCVerifier", reflect.TypeOf((*identity.OIDCVerifier)(nil)).Elem()},
		{"authorize.HostedAuthority", reflect.TypeOf((*authorize.HostedAuthority)(nil)).Elem()},
		{"pipeline.Resolver", reflect.TypeOf((*pipeline.Resolver)(nil)).Elem()},
		{"installation.BackupPort", reflect.TypeOf((*installation.BackupPort)(nil)).Elem()},
		{"installation.RestorePort", reflect.TypeOf((*installation.RestorePort)(nil)).Elem()},
		{"installation.FencePort", reflect.TypeOf((*installation.FencePort)(nil)).Elem()},
		{"installation.DispatchGate", reflect.TypeOf((*installation.DispatchGate)(nil)).Elem()},
	}
	for _, port := range ports {
		assertContextFirst(t, port.name, port.iface)
	}
}

// TestTransactionPortHasNoGenericRepository proves the SQL seam is one scoped
// transaction with Commit/Rollback, not a generic repository or split state
// write. The forbidden names are the ones S01 explicitly refuses.
func TestTransactionPortHasNoGenericRepository(t *testing.T) {
	transaction := reflect.TypeOf((*store.Transaction)(nil)).Elem()
	want := []string{"Commit", "Exec", "Query", "Rollback"}
	if got := methodNames(transaction); !reflect.DeepEqual(got, want) {
		t.Fatalf("store.Transaction methods = %v; want exactly %v", got, want)
	}
	forbidden := []string{"Save", "Update", "SetState", "Persist", "Store", "Repository"}
	for _, name := range methodNames(transaction) {
		for _, banned := range forbidden {
			if name == banned {
				t.Errorf("store.Transaction exposes forbidden generic method %q", name)
			}
		}
	}
	connection := reflect.TypeOf((*store.ScopedConnection)(nil)).Elem()
	if got := methodNames(connection); !reflect.DeepEqual(got, []string{"Begin"}) {
		t.Fatalf("store.ScopedConnection methods = %v; want [Begin]", got)
	}
}

// TestScopedConnectionShape proves the SQL seam begins one transaction with a
// typed ownership scope and returns the transaction/error pair. Scope is
// established transaction-locally by the adapter, not passed on every query.
func TestScopedConnectionShape(t *testing.T) {
	iface := reflect.TypeOf((*store.ScopedConnection)(nil)).Elem()
	method, ok := iface.MethodByName("Begin")
	if !ok {
		t.Fatal("store.ScopedConnection has no Begin method")
	}
	scopeType := reflect.TypeOf(tenancy.Scope{})
	transactionType := reflect.TypeOf((*store.Transaction)(nil)).Elem()
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if method.Type.NumIn() != 2 || method.Type.In(0) != reflect.TypeOf((*context.Context)(nil)).Elem() || method.Type.In(1) != scopeType {
		t.Fatalf("Begin inputs = %v; want (context.Context, tenancy.Scope)", method.Type)
	}
	if method.Type.NumOut() != 2 || method.Type.Out(0) != transactionType || method.Type.Out(1) != errorType {
		t.Fatalf("Begin outputs = %v; want (store.Transaction, error)", method.Type)
	}
}

// TestAdapterPortsAreDistinct proves the retained-log sink and live publisher
// are separate seams, and that the Platform seam is a single OAuth-scoped Check
// rather than a concrete vendor protocol.
func TestAdapterPortsAreDistinct(t *testing.T) {
	retained := reflect.TypeOf((*log.RetainedSink)(nil)).Elem()
	live := reflect.TypeOf((*log.LivePublisher)(nil)).Elem()
	if retained.NumMethod() == 0 || live.NumMethod() == 0 {
		t.Fatal("retained/live log ports must not be empty")
	}
	for _, name := range methodNames(retained) {
		if name == "PublishLive" {
			t.Error("retained log sink exposes live publication")
		}
	}
	for _, name := range methodNames(live) {
		if name == "Append" {
			t.Error("live log publisher exposes retained append")
		}
	}

	hosted := reflect.TypeOf((*authorize.HostedAuthority)(nil)).Elem()
	if got := methodNames(hosted); !reflect.DeepEqual(got, []string{"Check"}) {
		t.Fatalf("authorize.HostedAuthority methods = %v; want [Check]", got)
	}
}

func assertContextFirst(t *testing.T, name string, iface reflect.Type) {
	t.Helper()
	if iface.Kind() != reflect.Interface {
		t.Fatalf("%s is not an interface", name)
	}
	if iface.NumMethod() == 0 {
		t.Fatalf("%s has no methods", name)
	}
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	for index := 0; index < iface.NumMethod(); index++ {
		method := iface.Method(index)
		// For an interface type, reflect's method signature omits the
		// receiver, so the first parameter is In(0).
		if method.Type.NumIn() < 1 {
			t.Errorf("%s.%s has no context parameter", name, method.Name)
			continue
		}
		if method.Type.In(0) != contextType {
			t.Errorf("%s.%s first parameter = %v; want context.Context", name, method.Name, method.Type.In(0))
		}
	}
}

func methodNames(iface reflect.Type) []string {
	names := make([]string, 0, iface.NumMethod())
	for index := 0; index < iface.NumMethod(); index++ {
		names = append(names, iface.Method(index).Name)
	}
	sort.Strings(names)
	return names
}

func readTestJSON(t *testing.T, name string, target any) {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

func containsAll(value string, tokens []string) bool {
	lower := strings.ToLower(value)
	for _, token := range tokens {
		if !strings.Contains(lower, strings.ToLower(token)) {
			return false
		}
	}
	return true
}
