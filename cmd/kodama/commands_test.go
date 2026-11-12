package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	actorapp "github.com/optimiware/kodama/internal/userland/git/app/actors"
)

// serveTestSetup is one configured `kodama git serve` invocation. It installs
// the injectable composition seams and restores them when the test ends.
type serveTestSetup struct {
	args           []string
	credentialPath string
	root           string
	config         gitapp.Config
	runnerCalls    int
	runnerErr      error
}

// newServeTestSetup writes a mounted service credential, builds the required
// command line and replaces the app runner with one capturing the composed
// configuration. The read-verifier constructor is left at its production
// value unless the caller overrides it.
func newServeTestSetup(t *testing.T) *serveTestSetup {
	t.Helper()
	root := t.TempDir()
	credentialPath := filepath.Join(root, "service-token")
	if err := os.WriteFile(credentialPath, []byte("kodsvc.worker-token\n"), 0o600); err != nil {
		t.Fatalf("write service credential: %v", err)
	}
	setup := &serveTestSetup{
		root:           root,
		credentialPath: credentialPath,
		args: []string{
			"--tenant", "tenant-1",
			"--storage-root", filepath.Join(root, "repos"),
			"--journal-dir", filepath.Join(root, "journal"),
			"--listen", "127.0.0.1:0",
			"--base-url", "https://kodama.example.invalid",
			"--credential-file", credentialPath,
			"--resource", "shop/web=resource-1",
		},
	}
	originalConstruct := newReadVerifier
	originalRun := runGitApp
	t.Cleanup(func() {
		newReadVerifier = originalConstruct
		runGitApp = originalRun
	})
	runGitApp = func(_ context.Context, config gitapp.Config) error {
		setup.runnerCalls++
		setup.config = config
		return setup.runnerErr
	}
	return setup
}

func (setup *serveTestSetup) run(t *testing.T) (int, string) {
	t.Helper()
	var stderr lockedBuffer
	code := runGitServeEntry(context.Background(), setup.args, &lockedBuffer{}, &stderr)
	return code, stderr.String()
}

// repositoryAwareReadActors is the exact repository-aware admission port a test
// injects instead of the production public read verifier.
type repositoryAwareReadActors struct {
	mu         sync.Mutex
	repository string
	credential string
	identity   git.Identity
}

func (actors *repositoryAwareReadActors) VerifyReadActor(_ context.Context, repository, credential string) (git.Identity, error) {
	actors.mu.Lock()
	defer actors.mu.Unlock()
	actors.repository = repository
	actors.credential = credential
	return actors.identity, nil
}

// TestGitServeWiresActorVerifierAndSharedHandles pins the production
// composition: one app configuration carrying the injected read verifier, one
// shared tenant actor registry for smart HTTP and the native hooks, and the
// repository-aware admission port (not a credential-only port).
func TestGitServeWiresActorVerifierAndSharedHandles(t *testing.T) {
	setup := newServeTestSetup(t)
	admission := &repositoryAwareReadActors{identity: git.Identity{Roles: []string{"engineer"}}}
	var constructedVerification git.VerificationClient
	var mappedRepository, mappedResource string
	newReadVerifier = func(verification git.VerificationClient, resolve func(string) (string, error)) (git.ReadActorVerifier, error) {
		constructedVerification = verification
		mappedRepository = "shop/web"
		resource, err := resolve(mappedRepository)
		if err != nil {
			return nil, err
		}
		mappedResource = resource
		return admission, nil
	}

	var handle, credential string
	originalRun := runGitApp
	runGitApp = func(ctx context.Context, config gitapp.Config) error {
		setup.runnerCalls++
		setup.config = config
		issued, err := config.ActorHandles.IssueActor(ctx, "actor-token-1")
		if err != nil {
			return err
		}
		handle = issued
		resolved, err := config.HookActors.ResolveActor(ctx, issued)
		if err != nil {
			return err
		}
		credential = resolved
		return nil
	}
	t.Cleanup(func() { runGitApp = originalRun })

	code, stderr := setup.run(t)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q; want 0", code, stderr)
	}
	if setup.runnerCalls != 1 {
		t.Fatalf("app runner calls = %d; want 1", setup.runnerCalls)
	}
	config := setup.config
	if config.Verify == nil {
		t.Fatal("Config.Verify is nil; the write path has no verification client")
	}
	if constructedVerification == nil || config.Verify != constructedVerification {
		t.Fatal("Config.Verify is not the constructed public verification client")
	}
	if config.Actors != admission {
		t.Fatal("Config.Actors is not the constructed read verifier")
	}
	if mappedRepository != "shop/web" || mappedResource != "resource-1" {
		t.Fatalf("resource resolver = (%q, %q); want (shop/web, resource-1)", mappedRepository, mappedResource)
	}
	handles, ok := config.ActorHandles.(*actorapp.Registry)
	if !ok {
		t.Fatalf("Config.ActorHandles = %T; want the shared actor registry", config.ActorHandles)
	}
	hooks, ok := config.HookActors.(*actorapp.Registry)
	if !ok {
		t.Fatalf("Config.HookActors = %T; want the shared actor registry", config.HookActors)
	}
	if handles != hooks {
		t.Fatal("smart HTTP and the native hooks do not share one actor registry")
	}
	if handle == "" || credential != "actor-token-1" {
		t.Fatalf("issue->resolve = (%q, %q); want one shared registry credential", handle, credential)
	}
}

