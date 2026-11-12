package git_test

import (
	"context"
	"fmt"
	"sync"

	git "github.com/optimiware/kodama/internal/userland/git"
)

// fakeActorVerifier authenticates a forwarded credential to the current actor
// identity. It never stores or echoes the credential in a response.
type fakeActorVerifier struct {
	mu           sync.Mutex
	identities   map[string]git.Identity
	calls        int
	repositories []string
}

func newFakeActorVerifier() *fakeActorVerifier {
	return &fakeActorVerifier{identities: map[string]git.Identity{}}
}

func (verifier *fakeActorVerifier) set(credential string, identity git.Identity) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.identities[credential] = identity
}

func (verifier *fakeActorVerifier) VerifyReadActor(_ context.Context, repository, credential string) (git.Identity, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.calls++
	verifier.repositories = append(verifier.repositories, repository)
	identity, ok := verifier.identities[credential]
	if !ok {
		return git.Identity{}, fmt.Errorf("fake actor verifier: unknown credential")
	}
	return identity, nil
}

func (verifier *fakeActorVerifier) callCount() int {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return verifier.calls
}

func (verifier *fakeActorVerifier) verifiedRepositories() []string {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return append([]string(nil), verifier.repositories...)
}

// fail makes the existing verification/report fake return a dependency error.
func (verifier *fakeVerifier) fail(err error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.verifyErr = err
}

// fakeActorHandles issues and resolves opaque backend actor handles. The raw
// credential never leaves the HTTP handler through the environment: only the
// handle is placed in the backend environment.
type fakeActorHandles struct {
	mu         sync.Mutex
	handles    map[string]string
	next       int
	issueErr   error
	resolveErr error
}

func newFakeActorHandles() *fakeActorHandles {
	return &fakeActorHandles{handles: map[string]string{}}
}

func (handles *fakeActorHandles) IssueActor(_ context.Context, credential string) (string, error) {
	handles.mu.Lock()
	defer handles.mu.Unlock()
	if handles.issueErr != nil {
		return "", handles.issueErr
	}
	handles.next++
	handle := fmt.Sprintf("handle-%d", handles.next)
	handles.handles[handle] = credential
	return handle, nil
}

func (handles *fakeActorHandles) ResolveActor(_ context.Context, handle string) (string, error) {
	handles.mu.Lock()
	defer handles.mu.Unlock()
	if handles.resolveErr != nil {
		return "", handles.resolveErr
	}
	credential, ok := handles.handles[handle]
	if !ok {
		return "", fmt.Errorf("fake actor handles: unknown handle")
	}
	return credential, nil
}
