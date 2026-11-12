//go:build integration

package factorytest

// TestFactoryUsesNoContractSubstitute is the single substitute-removal pin.
//
// It has two independent halves. The source pin proves the factory's Git path
// composes the real public transport, verification client, read verifier,
// actor registry, tenant provisioning and Run entry point, and that none of the
// contract-server/admission fixtures is referenced from the factory or
// this suite's Git path. The behavioural half proves the served Shell actually
// produced a durable verification row for the clone: a fabricated verifier
// could not write the scoped PostgreSQL fact. The representative negative
// runs on the same served fixture: an unapproved protected push is refused
// by the managed hook, the ref does not move, and the durable refused row
// requests role/reviewer.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/factory"
)

// TestFactoryUsesNoContractSubstitute pins both the source composition and the
// durable Shell verification fact.
func TestFactoryUsesNoContractSubstitute(t *testing.T) {
	root := moduleRoot(t)

	// The banned symbols are assembled so this literal file cannot match its
	// own guard; the scan is over every .go file of the two factory paths. The
	// names are the constructors and types the boundaries native/recovery
	// harnesses actually expose, not decorative placeholders.
	banned := []string{
		"New" + "Harness",
		"New" + "SourceEvents",
		"type " + "SourceEvents",
		"New" + "AdmissionVerifier",
		"type " + "AdmissionVerifier",
		"test/support/boundaries/" + "native",
		"test/support/boundaries/" + "recovery",
		"test/support/boundaries/" + "actors",
	}
	directories := []string{
		filepath.Join(root, "test", "support", "factory"),
		filepath.Join(root, "test", "integration", "factory"),
	}
	scanned := 0
	sawIdentityProvider := false
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatalf("read %s: %v", directory, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			scanned++
			text := string(data)
			if strings.Contains(text, "boundaries/identity") || strings.Contains(text, "identityfixture") {
				sawIdentityProvider = true
			}
			for _, forbidden := range banned {
				if strings.Contains(text, forbidden) {
					t.Errorf("%s references the substitute symbol %q", path, forbidden)
				}
			}
		}
	}
	// A scan that found nothing is a broken guard, not a green result.
	if scanned < 20 {
		t.Fatalf("source pin scanned %d files; want the whole factory surface", scanned)
	}
	// The identity package is legitimately present for the real OIDC provider;
	// banning the entire package would be wrong, so the guard above distinguishes
	// its contract-server symbols instead.
	if !sawIdentityProvider {
		t.Fatalf("the source pin did not find the legitimate OIDC identity provider import")
	}

	// Composition pins: the factory path must compose the production entry
	// points rather than reimplement or substitute them.
	gitService, err := os.ReadFile(filepath.Join(root, "test", "support", "factory", "git_service.go"))
	if err != nil {
		t.Fatalf("read git_service.go: %v", err)
	}
	gitRepository, err := os.ReadFile(filepath.Join(root, "test", "support", "factory", "git_repository.go"))
	if err != nil {
		t.Fatalf("read git_repository.go: %v", err)
	}
	composition := string(gitService) + "\n" + string(gitRepository)
	for _, required := range []string{
		"gitapp.NewTransport",
		"gitapp.NewVerificationClient",
		"git.NewReadVerifier",
		"actors.NewRegistry",
		"gitapp.ProvisionTenant",
		"gitapp.Run",
	} {
		if !strings.Contains(composition, required) {
			t.Fatalf("the host Git composition no longer composes %s", required)
		}
	}
	// The accepted policy must come from the served admin repository, never
	// from a policy-file seam. CommittedPolicyFile is the in-repository path the
	// product reads, not a policy-file option.
	if strings.Contains(composition, "PolicyPath") || strings.Contains(composition, "NewPolicyFile") {
		t.Fatalf("the host Git composition reintroduced a policy-file seam")
	}

	// Behavioural half: a clone through the fixture writes a real,
	// scoped verification row for the registered resource and binding.
	fixture := startGitHarness(t)
	fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "substitute-pin")
	read := fixture.waitVerifications(t, "git.read")
	if read.ID == "" || read.ServiceID != fixture.git.ServiceID {
		t.Fatalf("git.read verification = %+v", read)
	}
	if read.ResourceID != fixture.git.ProjectResourceID || read.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("git.read verification was not linked to the registered binding: %+v", read)
	}
	if read.Result != "approved" || read.ActorID != fixture.engineerPrincipal {
		t.Fatalf("git.read verification authority = %+v", read)
	}
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no durable verification row exists after a served clone")
	}

	// The same pin runs the shared protected golden scenario against this
	// fixture. The protected journal's verification ID selects a real scoped
	// PostgreSQL row whose approval, identities and canonical subject are the
	// actual run's; the earlier review action in the same execution succeeded
	// with the authorized human pass and the same subject; and the journal,
	// verification and one linked resource.verified/git.ref.updated event are
	// unchanged after the exact report replay. Each test starts its own
	// fixture, so no assertion depends on another test's ordering or cache.
	evidence := runProtectedGoldenScenario(t, fixture)
	assertProtectedGoldenEvidence(t, evidence)

	// Representative negative on a fresh served composition: a live
	// execution whose test action has completed the real test but whose review
	// has not activated attempts the exact candidate against the protected ref.
	// The managed hook refuses it, the ref does not move, no accepted/reported
	// journal entry or verified event is produced, and the durable refused
	// git.ref.update row requests role/reviewer. The same live credential still
	// pushes the granted work ref. These are served Shell and Git facts, so a
	// substituted verifier or a fabricated refusal cannot satisfy them. The
	// scenario starts its own fixture because its deterministic candidate files
	// are already present in the golden fixture's base commit.
	edgeFixture := startGitHarness(t)
	edge := runProtectedEdgeScenario(t, edgeFixture, protectedEdgeOptions{
		holdBeforeReview:          true,
		stopAfterPreReviewRefusal: true,
	})
	assertProtectedEdgePreReviewRefusal(t, edge)
}

// moduleRoot resolves the repository root from this test file.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve test source path")
	}
	directory := filepath.Dir(current)
	for {
		if info, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil && !info.IsDir() {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("no go.mod above %s", filepath.Dir(current))
		}
		directory = parent
	}
}
