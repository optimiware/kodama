package secret_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func newResolver(t *testing.T, policy *fakePolicy, lookup *fakeLookup, cipher *fakeCipher) *secret.SecretResolver {
	t.Helper()
	resolver, err := secret.NewResolver(secret.ResolverConfig{
		InstallationID: "install-1",
		Policy:         policy,
		Lookup:         lookup,
		Cipher:         cipher,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if resolver == nil {
		t.Fatal("NewResolver returned nil")
	}
	return resolver
}

func resolverRequest(scope tenancy.Scope, names ...id.SecretName) secret.ResolveRequest {
	return secret.ResolveRequest{Scope: scope, Names: names}
}

func TestResolverProjectValueOverridesTenantFallback(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	name := id.SecretName("DB_PASSWORD")
	projectStored := testStored(scope, name, "secret-project", 3, 1)
	tenantStored := testStored(tenantScope("tenant-a"), name, "secret-tenant", 7, 1)
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(scope, name):                   projectStored,
		lookupKey(tenantScope("tenant-a"), name): tenantStored,
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{name: "project-secret-value"}}
	policy := &fakePolicy{}
	resolver := newResolver(t, policy, lookup, cipher)

	resolved, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved count = %d; want 1", len(resolved))
	}
	if resolved[0].Name != name || resolved[0].Value != "project-secret-value" || resolved[0].Scope != scope {
		t.Fatalf("resolved = %+v; want the project value in the project scope", resolved[0])
	}

	checks := policy.callSnapshot()
	if len(checks) != 1 || checks[0].scope != scope || checks[0].name != name {
		t.Fatalf("policy checks = %+v; want one check for the declared project name", checks)
	}
	lookups := lookup.callSnapshot()
	if len(lookups) != 1 || lookups[0].scope != scope {
		t.Fatalf("lookup calls = %+v; want only the project row", lookups)
	}
	opens := cipher.openCalls()
	if len(opens) != 1 {
		t.Fatalf("open count = %d; want 1", len(opens))
	}
	wantAssociated := secret.AssociatedData{
		InstallationID: "install-1",
		TenantID:       id.TenantID("tenant-a"),
		ProjectID:      id.ProjectID("project-a"),
		SecretID:       secret.SecretID("secret-project"),
		Name:           name,
		KeyVersion:     secret.KeyVersion(1),
	}
	if opens[0].associated != wantAssociated {
		t.Fatalf("open associated data = %+v; want %+v", opens[0].associated, wantAssociated)
	}
	if !reflect.DeepEqual(opens[0].envelope, projectStored.Envelope) {
		t.Fatalf("opened envelope = %+v; want the project row envelope", opens[0].envelope)
	}
}

func TestResolverFallsBackToTenantOnlyOnAbsence(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	tenant := tenantScope("tenant-a")
	name := id.SecretName("DB_PASSWORD")
	tenantStored := testStored(tenant, name, "secret-tenant", 7, 1)
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(tenant, name): tenantStored,
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{name: "tenant-secret-value"}}
	resolver := newResolver(t, &fakePolicy{}, lookup, cipher)

	resolved, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Value != "tenant-secret-value" || resolved[0].Scope != tenant {
		t.Fatalf("resolved = %+v; want the tenant value in the tenant scope", resolved)
	}
	lookups := lookup.callSnapshot()
	if len(lookups) != 2 {
		t.Fatalf("lookup count = %d; want project then tenant", len(lookups))
	}
	if lookups[0].scope != scope || lookups[1].scope != tenant {
		t.Fatalf("lookup scopes = %+v; want project then tenant", lookups)
	}
	opens := cipher.openCalls()
	if len(opens) != 1 {
		t.Fatalf("open count = %d; want 1", len(opens))
	}
	if opens[0].associated.ProjectID != "" {
		t.Fatalf("tenant fallback associated project = %q; want empty", opens[0].associated.ProjectID)
	}
	if opens[0].associated.SecretID != "secret-tenant" {
		t.Fatalf("tenant fallback associated secret = %q; want secret-tenant", opens[0].associated.SecretID)
	}
}

func TestResolverDeniedNameFailsBeforeLookupAndDecrypt(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	name := id.SecretName("DB_PASSWORD")
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(scope, name): testStored(scope, name, "secret-project", 3, 1),
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{name: "must-not-open"}}
	policy := &fakePolicy{deny: map[id.SecretName]error{name: fault.New(fault.Forbidden, "name is not authorized")}}
	resolver := newResolver(t, policy, lookup, cipher)

	resolved, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
	requireClass(t, err, fault.Forbidden)
	if len(resolved) != 0 {
		t.Fatalf("resolved count = %d; want no values", len(resolved))
	}
	if len(lookup.callSnapshot()) != 0 {
		t.Fatal("a denied name was looked up")
	}
	if len(cipher.openCalls()) != 0 {
		t.Fatal("a denied name was decrypted")
	}
}

