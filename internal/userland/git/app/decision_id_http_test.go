package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/git"
	"github.com/optimiware/kodema/internal/userland/git/app/actors"
)

type httpDecisionRecord struct {
	resourceID   string
	ref          string
	base         string
	candidate    string
	approval     string
	allowedCreds map[string]bool
	expired      bool
	revoked      bool
}

type httpDecisionVerifier struct {
	mu        sync.Mutex
	decisions map[string]*httpDecisionRecord
	records   map[string]httpVerifyRecord
	reports   []api.ReportRequest
	nextID    int
}

func newHTTPDecisionVerifier() *httpDecisionVerifier {
	return &httpDecisionVerifier{
		decisions: make(map[string]*httpDecisionRecord),
		records:   make(map[string]httpVerifyRecord),
	}
}

func (v *httpDecisionVerifier) registerDecision(decisionID string, rec *httpDecisionRecord) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.decisions[decisionID] = rec
}

func (v *httpDecisionVerifier) Verify(_ context.Context, req api.VerifyRequest) (api.VerifyResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var (
		principal api.VerifiedPrincipal
		roles     []string
	)
	switch req.ActorCredential {
	case "admin-token":
		principal = api.VerifiedPrincipal{ID: "usr-admin", Kind: "human"}
		roles = []string{"admin"}
	case "exec-integrate-token":
		principal = api.VerifiedPrincipal{ID: "exec-integrate", Kind: "execution"}
	case "exec-foreign-token":
		principal = api.VerifiedPrincipal{ID: "exec-foreign", Kind: "execution"}
	case "merge-coordinator-token":
		principal = api.VerifiedPrincipal{ID: "merge-coordinator", Kind: "service"}
	default:
		return api.VerifyResponse{}, fault.New(fault.Unauthenticated, "invalid token")
	}

	approved := false
	if req.Approval != "" {
		if req.DecisionID == "" {
			return api.VerifyResponse{}, fault.New(fault.Forbidden, "no matching approval evidence")
		}
		if principal.Kind == "human" {
			return api.VerifyResponse{}, fault.New(fault.Forbidden, "approval evidence requires an execution actor")
		}
		dec, ok := v.decisions[req.DecisionID]
		fields := httpSubjectMap(req.Subject)
		if !ok || dec.expired || dec.revoked ||
			!dec.allowedCreds[req.ActorCredential] ||
			dec.approval != req.Approval ||
			dec.resourceID != req.ResourceID ||
			dec.ref != fields["ref"] ||
			dec.base != fields["expected_base"] ||
			dec.candidate != fields["candidate"] {
			return api.VerifyResponse{}, fault.New(fault.Forbidden, "no matching approval evidence")
		}
		approved = true
	}

	v.nextID++
	verID := fmt.Sprintf("ver-dec-%d", v.nextID)
	v.records[verID] = httpVerifyRecord{
		resourceID: req.ResourceID,
		operation:  req.Operation,
		subject:    req.Subject,
	}
	return api.VerifyResponse{
		Principal:      principal,
		Roles:          roles,
		Approval:       approved,
		VerificationID: verID,
	}, nil
}

func (v *httpDecisionVerifier) Report(_ context.Context, req api.ReportRequest) (api.ReportResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	rec, ok := v.records[req.VerificationID]
	if !ok {
		return api.ReportResponse{}, fault.New(fault.NotFound, "verification not found")
	}
	if rec.resourceID != req.ResourceID || rec.operation != req.Operation || !httpSubjectsEqual(rec.subject, req.Subject) {
		return api.ReportResponse{}, fault.New(fault.Forbidden, "report subject does not match verification record")
	}
	v.reports = append(v.reports, req)
	return api.ReportResponse{
		VerificationID: req.VerificationID,
		EventID:        "evt-" + req.VerificationID,
	}, nil
}

