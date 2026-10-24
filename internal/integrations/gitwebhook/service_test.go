package gitwebhook_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	"github.com/optimiware/kodama/internal/workspace"
)

func pushMain(f *gitwebhooktest.Fixture, after workspace.Commit) []byte {
	return gitwebhooktest.Marshal(f.T, f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(after)))
}

// assertNothingHappened proves an unauthenticated or malformed request reached
// no collaborator and consumed no delivery identifier.
func assertNothingHappened(t *testing.T, f *gitwebhooktest.Fixture) {
	t.Helper()
	assertNoWork(t, f)
	if n := f.Store.DeliveryCount(); n != 0 {
		t.Fatalf("a delivery identifier was consumed (%d records)", n)
	}
}

// assertNoWork proves no Git read and no run admission happened.
func assertNoWork(t *testing.T, f *gitwebhooktest.Fixture) {
	t.Helper()
	if n := f.Git.CallCount(); n != 0 {
		t.Fatalf("git was called %d times", n)
	}
	if n := f.Runs.Created(); n != 0 || len(f.Runs.Requests) != 0 {
		t.Fatalf("run admission was reached (%d runs, %d requests)", n, len(f.Runs.Requests))
	}
}

func hasAudit(f *gitwebhooktest.Fixture, action, reason string) bool {
	for _, a := range f.Store.Audits() {
		if a.Action == action && a.Reason == reason {
			return true
		}
	}
	return false
}