// recordedPublicRequest is one observed outbound public request. It never
// carries the Authorization value in a message; the test asserts on it
// directly.
type recordedPublicRequest struct {
	method        string
	path          string
	authorization string
	request       api.Request
	verify        api.VerifyRequest
}

// verifyReply is one scripted public verification answer.
type verifyReply struct {
	principal string
	kind      string
	roles     []string
	status    int
	code      string
	message   string
}

// publicRecorder is a fake public RoundTripper. It records every request and
// answers from a queue, so it can prove both the request shape and token
// rotation.
type publicRecorder struct {
	mu       sync.Mutex
	replies  []verifyReply
	requests []recordedPublicRequest
}

func (recorder *publicRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var publicRequest api.Request
	if err := json.Unmarshal(body, &publicRequest); err != nil {
		return nil, err
	}
	var verify api.VerifyRequest
	if err := json.Unmarshal(publicRequest.Input, &verify); err != nil {
		return nil, err
	}
	recorder.mu.Lock()
	recorder.requests = append(recorder.requests, recordedPublicRequest{
		method:        request.Method,
		path:          request.URL.Path,
		authorization: request.Header.Get("Authorization"),
		request:       publicRequest,
		verify:        verify,
	})
	if len(recorder.replies) == 0 {
		recorder.mu.Unlock()
		return nil, errors.New("public fixture has no scripted reply")
	}
	reply := recorder.replies[0]
	recorder.replies = recorder.replies[1:]
	recorder.mu.Unlock()
	if reply.status != 0 {
		payload, err := json.Marshal(api.Error{Code: reply.code, Message: reply.message})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: reply.status,
			Status:     http.StatusText(reply.status),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(payload)),
		}, nil
	}
	responseBody, err := json.Marshal(api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: reply.principal, Kind: reply.kind},
		Roles:          reply.roles,
		VerificationID: "verification-1",
	})
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(api.Result{Operation: "actor.verify", Body: responseBody})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     http.StatusText(http.StatusOK),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}, nil
}

func (recorder *publicRecorder) recorded() []recordedPublicRequest {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]recordedPublicRequest(nil), recorder.requests...)
}

// TestGitServeUsesPublicGitReadVerification composes the production read
// verifier over the constructed public transport. It proves the forwarded
// actor credential reaches the public actor.verify operation with the
// registered resource, an empty subject, no approval and the mounted service
// credential as the outbound Bearer, and that a rotated mounted token takes
// effect without a restart.
func TestGitServeUsesPublicGitReadVerification(t *testing.T) {
	setup := newServeTestSetup(t)
	recorder := &publicRecorder{}
	code, stderr := setup.run(t)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q; want 0", code, stderr)
	}
	transport, ok := setup.config.Transport.(*client.HTTPTransport)
	if !ok {
		t.Fatalf("Config.Transport = %T; want the constructed public transport", setup.config.Transport)
	}
	transport.RoundTripper = recorder

	cases := []struct {
		actorCredential string
		reply           verifyReply
		want            git.Identity
	}{
		{
			actorCredential: "human-access-token",
			reply:           verifyReply{principal: "human-1", kind: "human", roles: []string{"engineer"}},
			want:            git.Identity{Roles: []string{"engineer"}},
		},
		{
			actorCredential: "kodex.execution-token",
			reply:           verifyReply{principal: "execution-1", kind: "execution"},
			want:            git.Identity{Execution: true},
		},
		{
			actorCredential: "kodsvc.service-token",
			reply:           verifyReply{principal: "service-1", kind: "service", roles: []string{"viewer"}},
			want:            git.Identity{ServiceID: "service-1"},
		},
	}
	for _, test := range cases {
		recorder.replies = append(recorder.replies, test.reply)
		identity, err := setup.config.Actors.VerifyReadActor(context.Background(), "shop/web", test.actorCredential)
		if err != nil {
			t.Fatalf("VerifyReadActor(%s): %v", test.reply.kind, err)
		}
		if !reflect.DeepEqual(identity, test.want) {
			t.Fatalf("identity for %s = %#v; want %#v", test.reply.kind, identity, test.want)
		}
	}

	// Rotate the mounted service token and prove the next attempt uses it.
	if err := os.WriteFile(setup.credentialPath, []byte("kodsvc.worker-token-rotated\n"), 0o600); err != nil {
		t.Fatalf("rotate service credential: %v", err)
	}
	recorder.replies = append(recorder.replies, verifyReply{principal: "human-1", kind: "human", roles: []string{"engineer"}})
	if _, err := setup.config.Actors.VerifyReadActor(context.Background(), "shop/web", "human-access-token"); err != nil {
		t.Fatalf("VerifyReadActor after rotation: %v", err)
	}

	requests := recorder.recorded()
	if len(requests) != len(cases)+1 {
		t.Fatalf("public requests = %d; want %d", len(requests), len(cases)+1)
	}
	wantBearer := []string{
		"Bearer kodsvc.worker-token",
		"Bearer kodsvc.worker-token",
		"Bearer kodsvc.worker-token",
		"Bearer kodsvc.worker-token-rotated",
	}
	wantActor := []string{
		"human-access-token",
		"kodex.execution-token",
		"kodsvc.service-token",
		"human-access-token",
	}
	for index, request := range requests {
		if request.method != http.MethodPost {
			t.Errorf("request %d method = %q; want POST", index, request.method)
		}
		if request.path != "/api/v1/verify" {
			t.Errorf("request %d path = %q; want /api/v1/verify", index, request.path)
		}
		if request.authorization != wantBearer[index] {
			t.Errorf("request %d Authorization = %q; want %q", index, request.authorization, wantBearer[index])
		}
		if len(request.request.Input) == 0 {
			t.Errorf("request %d carries no public input", index)
		}
		verify := request.verify
		if verify.ActorCredential != wantActor[index] {
			t.Errorf("request %d actor credential = %q; want the distinct forwarded actor", index, verify.ActorCredential)
		}
		if verify.ActorCredential == strings.TrimPrefix(request.authorization, "Bearer ") {
			t.Errorf("request %d forwards the mounted service credential as the actor", index)
		}
		if verify.ResourceID != "resource-1" {
			t.Errorf("request %d resource = %q; want resource-1", index, verify.ResourceID)
		}
		if verify.Operation != string(api.ResourceOperationGitRead) {
			t.Errorf("request %d operation = %q; want git.read", index, verify.Operation)
		}
		if len(verify.Subject.Fields) != 0 {
			t.Errorf("request %d subject = %#v; want an empty subject", index, verify.Subject)
		}
		if verify.Approval != "" {
			t.Errorf("request %d approval = %q; want none", index, verify.Approval)
		}
	}
}

