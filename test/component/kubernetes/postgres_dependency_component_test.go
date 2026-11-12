//go:build component

package kubernetesconformance

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	kubedeps "github.com/optimiware/kodama/test/support/infrastructure/kubernetes/dependencies"
)

// TestPostgresDependencyClosePreservesSibling runs two PostgreSQL dependency
// fixtures in one owned kind cluster and proves SQL round trips through each
// fixture's Kubernetes Service. Closing the first fixture removes its namespace
// and resources while the sibling keeps answering SQL, so cleanup is scoped to
// the fixture and a failed deletion cannot pass as success.
func TestPostgresDependencyClosePreservesSibling(t *testing.T) {
	t.Parallel()
	fx, ctx := newFixture(t, id.WorkspaceID("workspace-pg-dependency"), fixtureOptions{})

	start := func(name string) *kubedeps.Postgres {
		t.Helper()
		dependency, err := kubedeps.StartPostgres(ctx, fx.Cluster, kubedeps.PostgresOptions{Name: name})
		if err != nil {
			t.Fatalf("start PostgreSQL dependency %s: %v", name, err)
		}
		t.Cleanup(func() {
			if err := dependency.Close(context.Background()); err != nil {
				t.Errorf("close PostgreSQL dependency %s: %v", name, err)
			}
		})
		return dependency
	}
	first := start("pg-dependency-a")
	second := start("pg-dependency-b")
	if first.Namespace() == second.Namespace() {
		t.Fatalf("both fixtures share namespace %q; each fixture must own one namespace", first.Namespace())
	}
	if first.ServiceName() == second.ServiceName() {
		t.Fatalf("both fixtures share Service name %q", first.ServiceName())
	}
	if got, want := first.Endpoint(), first.ServiceHost()+":"+"5432"; got != want {
		t.Fatalf("in-cluster endpoint = %q; want %q", got, want)
	}

	passwordPattern := regexp.MustCompile(`^[0-9a-f]{48}$`)
	namespacePattern := regexp.MustCompile(`^kodama-dep-[0-9a-f]{8}$`)
	for _, dependency := range []*kubedeps.Postgres{first, second} {
		if password := dependency.Connection().Password; !passwordPattern.MatchString(password) {
			t.Fatalf("dependency %s password does not match %s", dependency.Name(), passwordPattern)
		}
		if namespace := dependency.Namespace(); !namespacePattern.MatchString(namespace) {
			t.Fatalf("dependency %s namespace %q does not match %s", dependency.Name(), namespace, namespacePattern)
		}
	}
	if first.Connection().Password == second.Connection().Password {
		t.Fatal("both fixtures share one generated password; each fixture must own its credential")
	}

	for _, dependency := range []*kubedeps.Postgres{first, second} {
		if err := dependency.WaitReady(ctx); err != nil {
			t.Fatalf("dependency %s did not become SQL-ready through its Service: %v", dependency.Name(), err)
		}
		// WaitReady already required a successful SQL query; answer once more so
		// the test itself records a real SQL round trip through the Service.
		if err := dependency.Query(ctx); err != nil {
			t.Fatalf("SQL through dependency %s Service: %v", dependency.Name(), err)
		}
	}

	if exists, err := first.NamespaceExists(ctx); err != nil {
		t.Fatalf("observe first namespace before close: %v", err)
	} else if !exists {
		t.Fatalf("first namespace %q is absent before close", first.Namespace())
	}

	if err := first.Close(ctx); err != nil {
		t.Fatalf("close first dependency: %v", err)
	}
	if exists, err := first.NamespaceExists(ctx); err != nil {
		t.Fatalf("observe first namespace after close: %v", err)
	} else if exists {
		t.Fatalf("first namespace %q survived Close", first.Namespace())
	}
	if _, err := fx.Cluster.GetJSON(ctx, "get", "service", first.ServiceName(), "-n", first.Namespace()); err == nil {
		t.Fatalf("first Service %q survived Close", first.ServiceName())
	}

	if exists, err := second.NamespaceExists(ctx); err != nil {
		t.Fatalf("observe sibling namespace after first close: %v", err)
	} else if !exists {
		t.Fatalf("closing the first fixture deleted the sibling namespace %q", second.Namespace())
	}
	if err := second.Query(ctx); err != nil {
		t.Fatalf("sibling SQL through its Service after first close: %v", err)
	}
}

// TestPostgresDependencyRefusesExistingNamespace proves on a real cluster that a
// fixture never adopts or deletes a namespace it did not create: a pre-existing
// namespace is refused before the apply, survives untouched, and receives none
// of the fixture objects.
func TestPostgresDependencyRefusesExistingNamespace(t *testing.T) {
	t.Parallel()
	fx, ctx := newFixture(t, id.WorkspaceID("workspace-pg-existing"), fixtureOptions{})

	namespaceExists := func() bool {
		t.Helper()
		result, err := fx.Cluster.Kubectl(ctx, "get", "namespace", fx.Namespace, "--ignore-not-found", "-o", "name")
		if err != nil {
			t.Fatalf("observe pre-existing namespace %q: %v", fx.Namespace, err)
		}
		return len(strings.TrimSpace(string(result.Stdout))) != 0
	}
	if !namespaceExists() {
		t.Fatalf("the fixture's baseline namespace %q does not exist; the refusal cannot be exercised", fx.Namespace)
	}

	dependency, err := kubedeps.StartPostgres(ctx, fx.Cluster, kubedeps.PostgresOptions{
		Namespace: fx.Namespace,
		Name:      "pg-existing",
	})
	if err == nil {
		t.Fatalf("StartPostgres adopted pre-existing namespace %q", fx.Namespace)
	}
	if dependency != nil {
		t.Fatalf("StartPostgres returned a fixture for a pre-existing namespace: %+v", dependency)
	}
	if !strings.Contains(err.Error(), "existing namespace") {
		t.Fatalf("StartPostgres error = %v; want an existing-namespace refusal", err)
	}
	if !namespaceExists() {
		t.Fatalf("StartPostgres deleted pre-existing namespace %q", fx.Namespace)
	}
	if _, err := fx.Cluster.GetJSON(ctx, "get", "secret", "pg-existing-credentials", "-n", fx.Namespace); err == nil {
		t.Fatalf("refused fixture still created Secret pg-existing-credentials in namespace %q", fx.Namespace)
	}
	if _, err := fx.Cluster.GetJSON(ctx, "get", "service", "pg-existing", "-n", fx.Namespace); err == nil {
		t.Fatalf("refused fixture still created Service pg-existing in namespace %q", fx.Namespace)
	}
}