func TestForgedSignature(t *testing.T) {
	body := func(f *gitwebhooktest.Fixture) []byte { return pushMain(f, gitwebhooktest.CommitA) }
	type tc struct {
		name   string
		reason string
		mutate func(f *gitwebhooktest.Fixture) gw.Notification
	}
	cases := []tc{
		{"wrong secret", gw.ReasonBadSignature, func(f *gitwebhooktest.Fixture) gw.Notification {
			return f.SignWith([]byte(strings.Repeat("x", 40)), f.Trigger.ID, "d1", f.Clock.Now().Unix(), body(f))
		}},
		{"tampered body", gw.ReasonBadSignature, func(f *gitwebhooktest.Fixture) gw.Notification {
			n := f.Sign("d1", body(f))
			n.Body = pushMain(f, gitwebhooktest.CommitB)
			return n
		}},
		{"other delivery id", gw.ReasonBadSignature, func(f *gitwebhooktest.Fixture) gw.Notification {
			n := f.Sign("d1", body(f))
			n.DeliveryID = "d2"
			return n
		}},
		{"signature copied from another timestamp", gw.ReasonBadSignature, func(f *gitwebhooktest.Fixture) gw.Notification {
			n := f.Sign("d1", body(f))
			n.Timestamp = fmt.Sprint(f.Clock.Now().Unix() + 1)
			return n
		}},
		{"empty signature", gw.ReasonMissingCredentials, func(f *gitwebhooktest.Fixture) gw.Notification {
			n := f.Sign("d1", body(f))
			n.Signature = ""
			return n
		}},
		{"bare hex without version", gw.ReasonBadSignature, func(f *gitwebhooktest.Fixture) gw.Notification {
			n := f.Sign("d1", body(f))
			n.Signature = strings.TrimPrefix(n.Signature, "v1=")
			return n
		}},
		{"stale timestamp", gw.ReasonStaleTimestamp, func(f *gitwebhooktest.Fixture) gw.Notification {
			return f.SignWith(f.Secret, f.Trigger.ID, "d1", f.Clock.Now().Add(-gw.MaxClockSkew-time.Second).Unix(), body(f))
		}},
		{"future timestamp", gw.ReasonStaleTimestamp, func(f *gitwebhooktest.Fixture) gw.Notification {
			return f.SignWith(f.Secret, f.Trigger.ID, "d1", f.Clock.Now().Add(gw.MaxClockSkew+time.Second).Unix(), body(f))
		}},
		{"unparsable timestamp", gw.ReasonMissingCredentials, func(f *gitwebhooktest.Fixture) gw.Notification {
			n := f.Sign("d1", body(f))
			n.Timestamp = "yesterday"
			return n
		}},
		{"malformed delivery id", gw.ReasonMissingCredentials, func(f *gitwebhooktest.Fixture) gw.Notification {
			return f.Sign("bad id\n", body(f))
		}},
		{"revoked hook secret", gw.ReasonSecretUnavailable, func(f *gitwebhooktest.Fixture) gw.Notification {
			f.Secrets.Delete(f.Scope, gitwebhooktest.HookSecretRef)
			return f.Sign("d1", body(f))
		}},
		{"disabled trigger", gw.ReasonTriggerDisabled, func(f *gitwebhooktest.Fixture) gw.Notification {
			if _, err := f.Service.SetEnabled(t.Context(), f.Scope, gitwebhooktest.Installer, f.Trigger.ID, false); err != nil {
				t.Fatal(err)
			}
			return f.Sign("d1", body(f))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := gitwebhooktest.NewFixture(t)
			r, err := f.Service.Receive(t.Context(), c.mutate(f))
			if !errors.Is(err, gw.ErrUnauthenticated) || r != (gw.Receipt{}) {
				t.Fatalf("got %+v, %v; want ErrUnauthenticated", r, err)
			}
			assertNothingHappened(t, f)
			if !hasAudit(f, "git_hook.authentication_failed", c.reason) {
				t.Fatalf("no authentication_failed audit with reason %s: %+v", c.reason, f.Store.Audits())
			}
		})
	}

	t.Run("unknown trigger is indistinguishable and unattributed", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.SignWith(f.Secret, "ght_unknown", "d1", f.Clock.Now().Unix(), body(f))
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnauthenticated) {
			t.Fatalf("got %v", err)
		}
		assertNothingHappened(t, f)
	})

	t.Run("a signature for one trigger is not valid for another", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		// A second trigger sharing the very same secret reference.
		second, err := f.Service.Install(t.Context(), gw.InstallCommand{
			Scope: f.Scope, Principal: gitwebhooktest.Installer, DefinitionID: gitwebhooktest.DefinitionName,
			Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef,
		})
		if err != nil {
			t.Fatal(err)
		}
		n := f.Sign("d1", body(f))
		n.TriggerID = second.ID
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnauthenticated) {
			t.Fatalf("replay across triggers: got %v", err)
		}
		assertNothingHappened(t, f)
	})

	t.Run("a secret of another tenant cannot authenticate", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		other := []byte(strings.Repeat("o", 40))
		f.Secrets.Put(f.Other, gitwebhooktest.HookSecretRef, other)
		n := f.SignWith(other, f.Trigger.ID, "d1", f.Clock.Now().Unix(), body(f))
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnauthenticated) {
			t.Fatalf("got %v", err)
		}
		assertNothingHappened(t, f)
	})

	t.Run("a refused forgery does not burn the delivery identifier", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		forged := f.SignWith([]byte(strings.Repeat("x", 40)), f.Trigger.ID, "d1", f.Clock.Now().Unix(), body(f))
		if _, err := f.Service.Receive(t.Context(), forged); !errors.Is(err, gw.ErrUnauthenticated) {
			t.Fatal(err)
		}
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", body(f)))
		if err != nil || r.Outcome != gw.OutcomeStarted {
			t.Fatalf("legitimate delivery after forgery: %+v, %v", r, err)
		}
	})

	t.Run("audit never carries signatures or secrets", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.SignWith([]byte(strings.Repeat("x", 40)), f.Trigger.ID, "d1", f.Clock.Now().Unix(), body(f))
		_, _ = f.Service.Receive(t.Context(), n)
		dump := fmt.Sprintf("%+v", f.Store.Audits())
		for _, forbidden := range []string{n.Signature, strings.TrimPrefix(n.Signature, "v1="), string(f.Secret)} {
			if strings.Contains(dump, forbidden) {
				t.Fatalf("audit leaked %q", forbidden)
			}
		}
	})
}