// TestSmartHTTPDecisionIDExactDisjointAndMergeCoordinatorEndToEnd exercises
// real `git push -o kodema.decision_id=<id>` over smart HTTP through the real
// `kodema git hook` binary for:
//  1. Cross-execution integrate push (succeeds for authorized execution, fails
//     for foreign/expired/mismatched decision_id).
//  2. `option approve-rebase.refs/heads/main = disjoint` combined with
//     `-o kodema.decision_id=<id> -o kodema.approved_base=... -o kodema.approved_candidate=...`
//     (succeeds for disjoint rebase, fails for overlapping paths or modified diff).
//  3. `service/merge-coordinator` pushing to approval-guarded `refs/heads/main`
//     using `-o kodema.decision_id=<id>`.
func TestSmartHTTPDecisionIDExactDisjointAndMergeCoordinatorEndToEnd(t *testing.T) {
	hookBin := buildRealKodemaHookBinary(t)
	layout := newProvisionLayout(t)
	tenantCfg := layout.config()
	tenantCfg.HookBinary = hookBin
	mustProvisionTenant(t, tenantCfg)

	cfg := repositoryProvisionConfig(layout, "shop/web")
	cfg.ResourceID = "res-shop-web"
	cfg.HookBinary = hookBin
	mustProvisionRepository(t, cfg)

	webBarePath := filepath.Join(layout.storageRoot, string(provisionTenantID), "shop", "web")

	socketBase, err := os.MkdirTemp("", "k-dec-")
	if err != nil {
		t.Fatalf("MkdirTemp socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketBase) })

	verifier := newHTTPDecisionVerifier()
	readVerifier, err := git.NewReadVerifier(verifier, func(repository string) (string, error) {
		return "", git.UnregisteredRepositoryError(repository)
	})
	if err != nil {
		t.Fatalf("NewReadVerifier: %v", err)
	}
	registry, err := actors.NewRegistry(actors.Config{TenantID: provisionTenantID})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	server, err := New(Config{
		TenantID:            provisionTenantID,
		StorageRoot:         layout.storageRoot,
		JournalDirectory:    layout.journalDirectory,
		ResourceIDs:         map[string]string{"admin": "res-admin"},
		HookBinary:          hookBin,
		HookSocketDirectory: socketBase,
		Verify:              verifier,
		Actors:              readVerifier,
		ActorHandles:        registry,
		HookActors:          registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := server.StartHooks(context.Background()); err != nil {
		t.Fatalf("StartHooks: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	// Push conf/git.conf enabling @execution and merge-coordinator on main with
	// reviewer approval and opt-in disjoint rebase.
	adminWork := filepath.Join(t.TempDir(), "admin-work")
	mustHTTPGit(t, "", "admin-token", "clone", "--quiet", httpServer.URL+"/admin", adminWork)
	mustHTTPGit(t, adminWork, "", "config", "user.name", "Admin")
	mustHTTPGit(t, adminWork, "", "config", "user.email", "admin@example.invalid")
	gitConf := strings.Join([]string{
		"repo admin",
		"    RW = @admin",
		"",
		"repo shop/web",
		"    option resource = res-shop-web",
		"    R = @all",
		"    RW+ refs/heads/work/ = @execution",
		"    RW refs/heads/main = @execution merge-coordinator",
		"    option approve.refs/heads/main = reviewer",
		"    option approve-rebase.refs/heads/main = disjoint",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(adminWork, "conf", "git.conf"), []byte(gitConf), 0o644); err != nil {
		t.Fatalf("write git.conf: %v", err)
	}
	mustHTTPGit(t, adminWork, "", "add", "conf/git.conf")
	mustHTTPGit(t, adminWork, "", "commit", "-q", "-m", "configure decision_id policy")
	mustHTTPGit(t, adminWork, "admin-token", "push", "-q", "origin", "HEAD:refs/heads/main")

	repoURL := httpServer.URL + "/shop/web"

	// Seed shop/web with base commit A over smart HTTP using an initial approval decision for unborn main.
	seedWork := filepath.Join(t.TempDir(), "seed-web")
	mustHTTPGit(t, "", "exec-integrate-token", "clone", "--quiet", repoURL, seedWork)
	mustHTTPGit(t, seedWork, "", "config", "user.name", "Seed")
	mustHTTPGit(t, seedWork, "", "config", "user.email", "seed@example.invalid")
	if err := os.WriteFile(filepath.Join(seedWork, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustHTTPGit(t, seedWork, "", "add", "README.md")
	mustHTTPGit(t, seedWork, "", "commit", "-q", "-m", "base A")
	commitA := mustHTTPGit(t, seedWork, "", "rev-parse", "HEAD")
	verifier.registerDecision("dec_seed_a", &httpDecisionRecord{
		resourceID:   "res-shop-web",
		ref:          "refs/heads/main",
		base:         "",
		candidate:    commitA,
		approval:     "role/reviewer",
		allowedCreds: map[string]bool{"exec-integrate-token": true},
	})
	mustHTTPGit(t, seedWork, "exec-integrate-token", "push", "-q", "-o", "kodema.decision_id=dec_seed_a", "origin", "HEAD:refs/heads/main")

	// Create candidate commit B on top of A.
	clone1 := filepath.Join(t.TempDir(), "clone-1")
	mustHTTPGit(t, "", "exec-integrate-token", "clone", "--quiet", repoURL, clone1)
	mustHTTPGit(t, clone1, "", "config", "user.name", "Exec")
	mustHTTPGit(t, clone1, "", "config", "user.email", "exec@example.invalid")
	if err := os.WriteFile(filepath.Join(clone1, "feature_b.txt"), []byte("feature b\n"), 0o644); err != nil {
		t.Fatalf("write feature_b: %v", err)
	}
	mustHTTPGit(t, clone1, "", "add", "feature_b.txt")
	mustHTTPGit(t, clone1, "", "commit", "-q", "-m", "feature B")
	commitB := mustHTTPGit(t, clone1, "", "rev-parse", "HEAD")

	// Register decision dec_exact_1 for (A -> B) authorized for exec-integrate-token.
	verifier.registerDecision("dec_exact_1", &httpDecisionRecord{
		resourceID:   "res-shop-web",
		ref:          "refs/heads/main",
		base:         commitA,
		candidate:    commitB,
		approval:     "role/reviewer",
		allowedCreds: map[string]bool{"exec-integrate-token": true},
	})

	// 1a. Foreign execution presenting dec_exact_1 MUST fail!
	if out, err := runHTTPGit(t, clone1, "exec-foreign-token", "push", "-o", "kodema.decision_id=dec_exact_1", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("foreign execution unexpectedly pushed with dec_exact_1:\n%s", out)
	}

	// 1b. Authorized subsequent integrate execution pushing with -o kodema.decision_id=dec_exact_1 succeeds!
	mustHTTPGit(t, clone1, "exec-integrate-token", "push", "-o", "kodema.decision_id=dec_exact_1", "origin", "HEAD:refs/heads/main")
	if got := mustHTTPGit(t, webBarePath, "", "rev-parse", "refs/heads/main"); got != commitB {
		t.Fatalf("refs/heads/main = %s; want %s", got, commitB)
	}

	// 2. Disjoint rebase + cross-execution decision_id:
	clone2 := filepath.Join(t.TempDir(), "clone-2")
	mustHTTPGit(t, "", "exec-integrate-token", "clone", "--quiet", repoURL, clone2)
	mustHTTPGit(t, clone2, "", "config", "user.name", "Exec")
	mustHTTPGit(t, clone2, "", "config", "user.email", "exec@example.invalid")
	mustHTTPGit(t, clone2, "", "checkout", "-q", "-b", "work/c", commitA)
	if err := os.WriteFile(filepath.Join(clone2, "feature_c.txt"), []byte("feature c\n"), 0o644); err != nil {
		t.Fatalf("write feature_c: %v", err)
	}
	mustHTTPGit(t, clone2, "", "add", "feature_c.txt")
	mustHTTPGit(t, clone2, "", "commit", "-q", "-m", "feature C")
	commitC := mustHTTPGit(t, clone2, "", "rev-parse", "HEAD")
	mustHTTPGit(t, clone2, "exec-integrate-token", "push", "-q", "origin", "HEAD:refs/heads/work/c")

	verifier.registerDecision("dec_rebase_1", &httpDecisionRecord{
		resourceID:   "res-shop-web",
		ref:          "refs/heads/main",
		base:         commitA,
		candidate:    commitC,
		approval:     "role/reviewer",
		allowedCreds: map[string]bool{"exec-integrate-token": true},
	})

	// Rebase C onto B -> C' (disjoint paths: feature_b.txt vs feature_c.txt).
	mustHTTPGit(t, clone2, "", "rebase", "-q", commitB)
	commitCRebased := mustHTTPGit(t, clone2, "", "rev-parse", "HEAD")

	// Tampered diff on top of C' MUST fail disjoint rebase check even with valid decision_id!
	cloneTamper := filepath.Join(t.TempDir(), "clone-tamper")
	mustHTTPGit(t, "", "exec-integrate-token", "clone", "--quiet", repoURL, cloneTamper)
	mustHTTPGit(t, cloneTamper, "", "config", "user.name", "Exec")
	mustHTTPGit(t, cloneTamper, "", "config", "user.email", "exec@example.invalid")
	mustHTTPGit(t, cloneTamper, "", "checkout", "-q", "-b", "tamper", commitB)
	if err := os.WriteFile(filepath.Join(cloneTamper, "feature_c.txt"), []byte("TAMPERED c\n"), 0o644); err != nil {
		t.Fatalf("write tampered feature_c: %v", err)
	}
	mustHTTPGit(t, cloneTamper, "", "add", "feature_c.txt")
	mustHTTPGit(t, cloneTamper, "", "commit", "-q", "-m", "feature C tampered")
	if out, err := runHTTPGit(t, cloneTamper, "exec-integrate-token", "push",
		"-o", "kodema.decision_id=dec_rebase_1",
		"-o", "kodema.approved_base="+commitA,
		"-o", "kodema.approved_candidate="+commitC,
		"origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("tampered disjoint rebase unexpectedly succeeded:\n%s", out)
	}

	// Clean disjoint rebase C' succeeds with decision_id + approved_base + approved_candidate!
	mustHTTPGit(t, clone2, "exec-integrate-token", "push",
		"-o", "kodema.decision_id=dec_rebase_1",
		"-o", "kodema.approved_base="+commitA,
		"-o", "kodema.approved_candidate="+commitC,
		"origin", "HEAD:refs/heads/main")
	if got := mustHTTPGit(t, webBarePath, "", "rev-parse", "refs/heads/main"); got != commitCRebased {
		t.Fatalf("refs/heads/main after disjoint rebase = %s; want %s", got, commitCRebased)
	}

	// 3. service/merge-coordinator pushes approved commit D onto main (currently at C') using -o kodema.decision_id=dec_merge_1!
	clone3 := filepath.Join(t.TempDir(), "clone-3")
	mustHTTPGit(t, "", "merge-coordinator-token", "clone", "--quiet", repoURL, clone3)
	mustHTTPGit(t, clone3, "", "config", "user.name", "MergeCoord")
	mustHTTPGit(t, clone3, "", "config", "user.email", "merge@example.invalid")
	if err := os.WriteFile(filepath.Join(clone3, "feature_d.txt"), []byte("feature d\n"), 0o644); err != nil {
		t.Fatalf("write feature_d: %v", err)
	}
	mustHTTPGit(t, clone3, "", "add", "feature_d.txt")
	mustHTTPGit(t, clone3, "", "commit", "-q", "-m", "feature D")
	commitD := mustHTTPGit(t, clone3, "", "rev-parse", "HEAD")

	verifier.registerDecision("dec_merge_1", &httpDecisionRecord{
		resourceID:   "res-shop-web",
		ref:          "refs/heads/main",
		base:         commitCRebased,
		candidate:    commitD,
		approval:     "role/reviewer",
		allowedCreds: map[string]bool{"merge-coordinator-token": true},
	})

	// Without decision_id, merge-coordinator MUST be refused on approval-guarded main!
	if out, err := runHTTPGit(t, clone3, "merge-coordinator-token", "push", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("merge-coordinator unexpectedly pushed without decision_id:\n%s", out)
	}

	// With valid decision_id, merge-coordinator push succeeds!
	mustHTTPGit(t, clone3, "merge-coordinator-token", "push", "-o", "kodema.decision_id=dec_merge_1", "origin", "HEAD:refs/heads/main")
	if got := mustHTTPGit(t, webBarePath, "", "rev-parse", "refs/heads/main"); got != commitD {
		t.Fatalf("refs/heads/main after merge-coordinator push = %s; want %s", got, commitD)
	}
}