// TestGitServeRefusesWithoutVerificationConfiguration pins that the public
// verification configuration is mandatory: there is no optional-transport
// branch, so a missing base URL or mounted credential file is a usage
// refusal before any state or app effect.
func TestGitServeRefusesWithoutVerificationConfiguration(t *testing.T) {
	complete := func(t *testing.T) []string {
		setup := newServeTestSetup(t)
		return setup.args
	}
	cases := []struct {
		name   string
		remove func([]string) []string
	}{
		{name: "missing base URL", remove: func(args []string) []string { return dropFlag(args, "--base-url") }},
		{name: "missing credential file", remove: func(args []string) []string { return dropFlag(args, "--credential-file") }},
		{name: "missing both", remove: func(args []string) []string {
			return dropFlag(dropFlag(args, "--base-url"), "--credential-file")
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			setup := newServeTestSetup(t)
			setup.args = test.remove(complete(t))
			code, stderr := setup.run(t)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2", code)
			}
			if !strings.Contains(stderr, "git serve requires --base-url and --credential-file") {
				t.Fatalf("stderr = %q; want the verification-configuration refusal", stderr)
			}
			if setup.runnerCalls != 0 {
				t.Fatalf("app runner calls = %d; want none", setup.runnerCalls)
			}
			if _, err := os.Stat(filepath.Join(setup.root, "repos")); !os.IsNotExist(err) {
				t.Fatalf("storage root state = %v; want no filesystem effect", err)
			}
		})
	}

	t.Run("environment supplies both", func(t *testing.T) {
		setup := newServeTestSetup(t)
		setup.args = dropFlag(dropFlag(setup.args, "--base-url"), "--credential-file")
		t.Setenv("KODAMA_BASE_URL", "https://kodama.example.invalid")
		t.Setenv("KODAMA_CREDENTIAL_FILE", setup.credentialPath)
		code, stderr := setup.run(t)
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %q; want 0", code, stderr)
		}
		if setup.runnerCalls != 1 {
			t.Fatalf("app runner calls = %d; want 1", setup.runnerCalls)
		}
	})
}

// dropFlag removes one flag and its value from a test command line.
func dropFlag(args []string, name string) []string {
	dropped := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == name {
			index++
			continue
		}
		dropped = append(dropped, args[index])
	}
	return dropped
}