func TestDuplicateDelivery(t *testing.T) {
	duplicateOutcomes(t)
	t.Run("redelivery reports the recorded result and starts nothing", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		first, err := f.Service.Receive(t.Context(), n)
		if err != nil || first.Outcome != gw.OutcomeStarted || first.Duplicate || first.RunID == "" {
			t.Fatalf("first: %+v, %v", first, err)
		}
		// Redelivered later with a fresh, equally valid signature timestamp.
		f.Clock.Advance(time.Minute)
		again, err := f.Service.Receive(t.Context(), f.Sign("d1", n.Body))
		if err != nil || !again.Duplicate || again.RunID != first.RunID || again.Commit != first.Commit {
			t.Fatalf("duplicate: %+v, %v (first %+v)", again, err, first)
		}
		if f.Runs.Created() != 1 || len(f.Runs.Requests) != 1 || f.Git.CallCount() != 1 {
			t.Fatalf("duplicate repeated work: runs=%d requests=%d git=%d", f.Runs.Created(), len(f.Runs.Requests), f.Git.CallCount())
		}
		if !hasAudit(f, "git_hook.duplicate", gw.ReasonDuplicateDelivery) {
			t.Fatal("duplicate not audited")
		}
	})

	t.Run("same identifier with another payload is a conflict", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		if _, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))); err != nil {
			t.Fatal(err)
		}
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitB)))
		if !errors.Is(err, gw.ErrDeliveryConflict) || r.RunID != "" {
			t.Fatalf("got %+v, %v", r, err)
		}
		if f.Runs.Created() != 1 {
			t.Fatalf("%d runs", f.Runs.Created())
		}
	})

	t.Run("the same identifier is independent per trigger", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		second, err := f.Service.Install(t.Context(), gw.InstallCommand{
			Scope: f.Scope, Principal: gitwebhooktest.Installer, DefinitionID: gitwebhooktest.DefinitionName,
			Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef,
		})
		if err != nil {
			t.Fatal(err)
		}
		body := pushMain(f, gitwebhooktest.CommitA)
		if _, err := f.Service.Receive(t.Context(), f.Sign("d1", body)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Service.Receive(t.Context(), f.SignWith(f.Secret, second.ID, "d1", f.Clock.Now().Unix(), body)); err != nil {
			t.Fatal(err)
		}
		if f.Runs.Created() != 2 {
			t.Fatalf("%d runs", f.Runs.Created())
		}
	})

	t.Run("concurrent identical deliveries create one run", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		var wg sync.WaitGroup
		ids := make(chan domain.RunID, 16)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := f.Service.Receive(t.Context(), n)
				if errors.Is(err, gw.ErrAdmissionBusy) {
					return // fenced out while another processor admits; it retries
				}
				if err != nil {
					t.Error(err)
					return
				}
				ids <- r.RunID
			}()
		}
		wg.Wait()
		close(ids)
		for id := range ids {
			if id != "run-a" {
				t.Fatalf("run %q", id)
			}
		}
		if f.Runs.Created() != 1 {
			t.Fatalf("%d runs", f.Runs.Created())
		}
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.RunID != "run-a" || !r.Duplicate {
			t.Fatalf("retry: %+v, %v", r, err)
		}
	})

	t.Run("an outage leaves the delivery retryable and the retry starts one run", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Runs.SetError(errors.New("admission database down"))
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		rec, _ := f.Store.Delivery(gw.DeliveryKey{TriggerID: f.Trigger.ID, DeliveryID: "d1"})
		if rec.Done {
			t.Fatal("an outage must not finish the delivery")
		}
		f.Runs.SetError(nil)
		r, err := f.Service.Receive(t.Context(), n)
		if err != nil || r.Outcome != gw.OutcomeStarted || r.Duplicate {
			t.Fatalf("retry: %+v, %v", r, err)
		}
		if f.Runs.Created() != 1 {
			t.Fatalf("%d runs", f.Runs.Created())
		}
	})

	t.Run("a store outage before the claim creates nothing", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		f.Store.FailClaims = errors.New("database down")
		if _, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if f.Runs.Created() != 0 || f.Git.CallCount() != 0 {
			t.Fatal("work happened without a durable claim")
		}
	})
}

