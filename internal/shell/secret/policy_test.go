package secret_test

// Behavior tests for the configuration-backed, deny-by-default NamePolicy.
//
// The policy is trusted installation configuration, never a store table or a
// per-name grant row. It carries exact declared names only: no globs, no
// prefix matching and no implicit widening. These tests pin the allow/deny
// matrix, scope containment, per-consumer isolation, construction validation,
// post-construction immutability, concurrent reads and error hygiene.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func newConfiguredPolicy(t *testing.T, config secret.NamePolicyConfig) *secret.ConfiguredNamePolicy {
	t.Helper()
	policy, err := secret.NewConfiguredNamePolicy(config)
	if err != nil {
		t.Fatalf("NewConfiguredNamePolicy: %v", err)
	}
	if policy == nil {
		t.Fatal("NewConfiguredNamePolicy returned nil")
	}
	return policy
}

func checkAllowed(t *testing.T, policy secret.NamePolicy, scope tenancy.Scope, name id.SecretName) {
	t.Helper()
	if err := policy.Check(context.Background(), scope, name); err != nil {
		t.Fatalf("Check(%+v, %q) = %v; want allow", scope, name, err)
	}
}

func checkDenied(t *testing.T, policy secret.NamePolicy, scope tenancy.Scope, name id.SecretName) {
	t.Helper()
	requireClass(t, policy.Check(context.Background(), scope, name), fault.Forbidden)
}

func TestConfiguredNamePolicyAllowDenyMatrix(t *testing.T) {
	policy := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerWorkload,
		Rules: []secret.NameRule{
			{Scope: tenantScope("tenant-a"), Names: []id.SecretName{"DB_PASSWORD"}},
			{Scope: projectScope("tenant-a", "project-a"), Names: []id.SecretName{"API_KEY"}},
			{Scope: projectScope("tenant-b", "project-b"), Names: []id.SecretName{"OTHER_KEY"}},
		},
	})

	cases := []struct {
		label  string
		scope  tenancy.Scope
		name   id.SecretName
		allows bool
	}{
		{"tenant rule in tenant scope", tenantScope("tenant-a"), "DB_PASSWORD", true},
		{"tenant rule covers its project", projectScope("tenant-a", "project-a"), "DB_PASSWORD", true},
		{"tenant rule covers another project of its tenant", projectScope("tenant-a", "project-z"), "DB_PASSWORD", true},
		{"project rule in its project", projectScope("tenant-a", "project-a"), "API_KEY", true},
		{"project rule in another project", projectScope("tenant-a", "project-b"), "API_KEY", false},
		{"project rule in another tenant", projectScope("tenant-b", "project-b"), "API_KEY", false},
		{"tenant rule in another tenant", projectScope("tenant-b", "project-b"), "DB_PASSWORD", false},
		{"tenant rule in another tenant scope", tenantScope("tenant-b"), "DB_PASSWORD", false},
		{"project rule does not cover tenant scope", tenantScope("tenant-a"), "API_KEY", false},
		{"exact name only", projectScope("tenant-a", "project-a"), "API_KEY_V2", false},
		{"prefix is not a name", projectScope("tenant-a", "project-a"), "API", false},
		{"unknown name", projectScope("tenant-a", "project-a"), "UNKNOWN", false},
		{"unknown tenant", tenantScope("tenant-z"), "DB_PASSWORD", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			if testCase.allows {
				checkAllowed(t, policy, testCase.scope, testCase.name)
				return
			}
			checkDenied(t, policy, testCase.scope, testCase.name)
		})
	}
}