// TestGitServeRefusesActorVerifierConstructionFailure pins the fail-closed
// startup order: a construction error or a nil verifier refuses with exit 1
// and a fixed, secret-free diagnostic before the app is invoked.
func TestGitServeRefusesActorVerifierConstructionFailure(t *testing.T) {
	cases := []struct {
		name       string
		construct  func(git.VerificationClient, func(string) (string, error)) (git.ReadActorVerifier, error)
		diagnostic string
		exitCode   int
	}{
		{
			name: "constructor error",
			construct: func(git.VerificationClient, func(string) (string, error)) (git.ReadActorVerifier, error) {
				return nil, errors.New("actor verifier construction failed for /secret/mounted/path")
			},
			diagnostic: "kodama: git serve: actor verification is not configured",
			exitCode:   1,
		},
		{
			name: "nil verifier",
			construct: func(git.VerificationClient, func(string) (string, error)) (git.ReadActorVerifier, error) {
				return nil, nil
			},
			diagnostic: "kodama: git serve: actor verification is not configured",
			exitCode:   1,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			setup := newServeTestSetup(t)
			newReadVerifier = test.construct
			code, stderr := setup.run(t)
			if code != test.exitCode {
				t.Fatalf("exit code = %d; want %d", code, test.exitCode)
			}
			if strings.TrimSpace(stderr) != test.diagnostic {
				t.Fatalf("stderr = %q; want the fixed diagnostic %q", stderr, test.diagnostic)
			}
			if strings.Contains(stderr, "kodsvc.worker-token") || strings.Contains(stderr, "secret") {
				t.Fatalf("stderr = %q; the diagnostic is not secret-free", stderr)
			}
			if setup.runnerCalls != 0 {
				t.Fatalf("app runner calls = %d; want none before a verifier exists", setup.runnerCalls)
			}
			if _, err := os.Stat(filepath.Join(setup.root, "repos")); !os.IsNotExist(err) {
				t.Fatalf("storage root state = %v; want no filesystem effect from the composition", err)
			}
		})
	}
}

// servePolicyExecutor scripts the committed admin policy so the command-level
// handler tests exercise the real smart-HTTP refusal mapping without native
// Git. It answers the exact policy command surface and reports a scripted
// failure when asked.
type servePolicyExecutor struct {
	mu       sync.Mutex
	policy   string
	commit   string
	broken   bool
	commands []git.Command
}

func (executor *servePolicyExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.commands = append(executor.commands, command)
	joined := strings.Join(command.Args, " ")
	switch {
	case command.Name == "git" && len(command.Args) == 1 && command.Args[0] == "--version":
		return git.CommandResult{Stdout: []byte("git version 2.44.0\n")}, nil
	case strings.Contains(joined, "for-each-ref"):
		if executor.broken {
			return git.CommandResult{ExitCode: 1}, nil
		}
		return git.CommandResult{Stdout: []byte("refs/heads/main " + executor.commit + "\n")}, nil
	case strings.Contains(joined, "rev-parse"):
		if strings.Contains(joined, "--show-ref-format") {
			return git.CommandResult{Stdout: []byte("files\n")}, nil
		}
		return git.CommandResult{Stdout: []byte("/scripted/admin\n")}, nil
	case strings.Contains(joined, "cat-file"):
		for _, argument := range command.Args {
			if argument == "-t" {
				return git.CommandResult{Stdout: []byte("commit\n")}, nil
			}
			if strings.Contains(argument, ":") {
				return git.CommandResult{Stdout: []byte(executor.policy)}, nil
			}
		}
	case strings.Contains(joined, "merge-base"):
		return git.CommandResult{}, nil
	}
	return git.CommandResult{}, nil
}

// serveBackend records every admitted smart-HTTP exchange.
type serveBackend struct {
	mu    sync.Mutex
	calls int
	last  git.GitBackendRequest
}

func (backend *serveBackend) Serve(_ context.Context, request git.GitBackendRequest, _ io.Reader, response git.GitResponseWriter) error {
	backend.mu.Lock()
	backend.calls++
	backend.last = request
	backend.mu.Unlock()
	response.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("ok"))
	return nil
}

func (backend *serveBackend) count() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.calls
}

