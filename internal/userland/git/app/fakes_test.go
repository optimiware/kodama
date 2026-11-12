package app

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

// fakeStorage resolves repository paths inside one temporary root.
type fakeStorage struct {
	root string
}

func (storage fakeStorage) RepositoryPath(tenant id.TenantID, repository string) (string, error) {
	if tenant == "" || repository == "" {
		return "", git.ErrInvalidRepository
	}
	return storage.root + "/" + string(tenant) + "/" + repository, nil
}

func (storage fakeStorage) EnsureRepository(tenant id.TenantID, repository string) (git.RepositoryLocation, error) {
	path, err := storage.RepositoryPath(tenant, repository)
	return git.RepositoryLocation{Path: path}, err
}

// fakeExecutor records commands and returns a scripted result.
type fakeExecutor struct {
	mu      sync.Mutex
	command git.Command
	result  git.CommandResult
	err     error
}

func (executor *fakeExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.command = command
	return executor.result, executor.err
}

func (executor *fakeExecutor) last() git.Command {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.command
}

// fakeVerification implements git.VerificationClient.
type fakeVerification struct {
	verifyRequest  api.VerifyRequest
	reportRequest  api.ReportRequest
	verifyResponse api.VerifyResponse
	reportResponse api.ReportResponse
	err            error
}

func (verification *fakeVerification) Verify(_ context.Context, request api.VerifyRequest) (api.VerifyResponse, error) {
	verification.verifyRequest = request
	return verification.verifyResponse, verification.err
}

func (verification *fakeVerification) Report(_ context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	verification.reportRequest = request
	return verification.reportResponse, verification.err
}

// fakeRetention implements git.RetentionQuery.
type fakeRetention struct {
	commits []string
	err     error
}

func (retention *fakeRetention) ReferencedCommits(_ context.Context, _ api.Page) (git.CommitRefs, error) {
	return git.CommitRefs{Commits: retention.commits}, retention.err
}

// fakeActors implements git.ReadActorVerifier.
type fakeActors struct {
	identity git.Identity
	err      error
}

func (actors *fakeActors) VerifyReadActor(_ context.Context, _, _ string) (git.Identity, error) {
	return actors.identity, actors.err
}

// fakeHandles implements git.ActorHandleIssuer.
type fakeHandles struct {
	handle string
	err    error
}

func (handles *fakeHandles) IssueActor(_ context.Context, _ string) (string, error) {
	return handles.handle, handles.err
}

// fakeActorSource implements git.ActorSource.
type fakeActorSource struct {
	credential string
	err        error
}

func (source *fakeActorSource) ResolveActor(_ context.Context, _ string) (string, error) {
	return source.credential, source.err
}

// fakeBackend implements git.GitBackend.
type fakeBackend struct {
	request git.GitBackendRequest
	body    []byte
	status  int
	payload string
	err     error
}

func (backend *fakeBackend) Serve(_ context.Context, request git.GitBackendRequest, body io.Reader, response git.GitResponseWriter) error {
	backend.request = request
	if body != nil {
		backend.body, _ = io.ReadAll(body)
	}
	if backend.err != nil {
		return backend.err
	}
	status := backend.status
	if status == 0 {
		status = http.StatusOK
	}
	response.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	response.WriteHeader(status)
	_, _ = response.Write([]byte(backend.payload))
	return nil
}

// fakeClock implements git.Clock.
type fakeClock struct {
	now time.Time
}

func (clock fakeClock) Now() time.Time { return clock.now }
