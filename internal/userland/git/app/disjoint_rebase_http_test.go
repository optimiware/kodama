package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/git"
	"github.com/optimiware/kodema/internal/userland/git/app/actors"
)

type httpApprovalKey struct {
	cred       string
	resourceID string
	ref        string
	base       string
	candidate  string
	approval   string
}

type httpVerifyRecord struct {
	resourceID string
	operation  string
	subject    api.Subject
}

// httpShellVerifier mirrors Shell /api/v1/verify and /api/v1/report checks,
// including strict subject linkage on Report.
type httpShellVerifier struct {
	mu        sync.Mutex
	approvals map[httpApprovalKey]bool
	records   map[string]httpVerifyRecord
	reports   []api.ReportRequest
	nextID    int
}

func newHTTPShellVerifier() *httpShellVerifier {
	return &httpShellVerifier{
		approvals: make(map[httpApprovalKey]bool),
		records:   make(map[string]httpVerifyRecord),
	}
}

func (v *httpShellVerifier) grant(cred, resourceID, ref, base, candidate, role string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.approvals[httpApprovalKey{
		cred:       cred,
		resourceID: resourceID,
		ref:        ref,
		base:       base,
		candidate:  candidate,
		approval:   "role/" + role,
	}] = true
}

func (v *httpShellVerifier) Verify(_ context.Context, req api.VerifyRequest) (api.VerifyResponse, error) {
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
	case "exec-1-token":
		principal = api.VerifiedPrincipal{ID: "exe-1", Kind: "execution"}
	case "exec-2-token":
		principal = api.VerifiedPrincipal{ID: "exe-2", Kind: "execution"}
	default:
		return api.VerifyResponse{}, fault.New(fault.Unauthenticated, "invalid token")
	}
	approved := false
	if req.Approval != "" {
		if principal.Kind != "execution" {
			return api.VerifyResponse{}, fault.New(fault.Forbidden, "approval evidence requires an execution actor")
		}
		fields := make(map[string]string, len(req.Subject.Fields))
		for _, f := range req.Subject.Fields {
			fields[f.Name] = f.Value
		}
		key := httpApprovalKey{
			cred:       req.ActorCredential,
			resourceID: req.ResourceID,
			ref:        fields["ref"],
			base:       fields["expected_base"],
			candidate:  fields["candidate"],
			approval:   req.Approval,
		}
		if !v.approvals[key] {
			return api.VerifyResponse{}, fault.New(fault.Forbidden, "no matching approval evidence")
		}
		approved = true
	}
	v.nextID++
	verID := fmt.Sprintf("ver-http-%d", v.nextID)
	v.records[verID] = httpVerifyRecord{
		resourceID: req.ResourceID,
		operation:  req.Operation,
		subject:    req.Subject,
	}
	return api.VerifyResponse{
		Principal:      principal,
		Roles:          roles,
		VerificationID: verID,
		Approval:       approved,
	}, nil
}

func (v *httpShellVerifier) Report(_ context.Context, req api.ReportRequest) (api.ReportResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	rec, ok := v.records[req.VerificationID]
	if !ok {
		return api.ReportResponse{}, fault.New(fault.Forbidden, "verification record not found")
	}
	if rec.resourceID != req.ResourceID || rec.operation != req.Operation || !httpSubjectsEqual(rec.subject, req.Subject) {
		return api.ReportResponse{}, fault.New(fault.Forbidden, "report subject does not match the verification record")
	}
	v.reports = append(v.reports, req)
	return api.ReportResponse{
		VerificationID: req.VerificationID,
		EventID:        "evt-" + req.SourceEventID,
	}, nil
}

func (v *httpShellVerifier) reportedFor(ref, candidate string) (api.ReportRequest, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, r := range v.reports {
		fields := httpSubjectMap(r.Subject)
		if fields["ref"] == ref && fields["candidate"] == candidate {
			return r, true
		}
	}
	return api.ReportRequest{}, false
}

func httpSubjectMap(subject api.Subject) map[string]string {
	out := make(map[string]string, len(subject.Fields))
	for _, f := range subject.Fields {
		out[f.Name] = f.Value
	}
	return out
}

func httpSubjectsEqual(a, b api.Subject) bool {
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	am := httpSubjectMap(a)
	bm := httpSubjectMap(b)
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}

func buildRealKodemaHookBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kodema")
	cmd := exec.Command("go", "build", "-o", bin, "../../../../cmd/kodema")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/kodema failed: %v\n%s", err, out)
	}
	return bin
}