// serveHandler composes the captured production configuration with a scripted
// policy, a recording backend and the fake public verification transport, and
// returns the served server.
func serveHandler(t *testing.T, setup *serveTestSetup, executor *servePolicyExecutor, backend *serveBackend, recorder *publicRecorder) (*gitapp.Server, *actorapp.Registry) {
	t.Helper()
	code, stderr := setup.run(t)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q; want 0", code, stderr)
	}
	config := setup.config
	if transport, ok := config.Transport.(*client.HTTPTransport); ok && recorder != nil {
		transport.RoundTripper = recorder
	}
	executor.commit = "0123456789012345678901234567890123456789"
	setup.config.Executor = executor
	setup.config.Backend = backend
	adminPath := filepath.Join(setup.config.StorageRoot, string(setup.config.TenantID), "admin")
	for _, directory := range []string{"refs/heads", "refs/tags", "objects"} {
		if err := os.MkdirAll(filepath.Join(adminPath, filepath.FromSlash(directory)), 0o700); err != nil {
			t.Fatalf("create admin repository layout: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(adminPath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write admin repository HEAD: %v", err)
	}
	// The command-level closure invalidates the composition registry when the
	// runner returns. These handler tests need a live registry for the served
	// instance; they use one fresh instance for both ports, as production does.
	registry, err := actorapp.NewRegistry(actorapp.Config{TenantID: id.TenantID("tenant-1")})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	setup.config.ActorHandles = registry
	setup.config.HookActors = registry
	server, err := gitapp.New(setup.config)
	if err != nil {
		t.Fatalf("gitapp.New: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, registry
}

// TestGitServePreservesHTTPRefusals pins the smart-HTTP refusal mapping on the
// production-composed handler: 401 with the Basic challenge for missing
// authentication and for an unregistered repository (deliberately
// indistinguishable from a credential failure), 403 for a reader attempting
// receive-pack, 404 for an actor denied visibility, and 503 for a verification
// dependency outage (with a retry hint, never the credential challenge) and for
// a committed-policy dependency failure. No refusal reaches the backend.
func TestGitServePreservesHTTPRefusals(t *testing.T) {
	engineerPolicy := "repo shop/web\n    R = @all\n"
	readerPolicy := "repo shop/web\n    R = @all\n"
	cases := []struct {
		name          string
		path          string
		credential    string
		policy        string
		broken        bool
		replies       []verifyReply
		wantStatus    int
		wantBody      string
		wantChallenge bool
		wantRetry     bool
	}{
		{
			name:          "missing authentication",
			path:          "/shop/web/info/refs?service=git-upload-pack",
			wantStatus:    http.StatusUnauthorized,
			wantBody:      "Unauthorized\n",
			wantChallenge: true,
		},
		{
			// A transient verifier failure is a retryable server error,
			// never a credential verdict. The client must retry, not re-prompt.
			name:       "verification outage during admission",
			path:       "/shop/web/info/refs?service=git-upload-pack",
			credential: "human-access-token",
			replies: []verifyReply{{
				status: http.StatusServiceUnavailable, code: api.CodeDependencyUnavailable,
				message: "the verification fixture is unavailable",
			}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "Service Unavailable\n",
			wantRetry:  true,
		},
		{
			name:          "unregistered repository",
			path:          "/unmapped/info/refs?service=git-upload-pack",
			credential:    "human-access-token",
			replies:       []verifyReply{{principal: "human-1", kind: "human", roles: []string{"engineer"}}},
			wantStatus:    http.StatusUnauthorized,
			wantBody:      "Unauthorized\n",
			wantChallenge: true,
		},
		{
			name:       "reader attempting receive-pack",
			path:       "/shop/web/info/refs?service=git-receive-pack",
			credential: "human-access-token",
			policy:     readerPolicy,
			replies:    []verifyReply{{principal: "human-1", kind: "human", roles: []string{"engineer"}}},
			wantStatus: http.StatusForbidden,
			wantBody:   "Forbidden\n",
		},
		{
			name:       "actor denied repository visibility",
			path:       "/shop/web/info/refs?service=git-upload-pack",
			credential: "human-access-token",
			policy:     "repo other\n    R = @all\n",
			replies:    []verifyReply{{principal: "human-1", kind: "human", roles: []string{"engineer"}}},
			wantStatus: http.StatusNotFound,
			wantBody:   "Not Found\n",
		},
		{
			name:       "committed policy dependency failure",
			path:       "/shop/web/info/refs?service=git-upload-pack",
			credential: "human-access-token",
			policy:     engineerPolicy,
			broken:     true,
			replies:    []verifyReply{{principal: "human-1", kind: "human", roles: []string{"engineer"}}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "Service Unavailable\n",
			wantRetry:  true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			setup := newServeTestSetup(t)
			executor := &servePolicyExecutor{policy: test.policy}
			backend := &serveBackend{}
			recorder := &publicRecorder{replies: append([]verifyReply(nil), test.replies...)}
			server, _ := serveHandler(t, setup, executor, backend, recorder)
			handler := server.Handler()
			executor.mu.Lock()
			executor.broken = test.broken
			executor.mu.Unlock()

			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.credential != "" {
				request.SetBasicAuth("kodama", test.credential)
			}
			recorderResponse := httptest.NewRecorder()
			handler.ServeHTTP(recorderResponse, request)

			if recorderResponse.Code != test.wantStatus {
				t.Fatalf("status = %d; want %d (body %q)", recorderResponse.Code, test.wantStatus, recorderResponse.Body.String())
			}
			if recorderResponse.Body.String() != test.wantBody {
				t.Fatalf("body = %q; want %q", recorderResponse.Body.String(), test.wantBody)
			}
			challenge := recorderResponse.Header().Get("WWW-Authenticate")
			if test.wantChallenge && challenge != `Basic realm="kodama"` {
				t.Fatalf("challenge = %q; want the Basic challenge", challenge)
			}
			if !test.wantChallenge && challenge != "" {
				t.Fatalf("challenge = %q; want none", challenge)
			}
			retryAfter := recorderResponse.Header().Get("Retry-After")
			if test.wantRetry {
				seconds, err := strconv.Atoi(retryAfter)
				if err != nil || seconds < 1 || seconds > 10 {
					t.Fatalf("Retry-After = %q; want a small whole-second retry hint", retryAfter)
				}
			} else if retryAfter != "" {
				t.Fatalf("Retry-After = %q; want none for a non-transient refusal", retryAfter)
			}
			if backend.count() != 0 {
				t.Fatalf("backend calls = %d; want none for a refusal", backend.count())
			}
		})
	}

	t.Run("bearer authentication is accepted", func(t *testing.T) {
		setup := newServeTestSetup(t)
		executor := &servePolicyExecutor{policy: readerPolicy}
		backend := &serveBackend{}
		recorder := &publicRecorder{replies: []verifyReply{{principal: "human-1", kind: "human", roles: []string{"engineer"}}}}
		server, _ := serveHandler(t, setup, executor, backend, recorder)
		handler := server.Handler()

		request := httptest.NewRequest(http.MethodGet, "/shop/web/info/refs?service=git-upload-pack", nil)
		request.Header.Set("Authorization", "Bearer human-access-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %q)", response.Code, response.Body.String())
		}
		if backend.count() != 1 {
			t.Fatalf("backend calls = %d; want 1", backend.count())
		}
	})
}

// TestGitServeReverifiesActorInPreReceive pins the two-step authority: read
// admission verifies git.read only, and the native pre-receive path resolves
// the same opaque handle through the shared registry and independently verifies
// the exact git.ref.update subject. A write-verification refusal rejects the
// push, and the original actor credential (never the mounted service
// credential) reaches the public client.
func TestGitServeReverifiesActorInPreReceive(t *testing.T) {
	setup := newServeTestSetup(t)
	policy := "repo shop/web\n    R = @all\n    RW refs/heads/ = @engineer\n"
	executor := &servePolicyExecutor{policy: policy}
	backend := &serveBackend{}
	recorder := &publicRecorder{replies: []verifyReply{
		{principal: "human-1", kind: "human", roles: []string{"engineer"}},
		{status: http.StatusForbidden, code: api.CodeForbidden, message: "the write verification was refused"},
	}}
	server, _ := serveHandler(t, setup, executor, backend, recorder)
	handler := server.Handler()

	admission := httptest.NewRequest(http.MethodPost, "/shop/web/git-receive-pack", strings.NewReader("admission"))
	admission.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	admission.SetBasicAuth("kodama", "human-access-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, admission)
	if response.Code != http.StatusOK {
		t.Fatalf("admission status = %d; want 200 (body %q)", response.Code, response.Body.String())
	}
	if backend.count() != 1 {
		t.Fatalf("admission backend calls = %d; want 1", backend.count())
	}
	environment := environmentMap(backend.last.Environment)
	handle := environment[git.EnvActorRef]
	if handle == "" {
		t.Fatalf("backend environment = %v; want the opaque actor handle", backend.last.Environment)
	}
	if handle == "human-access-token" || strings.Contains(handle, "human-access-token") {
		t.Fatal("backend environment carries the raw actor credential")
	}
	if got := environment[git.EnvResourceID]; got != "resource-1" {
		t.Fatalf("backend resource id = %q; want resource-1", got)
	}

	t.Setenv(git.EnvActorRef, handle)
	t.Setenv(git.EnvRepository, "shop/web")
	t.Setenv(git.EnvResourceID, "resource-1")
	t.Setenv(git.EnvSourceEvent, "0123456789abcdef0123456789abcdef")
	hook := server.Hook()
	old := strings.Repeat("a", 40)
	candidate := strings.Repeat("b", 40)
	result, err := hook.RunPreReceive(context.Background(),
		strings.NewReader(old+" "+candidate+" refs/heads/main\n"))
	if err == nil {
		t.Fatal("RunPreReceive returned no refusal cause for a rejected update")
	}
	if result.Accepted() {
		t.Fatal("pre-receive accepted an update whose write verification was refused")
	}
	if result.ExitCode != 1 || result.Message != "kodama git: update rejected" {
		t.Fatalf("pre-receive result = %#v; want the sanitized rejection", result)
	}

	requests := recorder.recorded()
	if len(requests) != 2 {
		t.Fatalf("public requests = %d; want exactly one admission and one update verification", len(requests))
	}
	if requests[0].verify.Operation != string(api.ResourceOperationGitRead) {
		t.Fatalf("first operation = %q; want git.read", requests[0].verify.Operation)
	}
	update := requests[1].verify
	if update.Operation != string(api.ResourceOperationGitRefUpdate) {
		t.Fatalf("second operation = %q; want git.ref.update", update.Operation)
	}
	if update.ActorCredential != "human-access-token" {
		t.Fatalf("update actor credential = %q; want the original actor credential", update.ActorCredential)
	}
	if update.ActorCredential == strings.TrimPrefix(requests[1].authorization, "Bearer ") {
		t.Fatal("the mounted service credential replaced the incoming actor credential")
	}
	if update.ResourceID != "resource-1" {
		t.Fatalf("update resource = %q; want resource-1", update.ResourceID)
	}
	if update.Subject.Fields == nil || len(update.Subject.Fields) != 5 {
		t.Fatalf("update subject = %#v; want the five-field git.ref.update subject", update.Subject)
	}
	wantFields := []api.SubjectField{
		{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
		{Name: "resource", Kind: "string", Value: "resource-1"},
		{Name: "ref", Kind: "string", Value: "refs/heads/main"},
		{Name: "expected_base", Kind: "string", Value: old},
		{Name: "candidate", Kind: "string", Value: candidate},
	}
	for index, want := range wantFields {
		if update.Subject.Fields[index] != want {
			t.Errorf("subject field %d = %#v; want %#v", index, update.Subject.Fields[index], want)
		}
	}
	if update.Approval != "" {
		t.Fatalf("update approval = %q; want none", update.Approval)
	}
}

func environmentMap(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

// TestGitServeRefusesPreReceiveAfterActorRevocation pins the revocation
// scenario: a handle admitted while the actor was valid stops resolving as
// soon as the registry is revoked, so pre-receive rejects instead of reusing a
// cached credential or skipping the exact update verification.
func TestGitServeRefusesPreReceiveAfterActorRevocation(t *testing.T) {
	setup := newServeTestSetup(t)
	policy := "repo shop/web\n    R = @all\n    RW refs/heads/ = @engineer\n"
	executor := &servePolicyExecutor{policy: policy}
	backend := &serveBackend{}
	recorder := &publicRecorder{replies: []verifyReply{
		{principal: "human-1", kind: "human", roles: []string{"engineer"}},
	}}
	server, registry := serveHandler(t, setup, executor, backend, recorder)
	handler := server.Handler()

	admission := httptest.NewRequest(http.MethodPost, "/shop/web/git-receive-pack", strings.NewReader("admission"))
	admission.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	admission.SetBasicAuth("kodama", "human-access-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, admission)
	if response.Code != http.StatusOK {
		t.Fatalf("admission status = %d; want 200 (body %q)", response.Code, response.Body.String())
	}
	handle := environmentMap(backend.last.Environment)[git.EnvActorRef]
	if handle == "" {
		t.Fatal("admission produced no actor handle")
	}

	// Revoke every live handle between admission and pre-receive.
	if err := registry.Close(); err != nil {
		t.Fatalf("revoke actor registry: %v", err)
	}

	t.Setenv(git.EnvActorRef, handle)
	t.Setenv(git.EnvRepository, "shop/web")
	t.Setenv(git.EnvResourceID, "resource-1")
	t.Setenv(git.EnvSourceEvent, "0123456789abcdef0123456789abcdef")
	result, err := server.Hook().RunPreReceive(context.Background(),
		strings.NewReader(strings.Repeat("a", 40)+" "+strings.Repeat("b", 40)+" refs/heads/main\n"))
	if err == nil {
		t.Fatal("pre-receive returned no refusal cause after revocation")
	}
	if result.Accepted() {
		t.Fatal("pre-receive accepted an update after the actor handle was revoked")
	}
	if result.Message != "kodama git: update rejected" {
		t.Fatalf("pre-receive message = %q; want the sanitized rejection", result.Message)
	}
	if requests := recorder.recorded(); len(requests) != 1 {
		t.Fatalf("public requests = %d; want only the admission read verification", len(requests))
	}
}

// TestGitServeClosesActorRegistry pins the registry lifetime: every return from
// the app runner, success or failure, invalidates the handles issued during
// composition and refuses later issuance.
func TestGitServeClosesActorRegistry(t *testing.T) {
	cases := []struct {
		name      string
		runnerErr error
		wantCode  int
	}{
		{name: "successful run", wantCode: 0},
		{name: "runner error", runnerErr: errors.New("serve failed"), wantCode: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			setup := newServeTestSetup(t)
			var issued string
			var handles git.ActorHandleIssuer
			runGitApp = func(_ context.Context, config gitapp.Config) error {
				setup.runnerCalls++
				setup.config = config
				handles = config.ActorHandles
				handle, err := config.ActorHandles.IssueActor(context.Background(), "actor-token-1")
				if err != nil {
					return err
				}
				issued = handle
				return test.runnerErr
			}
			setup.runnerErr = test.runnerErr
			code, _ := setup.run(t)
			if code != test.wantCode {
				t.Fatalf("exit code = %d; want %d", code, test.wantCode)
			}
			if issued == "" {
				t.Fatal("no handle was issued during the run")
			}
			if _, err := setup.config.HookActors.ResolveActor(context.Background(), issued); err == nil {
				t.Fatal("a handle issued during the run still resolves after composition returned")
			}
			if _, err := handles.IssueActor(context.Background(), "actor-token-2"); !errors.Is(err, actorapp.ErrClosed) {
				t.Fatalf("IssueActor after return = %v; want ErrClosed", err)
			}
		})
	}
}

// TestGitServeRefusesTheRemovedPolicyFlag pins that there is no --policy
// bootstrap fallback: --policy is an unknown flag, so it is a usage error
// (exit 2), not a silently ignored input.
func TestGitServeRefusesTheRemovedPolicyFlag(t *testing.T) {
	root := t.TempDir()
	var stderr lockedBuffer
	code := runGitServeEntry(context.Background(), []string{
		"--tenant", "tenant-1",
		"--storage-root", filepath.Join(root, "repos"),
		"--journal-dir", filepath.Join(root, "journal"),
		"--policy", filepath.Join(root, "git.conf"),
	}, &lockedBuffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") ||
		!strings.Contains(stderr.String(), "policy") {
		t.Fatalf("stderr = %q; want the unknown-flag usage error naming policy", stderr.String())
	}
}

// TestGitServeIgnoresTheRemovedPolicyEnvironment pins that the
// KODAMA_GIT_POLICY variable is not read: it satisfies no required input, so
// the refusal names the required flags and never a policy one.
func TestGitServeIgnoresTheRemovedPolicyEnvironment(t *testing.T) {
	t.Setenv("KODAMA_GIT_POLICY", filepath.Join(t.TempDir(), "git.conf"))
	var stderr lockedBuffer
	code := runGitServeEntry(context.Background(), nil, &lockedBuffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2 (the required-flag refusal)", code)
	}
	if !strings.Contains(stderr.String(), "git serve requires --tenant, --storage-root and --journal-dir") {
		t.Fatalf("stderr = %q; want the required-flag refusal", stderr.String())
	}
	if strings.Contains(stderr.String(), "policy") {
		t.Fatalf("stderr = %q; the removed KODAMA_GIT_POLICY variable must not be part of the contract", stderr.String())
	}
}

func TestGitServeRequiresExplicitConfiguration(t *testing.T) {
	var stderr lockedBuffer
	if code := runGitServeEntry(context.Background(), nil, &lockedBuffer{}, &stderr); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "git serve requires") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestAgentRequiresExplicitRunInput(t *testing.T) {
	var stderr lockedBuffer
	if code := runAgentEntry(context.Background(), nil, &lockedBuffer{}, &stderr); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "agent requires") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestServicesRequireExplicitConfiguration(t *testing.T) {
	var stderr lockedBuffer
	if code := runServicesEntry(context.Background(), nil, &lockedBuffer{}, &stderr); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "services requires") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestGitServeRejectsMalformedResourceFlag(t *testing.T) {
	root := t.TempDir()
	var stderr lockedBuffer
	code := runGitServeEntry(context.Background(), []string{
		"--tenant", "tenant-1",
		"--storage-root", root,
		"--journal-dir", root,
		"--resource", "missing-equals",
	}, &lockedBuffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid --resource") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestGitServeMaxBodyAndResponseBytesFlagsAndEnv(t *testing.T) {
	t.Run("explicit flags wire into gitapp.Config", func(t *testing.T) {
		setup := newServeTestSetup(t)
		setup.args = append(setup.args, "--max-body-bytes", "67108864", "--max-response-bytes", "134217728")
		code, stderr := setup.run(t)
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %q; want 0", code, stderr)
		}
		if setup.config.MaxBodyBytes != 67108864 || setup.config.MaxResponseBytes != 134217728 {
			t.Fatalf("bounds = (%d, %d); want (67108864, 134217728)",
				setup.config.MaxBodyBytes, setup.config.MaxResponseBytes)
		}
	})

	t.Run("environment supplies bounds", func(t *testing.T) {
		setup := newServeTestSetup(t)
		t.Setenv("KODAMA_GIT_MAX_BODY_BYTES", "33554432")
		t.Setenv("KODAMA_GIT_MAX_RESPONSE_BYTES", "268435456")
		code, stderr := setup.run(t)
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %q; want 0", code, stderr)
		}
		if setup.config.MaxBodyBytes != 33554432 || setup.config.MaxResponseBytes != 268435456 {
			t.Fatalf("bounds = (%d, %d); want (33554432, 268435456)",
				setup.config.MaxBodyBytes, setup.config.MaxResponseBytes)
		}
	})

	for _, invalid := range []struct {
		name string
		env  map[string]string
		args []string
	}{
		{name: "negative max-body-bytes flag", args: []string{"--max-body-bytes=-1"}},
		{name: "negative max-response-bytes flag", args: []string{"--max-response-bytes=-1"}},
		{name: "malformed max-body-bytes flag", args: []string{"--max-body-bytes=not-an-int"}},
		{name: "malformed max-response-bytes flag", args: []string{"--max-response-bytes=not-an-int"}},
		{name: "negative KODAMA_GIT_MAX_BODY_BYTES", env: map[string]string{"KODAMA_GIT_MAX_BODY_BYTES": "-1"}},
		{name: "negative KODAMA_GIT_MAX_RESPONSE_BYTES", env: map[string]string{"KODAMA_GIT_MAX_RESPONSE_BYTES": "-1"}},
		{name: "malformed KODAMA_GIT_MAX_BODY_BYTES", env: map[string]string{"KODAMA_GIT_MAX_BODY_BYTES": "abc"}},
		{name: "malformed KODAMA_GIT_MAX_RESPONSE_BYTES", env: map[string]string{"KODAMA_GIT_MAX_RESPONSE_BYTES": "abc"}},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			setup := newServeTestSetup(t)
			for key, value := range invalid.env {
				t.Setenv(key, value)
			}
			setup.args = append(setup.args, invalid.args...)
			code, _ := setup.run(t)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2", code)
			}
			if setup.runnerCalls != 0 {
				t.Fatalf("runner calls = %d; want 0 on usage error", setup.runnerCalls)
			}
		})
	}
}