func TestResolverDoesNotFallBackOnFailure(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	tenant := tenantScope("tenant-a")
	name := id.SecretName("DB_PASSWORD")

	t.Run("unreadable project row", func(t *testing.T) {
		lookup := &fakeLookup{onLookup: func(requestScope tenancy.Scope, requestName id.SecretName) (secret.Stored, error) {
			if requestScope == scope {
				return secret.Stored{}, fault.New(fault.DependencyUnavailable, "row read failed")
			}
			return testStored(tenant, requestName, "secret-tenant", 7, 1), nil
		}}
		cipher := &fakeCipher{}
		resolver := newResolver(t, &fakePolicy{}, lookup, cipher)
		_, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
		requireClass(t, err, fault.DependencyUnavailable)
		if len(lookup.callSnapshot()) != 1 {
			t.Fatalf("lookup count = %d; want no tenant fallback", len(lookup.callSnapshot()))
		}
		if len(cipher.openCalls()) != 0 {
			t.Fatal("an unreadable row was decrypted")
		}
	})

	t.Run("tampered project envelope", func(t *testing.T) {
		lookup := &fakeLookup{rows: map[string]secret.Stored{
			lookupKey(scope, name):  testStored(scope, name, "secret-project", 3, 1),
			lookupKey(tenant, name): testStored(tenant, name, "secret-tenant", 7, 1),
		}}
		cipher := &fakeCipher{openErr: fault.New(fault.InvalidInput, "envelope authentication failed")}
		resolver := newResolver(t, &fakePolicy{}, lookup, cipher)
		_, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
		requireClass(t, err, fault.InvalidInput)
		if len(lookup.callSnapshot()) != 1 {
			t.Fatalf("lookup count = %d; want no tenant fallback", len(lookup.callSnapshot()))
		}
		if len(cipher.openCalls()) != 1 {
			t.Fatalf("open count = %d; want the project envelope attempted once", len(cipher.openCalls()))
		}
	})

	t.Run("cross-scope project row", func(t *testing.T) {
		other := projectScope("tenant-b", "project-b")
		lookup := &fakeLookup{onLookup: func(requestScope tenancy.Scope, requestName id.SecretName) (secret.Stored, error) {
			if requestScope == scope {
				return testStored(other, requestName, "secret-other", 3, 1), nil
			}
			return testStored(tenant, requestName, "secret-tenant", 7, 1), nil
		}}
		cipher := &fakeCipher{}
		resolver := newResolver(t, &fakePolicy{}, lookup, cipher)
		_, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
		requireClass(t, err, fault.InvalidInput)
		if len(lookup.callSnapshot()) != 1 {
			t.Fatalf("lookup count = %d; want no tenant fallback", len(lookup.callSnapshot()))
		}
		if len(cipher.openCalls()) != 0 {
			t.Fatal("a cross-scope row was decrypted")
		}
	})

	t.Run("missing in project and tenant", func(t *testing.T) {
		lookup := &fakeLookup{rows: map[string]secret.Stored{}}
		cipher := &fakeCipher{}
		resolver := newResolver(t, &fakePolicy{}, lookup, cipher)
		_, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
		requireClass(t, err, fault.NotFound)
		if len(lookup.callSnapshot()) != 2 {
			t.Fatalf("lookup count = %d; want project then tenant", len(lookup.callSnapshot()))
		}
		if len(cipher.openCalls()) != 0 {
			t.Fatal("a missing name was decrypted")
		}
	})
}

func TestResolverAuthorizesBeforeDecryptingEachName(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	first := id.SecretName("DB_PASSWORD")
	second := id.SecretName("API_KEY")
	order := &callLog{}
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(scope, first):  testStored(scope, first, "secret-1", 1, 1),
		lookupKey(scope, second): testStored(scope, second, "secret-2", 1, 1),
	}}
	cipher := &fakeCipher{order: order}
	policy := &fakePolicy{
		order: order,
		deny:  map[id.SecretName]error{second: fault.New(fault.Forbidden, "name is not authorized")},
	}
	resolver := newResolver(t, policy, lookup, cipher)

	_, err := resolver.Resolve(context.Background(), resolverRequest(scope, first, second))
	requireClass(t, err, fault.Forbidden)
	entries := order.snapshot()
	for _, name := range []id.SecretName{first, second} {
		checkIndex := indexOf(entries, "check:"+string(name))
		if checkIndex < 0 {
			t.Fatalf("policy check for %q is absent from %v", name, entries)
		}
		openIndex := indexOf(entries, "open:"+string(name))
		if openIndex >= 0 && openIndex < checkIndex {
			t.Fatalf("name %q was decrypted before it was authorized: %v", name, entries)
		}
	}
	if indexOf(entries, "open:"+string(second)) >= 0 {
		t.Fatalf("denied name %q was decrypted: %v", second, entries)
	}
}

