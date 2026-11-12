package git_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	git "github.com/optimiware/kodama/internal/userland/git"
)

// fixedActorHandles always issues the same handle and resolves it to the
// credential, modelling an issuer whose handle is stable across requests.
type fixedActorHandles struct {
	handle     string
	credential string
}

func (handles fixedActorHandles) IssueActor(context.Context, string) (string, error) {
	return handles.handle, nil
}

func (handles fixedActorHandles) ResolveActor(_ context.Context, handle string) (string, error) {
	if handle != handles.handle {
		return "", fmt.Errorf("fixed actor handles: unknown handle")
	}
	return handles.credential, nil
}

func newHookFromEnvironment(t *testing.T, base *fixture, actors git.ActorSource, environment map[string]string) *git.Hook {
	t.Helper()
	hook, err := git.NewHook(git.HookConfig{
		Service: base.service,
		Actors:  actors,
		Environment: func(key string) (string, bool) {
			value, ok := environment[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatalf("NewHook: %v", err)
	}
	return hook
}

func TestSmartHTTPDistinctPushIdentityDefeatsStaleReportedEntry(t *testing.T) {
	base := newFixture(t)
	actors := newFakeActorVerifier()
	actors.set("exec-token", executionIdentity())
	handles := fixedActorHandles{handle: "handle-fixed", credential: "exec-token"}
	backend := newFakeBackend()
	handler, err := git.NewSmartHTTP(git.SmartHTTPConfig{
		Service:      base.service,
		Backend:      backend,
		Actors:       actors,
		ActorHandles: handles,
		ResourceID:   func(string) (string, error) { return testResourceID, nil },
		SourceEvent:  newTestSourceEvents(),
	})
	if err != nil {
		t.Fatalf("NewSmartHTTP: %v", err)
	}
	post := func() map[string]string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/shop/web/git-receive-pack", bytes.NewReader([]byte("commands")))
		request.SetBasicAuth("kodama", "exec-token")
		request.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("receive-pack status = %d; want 200", recorder.Code)
		}
		return envMap(backend.lastCall(t).Environment)
	}

	base.verifier.script("exec-token", executionScript("exe-1"))
	base.executor.setRef("shop/web", testRef, hookBase)
	base.executor.setAncestor("shop/web", hookBase, hookCandidate, true)
	stdin := hookBase + " " + hookCandidate + " " + testRef + "\n"

	firstEnv := post()
	firstHook := newHookFromEnvironment(t, base, handles, firstEnv)
	if result, err := firstHook.RunPreReceive(context.Background(), strings.NewReader(stdin)); err != nil || !result.Accepted() {
		t.Fatalf("first pre-receive = %+v, %v; want accept", result, err)
	}
	base.executor.setRef("shop/web", testRef, hookCandidate)
	if result, err := firstHook.RunPostReceive(context.Background(), strings.NewReader(stdin)); err != nil || !result.Accepted() {
		t.Fatalf("first post-receive = %+v, %v; want accept", result, err)
	}
	if calls := len(base.verifier.verifyRequests()); calls != 1 {
		t.Fatalf("first push verification calls = %d; want 1", calls)
	}

	// Reset the ref and revoke the credential. The same actor handle issues the
	// second push, but the handler must mint a new push identity.
	base.executor.setRef("shop/web", testRef, hookBase)
	base.verifier.fail(errors.New("credential revoked"))
	secondEnv := post()
	if firstEnv[git.EnvSourceEvent] == "" || secondEnv[git.EnvSourceEvent] == "" {
		t.Fatalf("backend environment lacks a push identity: %+v / %+v", firstEnv, secondEnv)
	}
	if firstEnv[git.EnvSourceEvent] == secondEnv[git.EnvSourceEvent] {
		t.Fatal("the handler reused a push identity for a second request")
	}
	secondHook := newHookFromEnvironment(t, base, handles, secondEnv)
	result, err := secondHook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err == nil {
		t.Fatal("second identical push was accepted from the stale reported entry")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("second pre-receive = %+v; want reject/1", result)
	}
	if calls := len(base.verifier.verifyRequests()); calls != 2 {
		t.Fatalf("verification calls = %d; want the second push verified afresh", calls)
	}
	if got := base.executor.commit("shop/web", testRef); got != hookBase {
		t.Fatalf("ref = %q; want the reset base", got)
	}
}

func TestSmartHTTPRejectsInvalidSourceEvent(t *testing.T) {
	base := newFixture(t)
	actors := newFakeActorVerifier()
	actors.set("exec-token", executionIdentity())
	backend := newFakeBackend()
	cases := []func() (string, error){
		func() (string, error) { return "", errors.New("entropy unavailable") },
		func() (string, error) { return "not a journal id", nil },
	}
	for index, source := range cases {
		handler, err := git.NewSmartHTTP(git.SmartHTTPConfig{
			Service:      base.service,
			Backend:      backend,
			Actors:       actors,
			ActorHandles: newFakeActorHandles(),
			ResourceID:   func(string) (string, error) { return testResourceID, nil },
			SourceEvent:  source,
		})
		if err != nil {
			t.Fatalf("case %d: NewSmartHTTP: %v", index, err)
		}
		request := httptest.NewRequest(http.MethodPost, "/shop/web/git-receive-pack", bytes.NewReader([]byte("commands")))
		request.SetBasicAuth("kodama", "exec-token")
		request.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("case %d: status = %d; want 500", index, recorder.Code)
		}
	}
	if backend.callCount() != 0 {
		t.Fatal("an invalid push identity reached the backend")
	}
}
