package git_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/git"
)

func TestHookPushOptionDecisionIDParsingAndValidation(t *testing.T) {
	baseOID := "1111111111111111111111111111111111111111"
	candOID := "2222222222222222222222222222222222222222"

	runWithOptions := func(t *testing.T, opts []string, actorCred string, principal api.VerifiedPrincipal, approved bool) (git.HookResult, error, []api.VerifyRequest) {
		t.Helper()
		f := newFixtureWithConfig(t, `repo shop/web
  option resource = `+testResourceID+`
  option approve.refs/heads/main = reviewer
  RW refs/heads/main = @execution merge-coordinator
  RW refs/heads/work/ = @execution
`)
		f.executor.setRef("shop/web", "refs/heads/main", baseOID)
		f.executor.setAncestor("shop/web", baseOID, candOID, true)
		f.verifier.script(actorCred, verificationScript{
			principal:      principal,
			approval:       approved,
			verificationID: "ver-dec-1",
		})
		envMap := map[string]string{
			git.EnvActorRef:        "handle-1",
			git.EnvRepository:      "shop/web",
			git.EnvResourceID:      testResourceID,
			git.EnvSourceEvent:     "push-evt-1",
			git.EnvPushOptionCount: itoa(len(opts)),
		}
		for i, opt := range opts {
			envMap[git.EnvPushOptionPrefix+itoa(i)] = opt
		}
		hook, err := git.NewHook(git.HookConfig{
			Service: f.service,
			Actors:  fakeActors{creds: map[string]string{"handle-1": actorCred}},
			Environment: func(key string) (string, bool) {
				v, ok := envMap[key]
				return v, ok
			},
		})
		if err != nil {
			t.Fatalf("NewHook: %v", err)
		}
		stdin := strings.NewReader(baseOID + " " + candOID + " refs/heads/main\n")
		res, err := hook.RunPreReceive(context.Background(), stdin)
		return res, err, f.verifier.verifyRequests()
	}

	t.Run("valid kodema.decision_id forwarded to verify and accepted for execution", func(t *testing.T) {
		res, err, captured := runWithOptions(t, []string{"kodema.decision_id=dec_abcdef123456"}, "exec-cred",
			api.VerifiedPrincipal{ID: "exec-integrate", Kind: "execution"}, true)
		if err != nil || !res.Accepted() {
			t.Fatalf("RunPreReceive = %+v, err=%v; want accepted", res, err)
		}
		if len(captured) != 1 || captured[0].DecisionID != "dec_abcdef123456" || captured[0].Approval != "role/reviewer" {
			t.Fatalf("verify requests = %+v", captured)
		}
	})

	t.Run("identical duplicate kodema.decision_id accepted", func(t *testing.T) {
		res, err, captured := runWithOptions(t, []string{"kodema.decision_id=dec_same", "kodema.decision_id=dec_same"}, "exec-cred",
			api.VerifiedPrincipal{ID: "exec-integrate", Kind: "execution"}, true)
		if err != nil || !res.Accepted() {
			t.Fatalf("RunPreReceive = %+v, err=%v; want accepted", res, err)
		}
		if len(captured) != 1 || captured[0].DecisionID != "dec_same" {
			t.Fatalf("verify requests = %+v", captured)
		}
	})

	t.Run("conflicting duplicate kodema.decision_id rejected", func(t *testing.T) {
		res, err, captured := runWithOptions(t, []string{"kodema.decision_id=dec_1", "kodema.decision_id=dec_2"}, "exec-cred",
			api.VerifiedPrincipal{ID: "exec-integrate", Kind: "execution"}, true)
		if err == nil || res.Accepted() {
			t.Fatalf("expected rejection for conflicting decision_id, got %+v", res)
		}
		if !strings.Contains(res.Message, "conflicting duplicate push option kodema.decision_id") {
			t.Errorf("rejection message = %q", res.Message)
		}
		if len(captured) != 0 {
			t.Fatalf("expected no verify call on malformed push options; got %d", len(captured))
		}
	})

	t.Run("malformed kodema.decision_id rejected", func(t *testing.T) {
		for _, bad := range []string{"kodema.decision_id=", "kodema.decision_id=../evil", "kodema.decision_id=has space"} {
			res, err, captured := runWithOptions(t, []string{bad}, "exec-cred",
				api.VerifiedPrincipal{ID: "exec-integrate", Kind: "execution"}, true)
			if err == nil || res.Accepted() {
				t.Fatalf("expected rejection for %q, got %+v", bad, res)
			}
			if len(captured) != 0 {
				t.Fatalf("expected no verify call for %q; got %d", bad, len(captured))
			}
		}
	})

	t.Run("authorized service account with kodema.decision_id accepted on guarded ref", func(t *testing.T) {
		res, err, captured := runWithOptions(t, []string{"kodema.decision_id=dec_merge_1"}, "svc-cred",
			api.VerifiedPrincipal{ID: "merge-coordinator", Kind: "service"}, true)
		if err != nil || !res.Accepted() {
			t.Fatalf("RunPreReceive(service+decision_id) = %+v, err=%v; want accepted", res, err)
		}
		if len(captured) != 1 || captured[0].DecisionID != "dec_merge_1" {
			t.Fatalf("verify requests = %+v", captured)
		}
	})

	t.Run("service account without kodema.decision_id refused on guarded ref even if verify returned approved", func(t *testing.T) {
		res, err, _ := runWithOptions(t, nil, "svc-cred",
			api.VerifiedPrincipal{ID: "merge-coordinator", Kind: "service"}, true)
		if err == nil || res.Accepted() || !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("RunPreReceive(service without decision_id) = %+v, err=%v; want ErrApprovalRequired", res, err)
		}
	})

	t.Run("human actor with kodema.decision_id refused on guarded ref", func(t *testing.T) {
		res, err, _ := runWithOptions(t, []string{"kodema.decision_id=dec_1"}, "human-cred",
			api.VerifiedPrincipal{ID: "usr-1", Kind: "human"}, true)
		if err == nil || res.Accepted() {
			t.Fatalf("RunPreReceive(human with decision_id) = %+v, err=%v; want rejected", res, err)
		}
	})
}

type fakeActors struct {
	creds map[string]string
}

func (f fakeActors) ResolveActor(_ context.Context, handle string) (string, error) {
	return f.creds[handle], nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