func TestConfiguredNamePolicySeparatesConsumerKinds(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	workload := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerWorkload,
		Rules:    []secret.NameRule{{Scope: scope, Names: []id.SecretName{"DB_PASSWORD"}}},
	})
	gateway := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerGateway,
		Rules:    []secret.NameRule{{Scope: scope, Names: []id.SecretName{"MODEL_API_KEY"}}},
	})
	mcp := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerMCP,
		Rules:    []secret.NameRule{{Scope: scope, Names: []id.SecretName{"MCP_TOKEN"}}},
	})

	if got := workload.Consumer(); got != secret.ConsumerWorkload {
		t.Fatalf("workload consumer = %q; want %q", got, secret.ConsumerWorkload)
	}
	if got := gateway.Consumer(); got != secret.ConsumerGateway {
		t.Fatalf("gateway consumer = %q; want %q", got, secret.ConsumerGateway)
	}
	if got := mcp.Consumer(); got != secret.ConsumerMCP {
		t.Fatalf("mcp consumer = %q; want %q", got, secret.ConsumerMCP)
	}

	checkAllowed(t, workload, scope, "DB_PASSWORD")
	checkDenied(t, workload, scope, "MODEL_API_KEY")
	checkDenied(t, workload, scope, "MCP_TOKEN")

	checkAllowed(t, gateway, scope, "MODEL_API_KEY")
	checkDenied(t, gateway, scope, "DB_PASSWORD")
	checkDenied(t, gateway, scope, "MCP_TOKEN")

	checkAllowed(t, mcp, scope, "MCP_TOKEN")
	checkDenied(t, mcp, scope, "DB_PASSWORD")
	checkDenied(t, mcp, scope, "MODEL_API_KEY")
}

func TestConfiguredNamePolicyEmptyConfigDeniesEverything(t *testing.T) {
	policy := newConfiguredPolicy(t, secret.NamePolicyConfig{})

	if got := policy.Consumer(); got != secret.ConsumerWorkload {
		t.Fatalf("empty config consumer = %q; want the workload default", got)
	}
	for _, scope := range []tenancy.Scope{
		tenantScope("tenant-a"),
		projectScope("tenant-a", "project-a"),
	} {
		checkDenied(t, policy, scope, "DB_PASSWORD")
		checkDenied(t, policy, scope, "ANY_NAME")
	}
}

func TestConfiguredNamePolicyRefusesInvalidConfiguration(t *testing.T) {
	longName := id.SecretName(strings.Repeat("A", id.MaxKeyLength+1))
	cases := []struct {
		label  string
		config secret.NamePolicyConfig
	}{
		{"unknown consumer", secret.NamePolicyConfig{Consumer: "shell"}},
		{"empty tenant scope", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenancy.Scope{}, Names: []id.SecretName{"DB_PASSWORD"}}}}},
		{"invalid project scope", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenancy.Scope{TenantID: "tenant-a", ProjectID: "bad project"}, Names: []id.SecretName{"DB_PASSWORD"}}}}},
		{"empty rule", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenantScope("tenant-a")}}}},
		{"empty name", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenantScope("tenant-a"), Names: []id.SecretName{""}}}}},
		{"lowercase name", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenantScope("tenant-a"), Names: []id.SecretName{"db_password"}}}}},
		{"reserved name", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenantScope("tenant-a"), Names: []id.SecretName{id.SecretTokenName}}}}},
		{"path name", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenantScope("tenant-a"), Names: []id.SecretName{"DB/PASSWORD"}}}}},
		{"oversized name", secret.NamePolicyConfig{Rules: []secret.NameRule{{Scope: tenantScope("tenant-a"), Names: []id.SecretName{longName}}}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			policy, err := secret.NewConfiguredNamePolicy(testCase.config)
			if policy != nil {
				t.Fatalf("policy = %+v; want nil for invalid configuration", policy)
			}
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

func TestConfiguredNamePolicyIsImmutableAfterConstruction(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	names := []id.SecretName{"DB_PASSWORD"}
	rules := []secret.NameRule{{Scope: scope, Names: names}}
	policy := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerWorkload,
		Rules:    rules,
	})

	// Mutate every reachable part of the caller-owned configuration after the
	// policy was built. The policy must retain its construction-time copy.
	names[0] = "API_KEY"
	rules[0].Names = append(rules[0].Names, "OTHER_KEY")
	rules[0].Scope = projectScope("tenant-b", "project-b")

	checkAllowed(t, policy, scope, "DB_PASSWORD")
	checkDenied(t, policy, scope, "API_KEY")
	checkDenied(t, policy, scope, "OTHER_KEY")
	checkDenied(t, policy, projectScope("tenant-b", "project-b"), "DB_PASSWORD")
}