func TestForeignRepository(t *testing.T) {
	t.Run("a hook attached to another repository starts nothing", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		p := f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA))
		p.Repository = "repo-other"
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", gitwebhooktest.Marshal(t, p)))
		if !errors.Is(err, gw.ErrForeignRepository) || !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonForeignRepository {
			t.Fatalf("got %+v, %v", r, err)
		}
		assertNoWork(t, f)
		if rec, ok := f.Store.Delivery(gw.DeliveryKey{TriggerID: f.Trigger.ID, DeliveryID: "d1"}); !ok || !rec.Done || rec.Outcome != gw.OutcomeRejected {
			t.Fatalf("the refusal was not recorded: %+v", rec)
		}
		if !hasAudit(f, "git_hook.rejected", gw.ReasonForeignRepository) {
			t.Fatal("not audited")
		}
	})

	t.Run("a caller-supplied tenant or project is never authority", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		for _, extra := range []string{`"tenantId":"tenant-b"`, `"projectId":"project-b"`, `"repositoryUrl":"https://evil.example/x.git"`, `"command":"rm -rf /"`} {
			body := fmt.Sprintf(`{"schemaVersion":1,"repository":"repo-main","ref":"refs/heads/main","before":"%s","after":"%s",%s}`,
				gitwebhooktest.Zero, gitwebhooktest.CommitA, extra)
			if _, err := f.Service.Receive(t.Context(), f.Sign("d-"+fmt.Sprint(len(extra)), []byte(body))); !errors.Is(err, gw.ErrInvalid) {
				t.Fatalf("%s: got %v", extra, err)
			}
		}
		assertNothingHappened(t, f)
	})

	t.Run("a repository rebound after installation refuses the old trigger", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		f.Repos.Set(f.Scope, workspace.RepositoryConfig{
			ID: "repo-replacement", URL: "https://git.example.test/org/other.git", CheckoutSecretRef: gitwebhooktest.CheckoutRef,
			PublisherSecretRef: gitwebhooktest.PublisherRef, BaseRef: "main",
		})
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA)))
		if !errors.Is(err, gw.ErrForeignRepository) || r.Reason != gw.ReasonRepositoryRebound {
			t.Fatalf("got %+v, %v", r, err)
		}
		assertNoWork(t, f)
	})

	t.Run("the repository comes from the project and tenant authority", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		// Another tenant installs against its own repository; the first
		// tenant's trigger and secret are invisible to it.
		f.Auth.Grant("user-b", f.Other, gw.ActionInstall)
		f.Defs.Register(f.Other, "def-b")
		f.Repos.Set(f.Other, workspace.RepositoryConfig{ID: "repo-b", URL: "https://git.example.test/b.git", CheckoutSecretRef: "co-b", PublisherSecretRef: "pub-b"})
		_, err := f.Service.Install(t.Context(), gw.InstallCommand{
			Scope: f.Other, Principal: "user-b", DefinitionID: "def-b", Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef,
		})
		if !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a foreign secret reference must not resolve: %v", err)
		}
		if _, err := f.Service.SetEnabled(t.Context(), f.Other, "user-b", f.Trigger.ID, false); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a foreign trigger must look missing: %v", err)
		}
		// A definition of another scope is not registered either.
		f.Auth.Grant(gitwebhooktest.Installer, f.Scope, gw.ActionInstall)
		_, err = f.Service.Install(t.Context(), gw.InstallCommand{
			Scope: f.Scope, Principal: gitwebhooktest.Installer, DefinitionID: "def-b", Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef,
		})
		if !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("a foreign definition must not bind: %v", err)
		}
	})

	t.Run("installation is authenticated and binds a dedicated secret", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		cmd := gw.InstallCommand{
			Scope: f.Scope, Principal: "stranger", DefinitionID: gitwebhooktest.DefinitionName, Branches: []string{"main"}, SecretRef: gitwebhooktest.HookSecretRef,
		}
		if _, err := f.Service.Install(t.Context(), cmd); !errors.Is(err, gw.ErrDenied) {
			t.Fatalf("unauthorized installer: %v", err)
		}
		cmd.Principal = gitwebhooktest.Installer
		for _, ref := range []string{gitwebhooktest.CheckoutRef, gitwebhooktest.PublisherRef} {
			cmd.SecretRef = ref
			if _, err := f.Service.Install(t.Context(), cmd); !errors.Is(err, gw.ErrInvalid) {
				t.Fatalf("a Git credential must not be a hook secret (%s): %v", ref, err)
			}
		}
		f.Secrets.Put(f.Scope, "short", []byte("tiny"))
		cmd.SecretRef = "short"
		if _, err := f.Service.Install(t.Context(), cmd); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("a weak secret: %v", err)
		}
		cmd.SecretRef = gitwebhooktest.HookSecretRef
		cmd.Branches = nil
		if _, err := f.Service.Install(t.Context(), cmd); !errors.Is(err, gw.ErrInvalid) {
			t.Fatalf("an unfiltered trigger: %v", err)
		}
		f.Auth.Err = errors.New("authority outage")
		cmd.Branches = []string{"main"}
		if _, err := f.Service.Install(t.Context(), cmd); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("an authority outage must refuse: %v", err)
		}
	})
}