func runHTTPGit(t *testing.T, dir, token string, args ...string) (string, error) {
	t.Helper()
	fullArgs := make([]string, 0, len(args)+2)
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("kodema:" + token))
		fullArgs = append(fullArgs, "-c", "http.extraHeader=Authorization: Basic "+basic)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command("git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustHTTPGit(t *testing.T, dir, token string, args ...string) string {
	t.Helper()
	out, err := runHTTPGit(t, dir, token, args...)
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

// TestSmartHTTPDisjointRebasePushOptionsEndToEnd exercises real `git push -o
// kodema.approved_base=... -o kodema.approved_candidate=...` over smart HTTP
// through the real `kodema git hook` binary, Unix callback socket, quarantine
// object store, disjoint-rebase verifier, and post-receive verified reporting.
func TestSmartHTTPDisjointRebasePushOptionsEndToEnd(t *testing.T) {
	hookBin := buildRealKodemaHookBinary(t)
	layout := newProvisionLayout(t)
	tenantCfg := layout.config()
	tenantCfg.HookBinary = hookBin
	mustProvisionTenant(t, tenantCfg)

	// Provision two repositories: one with opt-in disjoint rebase and one strict.
	for _, repo := range []struct {
		name string
		res  string
	}{
		{name: "shop/web", res: "res-shop-web"},
		{name: "shop/strict", res: "res-shop-strict"},
	} {
		cfg := repositoryProvisionConfig(layout, repo.name)
		cfg.ResourceID = repo.res
		cfg.HookBinary = hookBin
		mustProvisionRepository(t, cfg)
	}

	// Simulate an existing pre-upgrade bare repository by unsetting
	// receive.advertisePushOptions in shop/web's local config; HTTPBackend must
	// still advertise push-options automatically via GIT_CONFIG_COUNT.
	webBarePath := filepath.Join(layout.storageRoot, string(provisionTenantID), "shop", "web")
	mustHTTPGit(t, webBarePath, "", "config", "--unset", "receive.advertisePushOptions")

	socketBase, err := os.MkdirTemp("", "k-reb-")
	if err != nil {
		t.Fatalf("MkdirTemp socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketBase) })

	verifier := newHTTPShellVerifier()
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

	// Push updated conf/git.conf to admin enabling option approve-rebase.refs/heads/main = disjoint on shop/web only.
	adminWork := filepath.Join(t.TempDir(), "admin-work")
	mustHTTPGit(t, "", "admin-token", "clone", "--quiet", httpServer.URL+"/admin", adminWork)
	mustHTTPGit(t, adminWork, "", "config", "user.name", "Admin")
	mustHTTPGit(t, adminWork, "", "config", "user.email", "admin@example.invalid")
	confPath := filepath.Join(adminWork, "conf", "git.conf")
	policy := strings.Join([]string{
		"repo admin",
		"    RW = @admin",
		"",
		"repo shop/web",
		"    option resource = res-shop-web",
		"    R = @all",
		"    RW+ refs/heads/work/ = @execution",
		"    RW refs/heads/main = @execution",
		"    option approve.refs/heads/main = reviewer",
		"    option approve-rebase.refs/heads/main = disjoint",
		"",
		"repo shop/strict",
		"    option resource = res-shop-strict",
		"    R = @all",
		"    RW+ refs/heads/work/ = @execution",
		"    RW refs/heads/main = @execution",
		"    option approve.refs/heads/main = reviewer",
		"",
	}, "\n")
	if err := os.WriteFile(confPath, []byte(policy), 0o644); err != nil {
		t.Fatalf("WriteFile git.conf: %v", err)
	}
	mustHTTPGit(t, adminWork, "", "add", "conf/git.conf")
	mustHTTPGit(t, adminWork, "", "commit", "-q", "-m", "enable disjoint rebase on shop/web")
	mustHTTPGit(t, adminWork, "admin-token", "push", "-q", "origin", "HEAD:refs/heads/main")

	// Seed shop/web with initial commit on main.
	webWork := filepath.Join(t.TempDir(), "web-work")
	mustHTTPGit(t, "", "exec-1-token", "clone", "--quiet", httpServer.URL+"/shop/web", webWork)
	mustHTTPGit(t, webWork, "", "config", "user.name", "Exec")
	mustHTTPGit(t, webWork, "", "config", "user.email", "exec@example.invalid")
	if err := os.WriteFile(filepath.Join(webWork, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, webWork, "", "add", "-A")
	mustHTTPGit(t, webWork, "", "commit", "-q", "-m", "seed")
	baseOID := mustHTTPGit(t, webWork, "", "rev-parse", "HEAD")
	verifier.grant("exec-1-token", "res-shop-web", "refs/heads/main", "", baseOID, "reviewer")
	mustHTTPGit(t, webWork, "exec-1-token", "push", "-q", "origin", "HEAD:refs/heads/main")

	// 1. Disjoint multi-commit linear rebase accepted over real smart HTTP with -o push options!
	//    Note: rebased commits are NOT pushed to any work branch first — they exist ONLY in
	//    receive-pack's quarantine during pre-receive!
	mustHTTPGit(t, webWork, "", "checkout", "-q", "-b", "feat-multi")
	if err := os.WriteFile(filepath.Join(webWork, "feat1.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, webWork, "", "add", "-A")
	mustHTTPGit(t, webWork, "", "commit", "-q", "-m", "feat commit 1")
	if err := os.WriteFile(filepath.Join(webWork, "feat2.bin"), []byte{0x00, 0x11, 0x22}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, webWork, "", "add", "-A")
	mustHTTPGit(t, webWork, "", "commit", "-q", "-m", "feat commit 2")
	approvedCand := mustHTTPGit(t, webWork, "", "rev-parse", "HEAD")
	// Push reviewed branch to work/ so approvedCand exists in repo objects (as in normal review workflow).
	mustHTTPGit(t, webWork, "exec-1-token", "push", "-q", "origin", "HEAD:refs/heads/work/feat-multi")
	verifier.grant("exec-1-token", "res-shop-web", "refs/heads/main", baseOID, approvedCand, "reviewer")

	// Meanwhile another execution advances main on a disjoint file.
	mustHTTPGit(t, webWork, "", "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(webWork, "main_other.txt"), []byte("concurrent\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, webWork, "", "add", "-A")
	mustHTTPGit(t, webWork, "", "commit", "-q", "-m", "main concurrent change")
	movedMain := mustHTTPGit(t, webWork, "", "rev-parse", "HEAD")
	verifier.grant("exec-2-token", "res-shop-web", "refs/heads/main", baseOID, movedMain, "reviewer")
	mustHTTPGit(t, webWork, "exec-2-token", "push", "-q", "origin", "HEAD:refs/heads/main")

	// Rebase feat-multi onto movedMain and push DIRECTLY to main with -o push options.
	mustHTTPGit(t, webWork, "", "checkout", "-q", "feat-multi")
	mustHTTPGit(t, webWork, "", "rebase", "-q", "main")
	rebasedOID := mustHTTPGit(t, webWork, "", "rev-parse", "HEAD")
	mustHTTPGit(t, webWork, "exec-1-token", "push",
		"-o", "kodema.approved_base="+baseOID,
		"-o", "kodema.approved_candidate="+approvedCand,
		"origin", "HEAD:refs/heads/main",
	)

	// Confirm remote main updated to rebasedOID AND post-receive reported git.ref.updated with rebasedOID.
	if got := mustHTTPGit(t, webBarePath, "", "rev-parse", "refs/heads/main"); got != rebasedOID {
		t.Fatalf("remote refs/heads/main = %s; want %s", got, rebasedOID)
	}
	report, ok := verifier.reportedFor("refs/heads/main", rebasedOID)
	if !ok {
		t.Fatalf("missing git.ref.updated report for rebased commit %s", rebasedOID)
	}
	fields := httpSubjectMap(report.Subject)
	if fields["expected_base"] != movedMain || fields["candidate"] != rebasedOID {
		t.Fatalf("reported subject = %+v; want expected_base=%s candidate=%s", fields, movedMain, rebasedOID)
	}

	// 2. Overlapping file rebase pushed over HTTP with -o options MUST be refused with clear diagnostic!
	mustHTTPGit(t, webWork, "", "checkout", "-q", "-b", "feat-overlap", rebasedOID)
	if err := os.WriteFile(filepath.Join(webWork, "base.txt"), []byte("base\nfeat edit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, webWork, "", "add", "-A")
	mustHTTPGit(t, webWork, "", "commit", "-q", "-m", "edit base.txt on feat")
	overlapApproved := mustHTTPGit(t, webWork, "", "rev-parse", "HEAD")
	mustHTTPGit(t, webWork, "exec-1-token", "push", "-q", "origin", "HEAD:refs/heads/work/feat-overlap")
	verifier.grant("exec-1-token", "res-shop-web", "refs/heads/main", rebasedOID, overlapApproved, "reviewer")

	// Main also edits base.txt
	mustHTTPGit(t, webWork, "", "checkout", "-q", "-B", "main", rebasedOID)
	if err := os.WriteFile(filepath.Join(webWork, "base.txt"), []byte("main header\nbase\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, webWork, "", "add", "-A")
	mustHTTPGit(t, webWork, "", "commit", "-q", "-m", "edit base.txt on main")
	mainOverlapOID := mustHTTPGit(t, webWork, "", "rev-parse", "HEAD")
	verifier.grant("exec-2-token", "res-shop-web", "refs/heads/main", rebasedOID, mainOverlapOID, "reviewer")
	mustHTTPGit(t, webWork, "exec-2-token", "push", "-q", "origin", "HEAD:refs/heads/main")

	mustHTTPGit(t, webWork, "", "checkout", "-q", "feat-overlap")
	mustHTTPGit(t, webWork, "", "rebase", "-q", "main")
	out, err := runHTTPGit(t, webWork, "exec-1-token", "push",
		"-o", "kodema.approved_base="+rebasedOID,
		"-o", "kodema.approved_candidate="+overlapApproved,
		"origin", "HEAD:refs/heads/main",
	)
	if err == nil {
		t.Fatalf("overlapping rebase push over HTTP succeeded unexpectedly:\n%s", out)
	}
	if !strings.Contains(out, "overlap") || !strings.Contains(out, "fresh review required") {
		t.Fatalf("overlapping push output = %q; want overlap and fresh review required diagnostic", out)
	}
	if got := mustHTTPGit(t, webBarePath, "", "rev-parse", "refs/heads/main"); got != mainOverlapOID {
		t.Fatalf("remote refs/heads/main moved to %s after refusal; want %s", got, mainOverlapOID)
	}

	// 3. Strict repo (without option approve-rebase) refuses disjoint rebase push over HTTP!
	strictWork := filepath.Join(t.TempDir(), "strict-work")
	mustHTTPGit(t, "", "exec-1-token", "clone", "--quiet", httpServer.URL+"/shop/strict", strictWork)
	mustHTTPGit(t, strictWork, "", "config", "user.name", "Exec")
	mustHTTPGit(t, strictWork, "", "config", "user.email", "exec@example.invalid")
	if err := os.WriteFile(filepath.Join(strictWork, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, strictWork, "", "add", "-A")
	mustHTTPGit(t, strictWork, "", "commit", "-q", "-m", "seed")
	strictBase := mustHTTPGit(t, strictWork, "", "rev-parse", "HEAD")
	verifier.grant("exec-1-token", "res-shop-strict", "refs/heads/main", "", strictBase, "reviewer")
	mustHTTPGit(t, strictWork, "exec-1-token", "push", "-q", "origin", "HEAD:refs/heads/main")

	mustHTTPGit(t, strictWork, "", "checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(strictWork, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, strictWork, "", "add", "-A")
	mustHTTPGit(t, strictWork, "", "commit", "-q", "-m", "feat")
	strictApproved := mustHTTPGit(t, strictWork, "", "rev-parse", "HEAD")
	mustHTTPGit(t, strictWork, "exec-1-token", "push", "-q", "origin", "HEAD:refs/heads/work/feat")
	verifier.grant("exec-1-token", "res-shop-strict", "refs/heads/main", strictBase, strictApproved, "reviewer")

	mustHTTPGit(t, strictWork, "", "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(strictWork, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustHTTPGit(t, strictWork, "", "add", "-A")
	mustHTTPGit(t, strictWork, "", "commit", "-q", "-m", "main move")
	strictMoved := mustHTTPGit(t, strictWork, "", "rev-parse", "HEAD")
	verifier.grant("exec-2-token", "res-shop-strict", "refs/heads/main", strictBase, strictMoved, "reviewer")
	mustHTTPGit(t, strictWork, "exec-2-token", "push", "-q", "origin", "HEAD:refs/heads/main")

	mustHTTPGit(t, strictWork, "", "checkout", "-q", "feat")
	mustHTTPGit(t, strictWork, "", "rebase", "-q", "main")
	out, err = runHTTPGit(t, strictWork, "exec-1-token", "push",
		"-o", "kodema.approved_base="+strictBase,
		"-o", "kodema.approved_candidate="+strictApproved,
		"origin", "HEAD:refs/heads/main",
	)
	if err == nil {
		t.Fatalf("strict repo accepted disjoint rebase push unexpectedly:\n%s", out)
	}
	if !strings.Contains(out, "approval") {
		t.Fatalf("strict repo refusal output = %q; want approval diagnostic", out)
	}
}