func TestResolverMissingNameFailsWholeRequest(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	present := id.SecretName("DB_PASSWORD")
	absent := id.SecretName("API_KEY")
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(scope, present): testStored(scope, present, "secret-1", 1, 1),
	}}
	cipher := &fakeCipher{}
	resolver := newResolver(t, &fakePolicy{}, lookup, cipher)

	resolved, err := resolver.Resolve(context.Background(), resolverRequest(scope, present, absent))
	requireClass(t, err, fault.NotFound)
	if len(resolved) != 0 {
		t.Fatalf("resolved count = %d; want no partial values", len(resolved))
	}
}

func TestResolverRefusesOversizedResolvedValue(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	name := id.SecretName("DB_PASSWORD")
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(scope, name): testStored(scope, name, "secret-1", 1, 1),
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{
		name: strings.Repeat("x", secret.DefaultBounds.MaxValueBytes+1),
	}}
	resolver := newResolver(t, &fakePolicy{}, lookup, cipher)

	_, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
	requireClass(t, err, fault.InvalidInput)
}

func TestResolverValidatesRequestBeforePorts(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	cases := []struct {
		name    string
		request secret.ResolveRequest
	}{
		{name: "no names", request: secret.ResolveRequest{Scope: scope}},
		{name: "duplicate names", request: secret.ResolveRequest{Scope: scope, Names: []id.SecretName{"DB_PASSWORD", "DB_PASSWORD"}}},
		{name: "invalid name", request: secret.ResolveRequest{Scope: scope, Names: []id.SecretName{"db_password"}}},
		{name: "invalid scope", request: secret.ResolveRequest{Names: []id.SecretName{"DB_PASSWORD"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			policy := &fakePolicy{}
			lookup := &fakeLookup{rows: map[string]secret.Stored{}}
			cipher := &fakeCipher{}
			resolver := newResolver(t, policy, lookup, cipher)
			_, err := resolver.Resolve(context.Background(), testCase.request)
			requireClass(t, err, fault.InvalidInput)
			if len(policy.callSnapshot()) != 0 || len(lookup.callSnapshot()) != 0 || len(cipher.openCalls()) != 0 {
				t.Fatal("an invalid request reached a port")
			}
		})
	}
}

func TestResolverTenantScopedRequestReadsOnlyTenantRow(t *testing.T) {
	tenant := tenantScope("tenant-a")
	name := id.SecretName("DB_PASSWORD")
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(tenant, name): testStored(tenant, name, "secret-tenant", 1, 1),
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{name: "tenant-value"}}
	resolver := newResolver(t, &fakePolicy{}, lookup, cipher)

	resolved, err := resolver.Resolve(context.Background(), resolverRequest(tenant, name))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Scope != tenant {
		t.Fatalf("resolved = %+v; want the tenant row", resolved)
	}
	lookups := lookup.callSnapshot()
	if len(lookups) != 1 || lookups[0].scope != tenant {
		t.Fatalf("lookup calls = %+v; want only the tenant row", lookups)
	}
}

func TestResolverReadsOneSnapshotPerName(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	name := id.SecretName("DB_PASSWORD")
	reads := 0
	lookup := &fakeLookup{onLookup: func(requestScope tenancy.Scope, requestName id.SecretName) (secret.Stored, error) {
		reads++
		stored := testStored(requestScope, requestName, "secret-1", uint64(reads), 1)
		return stored, nil
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{name: "value"}}
	resolver := newResolver(t, &fakePolicy{}, lookup, cipher)

	if _, err := resolver.Resolve(context.Background(), resolverRequest(scope, name)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if reads != 1 {
		t.Fatalf("lookup count = %d; want one snapshot per name", reads)
	}
	opens := cipher.openCalls()
	if len(opens) != 1 || opens[0].associated.SecretID != "secret-1" {
		t.Fatalf("opened associated = %+v; want the first lookup snapshot", opens)
	}
}

func TestResolverKeepsRequestNameOrder(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	first := id.SecretName("B_SECRET")
	second := id.SecretName("A_SECRET")
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(scope, first):  testStored(scope, first, "secret-b", 1, 1),
		lookupKey(scope, second): testStored(scope, second, "secret-a", 1, 1),
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{first: "b-value", second: "a-value"}}
	resolver := newResolver(t, &fakePolicy{}, lookup, cipher)

	resolved, err := resolver.Resolve(context.Background(), resolverRequest(scope, first, second))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 2 || resolved[0].Name != first || resolved[1].Name != second {
		t.Fatalf("resolved order = %+v; want request order", resolved)
	}
	if resolved[0].Value != "b-value" || resolved[1].Value != "a-value" {
		t.Fatalf("resolved values = %+v; want the scoped values", resolved)
	}
}

func indexOf(entries []string, want string) int {
	for index, entry := range entries {
		if entry == want {
			return index
		}
	}
	return -1
}