func TestExactCommitTrigger(t *testing.T) {
	t.Run("the run is pinned to the announced commit when the branch moved on", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		// Main already advanced to C, whose history contains B.
		f.Git.SetBranch(gitwebhooktest.MainRef, workspace.BranchInfo{Tip: gitwebhooktest.CommitC, Exists: true, Contains: true})
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitB)))
		if err != nil || r.Outcome != gw.OutcomeStarted {
			t.Fatalf("got %+v, %v", r, err)
		}
		if len(f.Runs.Requests) != 1 {
			t.Fatalf("%d requests", len(f.Runs.Requests))
		}
		req := f.Runs.Requests[0]
		if req.Commit != gitwebhooktest.CommitB || r.Commit != gitwebhooktest.CommitB {
			t.Fatalf("run commit %s / receipt %s, want %s", req.Commit, r.Commit, gitwebhooktest.CommitB)
		}
		want := gw.RunRequest{
			Scope: f.Scope, DefinitionID: gitwebhooktest.DefinitionName, Commit: gitwebhooktest.CommitB, Ref: gitwebhooktest.MainRef,
			TriggerID: f.Trigger.ID, DeliveryID: "d1", IdempotencyKey: "git-hook:" + string(f.Trigger.ID) + ":d1", RequestedBy: gitwebhooktest.Installer,
			RepositoryID: gitwebhooktest.RepositoryName,
		}
		if fmt.Sprintf("%+v", req) != fmt.Sprintf("%+v", want) {
			t.Fatalf("request %+v, want %+v", req, want)
		}
	})

	t.Run("the remote is read with the checkout credential only", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		if _, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))); err != nil {
			t.Fatal(err)
		}
		if len(f.Git.Calls) != 1 {
			t.Fatalf("%d git calls", len(f.Git.Calls))
		}
		c := f.Git.Calls[0]
		if c.Repo.CheckoutSecretRef != gitwebhooktest.CheckoutRef || c.Repo.PublisherSecretRef != "" || c.Repo.URL != gitwebhooktest.RepositoryURL ||
			c.Repo.Scope != f.Scope || c.Branch != gitwebhooktest.MainRef || c.Commit != gitwebhooktest.CommitA {
			t.Fatalf("unexpected git call %+v", c)
		}
		if got := fmt.Sprint(f.Secrets.Calls); strings.Contains(got, gitwebhooktest.PublisherRef) || strings.Contains(got, gitwebhooktest.CheckoutRef) {
			t.Fatalf("the hook path resolved a Git credential: %s", got)
		}
	})

	t.Run("a rewound branch that no longer holds the commit is refused for good", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		f.Git.SetBranch(gitwebhooktest.MainRef, workspace.BranchInfo{Tip: gitwebhooktest.CommitC, Exists: true, Contains: false})
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitB))
		r, err := f.Service.Receive(t.Context(), n)
		if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonCommitNotOnBranch || r.RunID != "" {
			t.Fatalf("got %+v, %v", r, err)
		}
		if f.Runs.Created() != 0 || len(f.Runs.Requests) != 0 {
			t.Fatal("a run was admitted for an unreachable commit")
		}
		// Even if the branch later reaches the commit, the recorded refusal stands;
		// the later push is a new delivery.
		f.Git.SetBranch(gitwebhooktest.MainRef, workspace.BranchInfo{Tip: gitwebhooktest.CommitB, Exists: true, Contains: true})
		r, err = f.Service.Receive(t.Context(), n)
		if !errors.Is(err, gw.ErrRejected) || !r.Duplicate || f.Git.CallCount() != 1 {
			t.Fatalf("redelivery: %+v, %v, git=%d", r, err, f.Git.CallCount())
		}
	})

	t.Run("a deleted remote branch is refused", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		f.Git.SetBranch(gitwebhooktest.MainRef, workspace.BranchInfo{})
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA)))
		if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonBranchNotFound || f.Runs.Created() != 0 {
			t.Fatalf("got %+v, %v", r, err)
		}
	})

	t.Run("a git outage is retryable and creates no run", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		f.Git.SetError(workspace.ErrUnavailable)
		if _, err := f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if f.Runs.Created() != 0 {
			t.Fatal("run created during a git outage")
		}
		f.Git.SetError(nil)
		if r, err := f.Service.Receive(t.Context(), n); err != nil || r.Outcome != gw.OutcomeStarted {
			t.Fatalf("retry: %+v, %v", r, err)
		}
	})

	t.Run("branch filters ignore without running", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		f.Git.SetBranch("refs/heads/release/1.0", workspace.BranchInfo{Tip: gitwebhooktest.CommitA, Exists: true, Contains: true})
		for i, c := range []struct {
			ref    string
			after  string
			reason string
		}{
			{"refs/heads/feature/x", string(gitwebhooktest.CommitA), gw.ReasonBranchFiltered},
			{"refs/heads/main-evil", string(gitwebhooktest.CommitA), gw.ReasonBranchFiltered},
			{"refs/heads/release", string(gitwebhooktest.CommitA), gw.ReasonBranchFiltered},
			{"refs/tags/v1", string(gitwebhooktest.CommitA), gw.ReasonNotBranch},
			{"refs/heads/main", gitwebhooktest.Zero, gw.ReasonBranchDeleted},
		} {
			r, err := f.Service.Receive(t.Context(), f.Sign(fmt.Sprintf("d%d", i), gitwebhooktest.Marshal(t, f.Payload(c.ref, gitwebhooktest.Zero, c.after))))
			if err != nil || r.Outcome != gw.OutcomeIgnored || r.Reason != c.reason {
				t.Fatalf("%s: got %+v, %v", c.ref, r, err)
			}
		}
		if f.Runs.Created() != 0 || f.Git.CallCount() != 0 {
			t.Fatal("an ignored update reached Git or admission")
		}
		r, err := f.Service.Receive(t.Context(), f.Sign("rel", gitwebhooktest.Marshal(t, f.Payload("refs/heads/release/1.0", gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))))
		if err != nil || r.Outcome != gw.OutcomeStarted {
			t.Fatalf("wildcard match: %+v, %v", r, err)
		}
	})

	t.Run("an unsafe ref is invalid, not executed", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		for i, ref := range []string{"refs/heads/../x", "refs/heads/--upload-pack=sh", "refs/heads/a b", "refs/heads/x.lock", "refs/heads/release/"} {
			_, err := f.Service.Receive(t.Context(), f.Sign(fmt.Sprintf("u%d", i), gitwebhooktest.Marshal(t, f.Payload(ref, gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))))
			if err == nil {
				t.Fatalf("%q was accepted", ref)
			}
		}
		if f.Runs.Created() != 0 || f.Git.CallCount() != 0 {
			t.Fatal("an unsafe ref reached Git or admission")
		}
	})

	t.Run("the installer's authority is checked on every delivery", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		f.Auth.Revoke(gitwebhooktest.Installer, f.Scope, gw.ActionRun)
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA)))
		if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonInstallerDenied {
			t.Fatalf("got %+v, %v", r, err)
		}
		if f.Runs.Created() != 0 || f.Git.CallCount() != 0 {
			t.Fatal("work performed without installer authority")
		}
	})

	t.Run("a permanent admission refusal is recorded; the commit is not retried", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		f.Runs.SetError(fmt.Errorf("%w: definition has a cycle", gw.ErrRunRefused))
		n := f.Sign("d1", pushMain(f, gitwebhooktest.CommitA))
		r, err := f.Service.Receive(t.Context(), n)
		if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonAdmissionRefused {
			t.Fatalf("got %+v, %v", r, err)
		}
		f.Runs.SetError(nil)
		if r, err = f.Service.Receive(t.Context(), n); !errors.Is(err, gw.ErrRejected) || !r.Duplicate {
			t.Fatalf("redelivery: %+v, %v", r, err)
		}
		if f.Runs.Created() != 0 {
			t.Fatal("run created after refusal")
		}
	})

	t.Run("the run and its audit are recorded together", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		r, err := f.Service.Receive(t.Context(), f.Sign("d1", pushMain(f, gitwebhooktest.CommitA)))
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, a := range f.Store.Audits() {
			if a.Action == "git_hook.run_started" {
				found = a.Metadata["run"] == string(r.RunID) && a.Metadata["commit"] == string(gitwebhooktest.CommitA) && a.Scope == f.Scope && a.TriggerID == f.Trigger.ID
			}
		}
		if !found {
			t.Fatalf("missing run_started audit: %+v", f.Store.Audits())
		}
	})
}

func TestServiceRequiresEveryDependency(t *testing.T) {
	if _, err := gw.New(gw.Dependencies{}); err == nil {
		t.Fatal("construction without dependencies must fail")
	}
}

func TestSetEnabledTransitions(t *testing.T) {
	f := gitwebhooktest.NewFixture(t)
	if _, err := f.Service.SetEnabled(t.Context(), f.Scope, gitwebhooktest.Installer, f.Trigger.ID, true); !errors.Is(err, gw.ErrConflict) {
		t.Fatalf("enabling an enabled trigger: %v", err)
	}
	got, err := f.Service.SetEnabled(t.Context(), f.Scope, gitwebhooktest.Installer, f.Trigger.ID, false)
	if err != nil || got.Enabled || got.Version != 2 {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	if _, err := f.Service.SetEnabled(t.Context(), f.Scope, "stranger", f.Trigger.ID, true); !errors.Is(err, gw.ErrDenied) {
		t.Fatalf("unauthorized: %v", err)
	}
	if got, err = f.Service.SetEnabled(t.Context(), f.Scope, gitwebhooktest.Installer, f.Trigger.ID, true); err != nil || !got.Enabled {
		t.Fatalf("enable: %+v, %v", got, err)
	}
}