func TestConfiguredNamePolicyConcurrentCheck(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	policy := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerWorkload,
		Rules: []secret.NameRule{
			{Scope: scope, Names: []id.SecretName{"DB_PASSWORD", "API_KEY"}},
		},
	})

	const workers = 16
	const iterations = 200
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for range iterations {
				if err := policy.Check(context.Background(), scope, "DB_PASSWORD"); err != nil {
					t.Errorf("concurrent allowed check = %v; want allow", err)
					return
				}
				if err := policy.Check(context.Background(), scope, "UNKNOWN"); !isForbidden(err) {
					t.Errorf("concurrent denied check = %v; want forbidden", err)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func TestConfiguredNamePolicyErrorsCarryNoSecretMaterial(t *testing.T) {
	const secretValue = "sup3r-s3cret-value"
	scope := projectScope("tenant-a", "project-a")
	policy := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerWorkload,
		Rules:    []secret.NameRule{{Scope: scope, Names: []id.SecretName{"DB_PASSWORD"}}},
	})

	denial := policy.Check(context.Background(), scope, "UNKNOWN")
	requireClass(t, denial, fault.Forbidden)
	if strings.Contains(denial.Error(), secretValue) {
		t.Fatalf("denial error %q leaks secret material", denial.Error())
	}
	if strings.Contains(denial.Error(), "UNKNOWN") || strings.Contains(denial.Error(), "DB_PASSWORD") {
		t.Fatalf("denial error %q echoes a secret name", denial.Error())
	}

	_, construction := secret.NewConfiguredNamePolicy(secret.NamePolicyConfig{
		Rules: []secret.NameRule{{Scope: scope, Names: []id.SecretName{"lowercase-secret"}}},
	})
	requireClass(t, construction, fault.InvalidInput)
	if strings.Contains(construction.Error(), secretValue) {
		t.Fatalf("construction error %q leaks secret material", construction.Error())
	}
}

func TestConfiguredNamePolicyVocabulary(t *testing.T) {
	want := []secret.ConsumerKind{secret.ConsumerWorkload, secret.ConsumerGateway, secret.ConsumerMCP}
	if got := secret.ConsumerKinds(); len(got) != len(want) {
		t.Fatalf("ConsumerKinds() = %v; want %v", got, want)
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("ConsumerKinds() = %v; want %v", got, want)
			}
		}
	}
	for _, kind := range want {
		if !kind.Valid() {
			t.Fatalf("%q.Valid() = false; want true", kind)
		}
	}
	if secret.ConsumerKind("shell").Valid() {
		t.Fatal("unknown consumer kind is valid")
	}
	if secret.ConsumerKind("").Valid() {
		t.Fatal("empty consumer kind is valid")
	}
}

// A tenant-wide rule must authorize the resolver's tenant fallback for a
// project request; otherwise a declared name with only a tenant-wide value
// could never be materialized.
func TestConfiguredNamePolicyAuthorizesResolverTenantFallback(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	tenant := tenantScope("tenant-a")
	name := id.SecretName("DB_PASSWORD")
	policy := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerWorkload,
		Rules:    []secret.NameRule{{Scope: tenant, Names: []id.SecretName{name}}},
	})
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(tenant, name): testStored(tenant, name, "secret-tenant", 7, 1),
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{name: "tenant-secret-value"}}
	resolver, err := secret.NewResolver(secret.ResolverConfig{
		InstallationID: "install-1",
		Policy:         policy,
		Lookup:         lookup,
		Cipher:         cipher,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	resolved, err := resolver.Resolve(context.Background(), resolverRequest(scope, name))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Value != "tenant-secret-value" || resolved[0].Scope != tenant {
		t.Fatalf("resolved = %+v; want the tenant-wide value in the tenant scope", resolved)
	}
}

func TestConfiguredNamePolicyDeniesResolverBeforeLookup(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	tenant := tenantScope("tenant-a")
	name := id.SecretName("DB_PASSWORD")
	policy := newConfiguredPolicy(t, secret.NamePolicyConfig{
		Consumer: secret.ConsumerWorkload,
		Rules:    []secret.NameRule{{Scope: tenant, Names: []id.SecretName{"OTHER_NAME"}}},
	})
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(tenant, name): testStored(tenant, name, "secret-tenant", 7, 1),
	}}
	cipher := &fakeCipher{values: map[id.SecretName]string{name: "must-not-open"}}
	resolver, err := secret.NewResolver(secret.ResolverConfig{
		InstallationID: "install-1",
		Policy:         policy,
		Lookup:         lookup,
		Cipher:         cipher,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

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

func isForbidden(err error) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == fault.Forbidden
}
