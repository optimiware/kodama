package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/git"
)

func wiredConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		TenantID:         "tenant-1",
		StorageRoot:      filepath.Join(root, "repos"),
		JournalDirectory: filepath.Join(root, "journal"),
		ResourceIDs:      map[string]string{"repo": "resource-1"},
		// Startup declares the native Git minimum through the command seam, so
		// every wired config explicitly answers `git --version`.
		Executor:     &fakeExecutor{result: git.CommandResult{Stdout: []byte("git version 2.44.0\n")}},
		Observer:     &fakeObserver{},
		Verify:       &fakeVerification{},
		Retention:    &fakeRetention{},
		Clock:        fakeClock{},
		Backend:      &fakeBackend{payload: "ok"},
		Actors:       &fakeActors{},
		ActorHandles: &fakeHandles{handle: "handle-1"},
		HookActors:   &fakeActorSource{credential: "credential-1"},
		SourceEvent:  func() (string, error) { return "event-1", nil },
	}
}

// TestRunRefusesWithoutActorPorts pins the independent missing-actor-port
// guard: each actor port is required, and the refusal happens before the
// executor, storage, journal or listener has any effect.
func TestRunRefusesWithoutActorPorts(t *testing.T) {
	cases := []struct {
		name   string
		remove func(*Config)
	}{
		{name: "read actor verifier", remove: func(config *Config) { config.Actors = nil }},
		{name: "actor handle issuer", remove: func(config *Config) { config.ActorHandles = nil }},
		{name: "hook actor source", remove: func(config *Config) { config.HookActors = nil }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := wiredConfig(t)
			executor := config.Executor.(*fakeExecutor)
			test.remove(&config)
			listener := &probeListener{}
			config.Listener = listener
			err := Run(context.Background(), config)
			if !errors.Is(err, ErrDisabled) {
				t.Fatalf("Run error = %v; want ErrDisabled", err)
			}
			if command := executor.last(); command.Name != "" {
				t.Fatalf("executor ran %q; want no command before the port refusal", command.Name)
			}
			if _, statErr := os.Stat(config.StorageRoot); !os.IsNotExist(statErr) {
				t.Fatalf("storage root state = %v; want no storage effect", statErr)
			}
			if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
				t.Fatalf("journal state = %v; want no journal effect", statErr)
			}
			if listener.accepts != 0 {
				t.Fatalf("listener accepts = %d; want none", listener.accepts)
			}
		})
	}
}

// TestErrDisabledNamesTheMissingPorts pins the clean refusal message: it names
// the three required actor boundaries and never leaks an internal owner or
// claims the product cannot support them. The production entry wires these
// ports, so the message must describe a configuration omission, not a missing capability.
func TestErrDisabledNamesTheMissingPorts(t *testing.T) {
	message := ErrDisabled.Error()
	for _, want := range []string{"smart-HTTP actor authentication", "handle"} {
		if !strings.Contains(message, want) {
			t.Fatalf("ErrDisabled message = %q; want it to name %q", message, want)
		}
	}
	for _, stale := range []string{"I03", "owned by", "not wired"} {
		if strings.Contains(message, stale) {
			t.Fatalf("ErrDisabled message = %q; it still carries the stale marker %q", message, stale)
		}
	}
}

// probeListener records whether startup ever reached the serving path.
type probeListener struct {
	accepts int
}

func (listener *probeListener) Accept() (net.Conn, error) {
	listener.accepts++
	return nil, errors.New("unexpected accept")
}

func (listener *probeListener) Close() error   { return nil }
func (listener *probeListener) Addr() net.Addr { return &net.TCPAddr{} }

// TestGitStartupChecksVersionBeforeEffects pins the startup order: an
// unsupported native Git refuses before hook installation, journal creation or
// any listener use. The configured executor is the same seam the service uses,
// so the check cannot pass against a different executable.
func TestGitStartupChecksVersionBeforeEffects(t *testing.T) {
	config := committedPolicyConfig(t, "", "")
	config.Executor = &fakeExecutor{result: git.CommandResult{
		Stdout: []byte("git version 2.43.0\n"),
	}}
	listener := &probeListener{}
	config.Listener = listener

	err := Run(context.Background(), config)
	if err == nil {
		t.Fatal("Run started with an unsupported Git version")
	}
	if !strings.Contains(err.Error(), "2.43.0") || !strings.Contains(err.Error(), "2.44.0") {
		t.Fatalf("Run error = %q; want the observed and minimum Git versions", err)
	}
	if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("journal state = %v; want no journal directory before the version check", statErr)
	}
	adminPath := filepath.Join(config.StorageRoot, string(config.TenantID), "admin")
	if _, statErr := os.Stat(filepath.Join(adminPath, "hooks")); !os.IsNotExist(statErr) {
		t.Fatalf("hooks state = %v; want no installed hooks before the version check", statErr)
	}
	if listener.accepts != 0 {
		t.Fatalf("listener accepts = %d; want none before the version check", listener.accepts)
	}
}

// TestGitStartupVersionCheckPrecedesStorageRoot kills the mutant that probes
// after NewLocalStorage: the storage root (which MkdirAlls) must not exist yet
// and must still be absent after an unsupported Git refuses startup.
func TestGitStartupVersionCheckPrecedesStorageRoot(t *testing.T) {
	config := wiredConfig(t)
	config.Executor = &fakeExecutor{result: git.CommandResult{
		Stdout: []byte("git version 2.43.0\n"),
	}}
	listener := &probeListener{}
	config.Listener = listener
	if _, statErr := os.Stat(config.StorageRoot); !os.IsNotExist(statErr) {
		t.Fatalf("storage root precondition = %v; want an absent directory", statErr)
	}

	err := Run(context.Background(), config)
	if err == nil {
		t.Fatal("Run started with an unsupported Git version")
	}
	if _, statErr := os.Stat(config.StorageRoot); !os.IsNotExist(statErr) {
		t.Fatalf("storage root state = %v; want none before the version check", statErr)
	}
	if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("journal state = %v; want none before the version check", statErr)
	}
	if listener.accepts != 0 {
		t.Fatalf("listener accepts = %d; want none before the version check", listener.accepts)
	}
}

// installFakeGit writes an executable `git` that answers only `git --version`
// and puts it first on PATH for this test. Any other invocation fails loudly, so
// startup can never silently rely on a different native Git behaviour.
func installFakeGit(t *testing.T, version string) {
	t.Helper()
	directory := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$#\" -eq 1 ] && [ \"$1\" = \"--version\" ]; then\n" +
		"  echo 'git version " + version + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo 'fake git: unsupported invocation' >&2\n" +
		"exit 127\n"
	path := filepath.Join(directory, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("make fake git executable: %v", err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// requireStartupEffectsAbsent proves the version check ran before storage,
// journal or listener effects.
func requireStartupEffectsAbsent(t *testing.T, config Config, listener *probeListener) {
	t.Helper()
	if _, statErr := os.Stat(config.StorageRoot); !os.IsNotExist(statErr) {
		t.Fatalf("storage root state = %v; want it absent before the version check", statErr)
	}
	if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("journal state = %v; want it absent before the version check", statErr)
	}
	if listener != nil && listener.accepts != 0 {
		t.Fatalf("listener accepts = %d; want none before the version check", listener.accepts)
	}
}

// TestGitStartupProbesDefaultExecutorBeforeEffects pins the production startup
// path: cmd/kodama leaves Config.Executor nil, so Run and New must probe the
// default executor built for the server rather than skip the check. The only
// native Git here is a PATH fake answering `--version` with 2.43.0, so a
// skipped probe (or a probe that consults another executable) lets startup
// reach storage, journal or listener effects and fails this test.
func TestGitStartupProbesDefaultExecutorBeforeEffects(t *testing.T) {
	installFakeGit(t, "2.43.0")

	t.Run("Run", func(t *testing.T) {
		config := wiredConfig(t)
		config.Executor = nil
		listener := &probeListener{}
		config.Listener = listener

		err := Run(context.Background(), config)
		if err == nil {
			t.Fatal("Run started with the default executor resolving Git 2.43.0")
		}
		if !strings.Contains(err.Error(), "2.43.0") || !strings.Contains(err.Error(), "2.44.0") {
			t.Fatalf("Run error = %q; want the observed and minimum Git versions", err)
		}
		requireStartupEffectsAbsent(t, config, listener)
	})

	t.Run("New", func(t *testing.T) {
		config := wiredConfig(t)
		config.Executor = nil
		listener := &probeListener{}
		config.Listener = listener

		server, err := New(config)
		if err == nil {
			_ = server.Close()
			t.Fatal("New started with the default executor resolving Git 2.43.0")
		}
		if !strings.Contains(err.Error(), "2.43.0") || !strings.Contains(err.Error(), "2.44.0") {
			t.Fatalf("New error = %q; want the observed and minimum Git versions", err)
		}
		requireStartupEffectsAbsent(t, config, listener)
	})
}

// TestGitStartupDefaultExecutorAcceptsSupportedGit is the positive control for
// the default-executor path: the same PATH fake answering 2.44.0 lets New finish
// startup, proving the refusal above observes the real probe result rather than
// failing for an unrelated reason. Every remaining startup boundary is an
// injected fake, so no other native Git command is needed and the real Git
// positive control stays with the integration test.
func TestGitStartupDefaultExecutorAcceptsSupportedGit(t *testing.T) {
	installFakeGit(t, "2.44.0")
	config := wiredConfig(t)
	config.Executor = nil

	server, err := New(config)
	if err != nil {
		t.Fatalf("New with the default executor resolving Git 2.44.0: %v", err)
	}
	defer func() { _ = server.Close() }()
	if server.Handler() == nil || server.Service() == nil {
		t.Fatal("New returned an incomplete server")
	}
	if info, statErr := os.Stat(config.StorageRoot); statErr != nil || !info.IsDir() {
		t.Fatalf("storage root state = %v; want the directory startup created after the version check", statErr)
	}
}

// TestGitStartupRefusesCancelledContextBeforeEffects pins the context
// pass-through: Run hands its own context to the probe, so an already-cancelled
// startup returns context.Canceled and never runs the executable, creates the
// storage root or journal, or accepts a connection.
func TestGitStartupRefusesCancelledContextBeforeEffects(t *testing.T) {
	config := wiredConfig(t)
	executor := &fakeExecutor{result: git.CommandResult{
		Stdout: []byte("git version 2.44.0\n"),
	}}
	config.Executor = executor
	listener := &probeListener{}
	config.Listener = listener
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Run(ctx, config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run on a cancelled context = %v; want context.Canceled", err)
	}
	if command := executor.last(); command.Name != "" {
		t.Fatalf("probe ran %q on a cancelled context; want no executable effect", command.Name)
	}
	if _, statErr := os.Stat(config.StorageRoot); !os.IsNotExist(statErr) {
		t.Fatalf("storage root state = %v; want none before the version check", statErr)
	}
	if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("journal state = %v; want none before the version check", statErr)
	}
	if listener.accepts != 0 {
		t.Fatalf("listener accepts = %d; want none before the version check", listener.accepts)
	}
}

func TestNewBuildsHandlerAndHook(t *testing.T) {
	server, err := New(wiredConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if server.Handler() == nil || server.Hook() == nil || server.Service() == nil {
		t.Fatal("New returned an incomplete server")
	}
}

func TestServeShutsDownAndJoins(t *testing.T) {
	server, err := New(wiredConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/repo/info/refs?service=git-upload-pack")
	if err != nil {
		cancel()
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		cancel()
		t.Fatalf("status = %d; want 401", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not join after cancellation")
	}
}

// policyExecutor scripts the trusted admin ref and one committed policy. The
// admin repository path is asserted from the command seam, so wiring the
// source to any other location fails instead of silently passing.
type policyExecutor struct {
	mu         sync.Mutex
	commands   []git.Command
	adminPath  string
	refCommit  string
	contents   map[string]string
	unexpected []string
}

func (executor *policyExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.commands = append(executor.commands, command)
	joined := strings.Join(command.Args, " ")
	switch {
	case command.Name == "git" && len(command.Args) == 1 && command.Args[0] == "--version":
		return git.CommandResult{Stdout: []byte("git version 2.44.0\n")}, nil
	case strings.Contains(joined, "for-each-ref"):
		if !strings.Contains(joined, executor.adminPath) {
			executor.unexpected = append(executor.unexpected, joined)
			return git.CommandResult{ExitCode: 1}, nil
		}
		if executor.refCommit == "" {
			return git.CommandResult{}, nil
		}
		return git.CommandResult{Stdout: []byte("refs/heads/main " + executor.refCommit + "\n")}, nil
	case strings.Contains(joined, "rev-parse"):
		if strings.Contains(joined, "--show-ref-format") {
			return git.CommandResult{Stdout: []byte("files\n")}, nil
		}
		if strings.Contains(joined, "--verify") {
			// The trusted ref is absent in this scripted state; the explicit
			// --git-dir repository check still succeeds.
			if executor.refCommit == "" {
				return git.CommandResult{ExitCode: 1}, nil
			}
			return git.CommandResult{Stdout: []byte(executor.refCommit + "\n")}, nil
		}
		return git.CommandResult{Stdout: []byte("/git\n")}, nil
	case strings.Contains(joined, "cat-file"):
		for _, argument := range command.Args {
			if argument == "-t" {
				return git.CommandResult{Stdout: []byte("commit\n")}, nil
			}
			if strings.Contains(argument, ":") {
				return executor.read(argument)
			}
		}
	case strings.Contains(joined, "show"):
		for _, argument := range command.Args {
			if strings.Contains(argument, ":") {
				return executor.read(argument)
			}
		}
	}
	return git.CommandResult{}, nil
}

func (executor *policyExecutor) read(spec string) (git.CommandResult, error) {
	content, ok := executor.contents[spec]
	if !ok {
		return git.CommandResult{ExitCode: 128}, nil
	}
	return git.CommandResult{Stdout: []byte(content)}, nil
}

func (executor *policyExecutor) sawUnexpected() []string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]string(nil), executor.unexpected...)
}

func committedPolicyConfig(t *testing.T, commit, policy string) Config {
	t.Helper()
	config := wiredConfig(t)
	adminPath := filepath.Join(config.StorageRoot, string(config.TenantID), "admin")
	// The policy source now proves a genuinely absent trusted ref against the
	// files-backend layout, so the scripted admin path must look like a bare
	// repository with an unborn main rather than an empty directory.
	for _, directory := range []string{"refs/heads", "refs/tags", "objects"} {
		if err := os.MkdirAll(filepath.Join(adminPath, filepath.FromSlash(directory)), 0o700); err != nil {
			t.Fatalf("create admin repository layout: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(adminPath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write admin repository HEAD: %v", err)
	}
	executor := &policyExecutor{
		adminPath: adminPath,
		refCommit: commit,
		contents:  map[string]string{commit + ":conf/git.conf": policy},
	}
	config.Executor = executor
	return config
}

func TestNewUsesCommittedAdminPolicyForAuthorization(t *testing.T) {
	// The accepted admin commit denies the engineer. An app that reads any
	// other policy source, such as a working-tree file, is observably wrong.
	config := committedPolicyConfig(t, "commit-one", "repo repo\nR = @admin\n")
	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decision, err := server.Service().AuthorizeRead(context.Background(), "repo",
		git.Identity{Roles: []string{"engineer"}})
	if err != nil {
		t.Fatalf("AuthorizeRead: %v", err)
	}
	if decision != git.DecisionDeny {
		t.Fatalf("decision = %q; want deny from the accepted admin commit", decision)
	}
	if unexpected := config.Executor.(*policyExecutor).sawUnexpected(); len(unexpected) != 0 {
		t.Fatalf("policy resolved outside the tenant admin repository: %v", unexpected)
	}
}

// TestNewFailsClosedWithoutAnAcceptedAdminRef pins that a provisioned admin
// repository whose trusted ref is absent is a dependency failure, never a
// policy-file fallback.
func TestNewFailsClosedWithoutAnAcceptedAdminRef(t *testing.T) {
	config := committedPolicyConfig(t, "", "")
	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = server.Service().AuthorizeRead(context.Background(), "repo",
		git.Identity{Roles: []string{"engineer"}})
	if err == nil {
		t.Fatal("AuthorizeRead accepted a missing trusted policy ref")
	}
	code, ok := fault.CodeOf(err)
	if !ok || code != fault.CodeDependencyUnavailable {
		t.Fatalf("fault code = %q (%v); want %q", code, ok, fault.CodeDependencyUnavailable)
	}
}

// TestNewFailsClosedWithoutAnAdminRepository pins the other missing dependency:
// a tenant that was never provisioned has no policy and refuses.
func TestNewFailsClosedWithoutAnAdminRepository(t *testing.T) {
	config := wiredConfig(t)
	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = server.Service().AuthorizeRead(context.Background(), "repo",
		git.Identity{Roles: []string{"engineer"}})
	if err == nil {
		t.Fatal("AuthorizeRead accepted a tenant with no admin repository")
	}
	code, ok := fault.CodeOf(err)
	if !ok || code != fault.CodeDependencyUnavailable {
		t.Fatalf("fault code = %q (%v); want %q", code, ok, fault.CodeDependencyUnavailable)
	}
	if message, ok := fault.MessageOf(err); ok && strings.Contains(message, config.StorageRoot) {
		t.Fatalf("dependency fault leaks the storage root: %q", message)
	}
}

func TestNewFailsClosedOnCorruptCommittedAdminPolicy(t *testing.T) {
	config := committedPolicyConfig(t, "commit-one", "repo repo\nRW = @wizard\n")
	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decision, err := server.Service().AuthorizeRead(context.Background(), "repo",
		git.Identity{Roles: []string{"engineer"}})
	if err == nil {
		t.Fatalf("AuthorizeRead accepted a corrupt established policy with decision %q", decision)
	}
	if decision != git.DecisionDeny {
		t.Fatalf("decision = %q; want deny", decision)
	}
}

// fakeObserver implements git.RefObserver.
type fakeObserver struct{}

func (fakeObserver) Observe(context.Context, string, string) (git.RefState, error) {
	return git.RefState{}, nil
}

// TestServerTimeoutDefaults pins the fixed 10s bounds: a constructed server
// starts with a 10s request-header timeout (so one stalled connection cannot
// pin a serving goroutine), and a graceful shutdown is bounded to 10s.
func TestServerTimeoutDefaults(t *testing.T) {
	server, err := New(wiredConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if server.readHeaderTimeout != 10*time.Second {
		t.Fatalf("default read-header timeout = %s; want 10s", server.readHeaderTimeout)
	}
	if shutdownTimeout != 10*time.Second {
		t.Fatalf("shutdown timeout = %s; want 10s", shutdownTimeout)
	}
}

// TestReadHeaderTimeoutZeroFallsBackToTenSeconds pins the zero-value fallback
// itself, not only the constructor's initial value: a Server whose field is
// zeroed (or negative) still serves with the 10s slowloris bound. Without the
// fallback, ReadHeaderTimeout would be zero and http.Server would wait forever.
func TestReadHeaderTimeoutZeroFallsBackToTenSeconds(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value time.Duration
		want  time.Duration
	}{
		{"zero falls back", 0, 10 * time.Second},
		{"negative falls back", -time.Second, 10 * time.Second},
		{"explicit override is kept", 250 * time.Millisecond, 250 * time.Millisecond},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := &Server{readHeaderTimeout: testCase.value}
			if got := server.effectiveReadHeaderTimeout(); got != testCase.want {
				t.Fatalf("effective read-header timeout = %s; want %s", got, testCase.want)
			}
		})
	}
	if defaultReadHeaderTimeout != 10*time.Second {
		t.Fatalf("default read-header timeout constant = %s; want 10s", defaultReadHeaderTimeout)
	}
}

// TestServeAppliesTheFallbackForAZeroedReadHeaderTimeout proves the served
// HTTP server receives the effective (fallback) bound, not the raw field. The
// constructed Server is otherwise complete, but its timeout field is zero: a
// Serve that passed the field through (or built its own http.Server) would
// serve with no header timeout at all, which is exactly the slowloris hole the
// fallback exists to close.
func TestServeAppliesTheFallbackForAZeroedReadHeaderTimeout(t *testing.T) {
	server, err := New(wiredConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server.readHeaderTimeout = 0

	applied := make(chan time.Duration, 1)
	previous := newServedHTTPServer
	newServedHTTPServer = func(handler http.Handler, readHeaderTimeout time.Duration) *http.Server {
		applied <- readHeaderTimeout
		return &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	}
	t.Cleanup(func() { newServedHTTPServer = previous })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	var got time.Duration
	select {
	case got = <-applied:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Serve never constructed the HTTP server")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not join after cancellation")
	}
	if got != defaultReadHeaderTimeout {
		t.Fatalf("served read-header timeout = %s; want the %s fallback for a zero-valued field", got, defaultReadHeaderTimeout)
	}
}

// TestServeClosesAStalledRequestHeader pins the slowloris bound: a client that
// opens a connection and never finishes its request headers is disconnected by
// ReadHeaderTimeout instead of pinning a serving goroutine forever.
func TestServeClosesAStalledRequestHeader(t *testing.T) {
	server, err := New(wiredConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server.readHeaderTimeout = 200 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not join after cancellation")
		}
	}()

	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close()
	// A request line and one header, then nothing: the headers never finish.
	if _, err := connection.Write([]byte("GET /repo/info/refs HTTP/1.1\r\nHost: stalled\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var probe [1]byte
	_, readErr := connection.Read(probe[:])
	if readErr == nil {
		t.Fatal("the stalled connection was not closed")
	}
	var networkError net.Error
	if errors.As(readErr, &networkError) && networkError.Timeout() {
		t.Fatal("the server left a stalled request-header connection open")
	}
}

// scriptedGitInvocation is one recorded native command and its exact child
// environment.
type scriptedGitInvocation struct {
	args string
	env  map[string]string
}

// installScriptedGit writes a PATH `git` that records every invocation's exact
// child environment to recordDir and answers the startup probe plus the
// committed-policy command surface and one `http-backend` CGI exchange.
func installScriptedGit(t *testing.T, recordDir string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"record=" + shellSingleQuote(recordDir) + "\n" +
		"{\n" +
		"  printf 'BEGIN\\n'\n" +
		"  printf 'ARGS'\n" +
		"  for argument in \"$@\"; do printf ' %s' \"$argument\"; done\n" +
		"  printf '\\n'\n" +
		"  env\n" +
		"  printf 'END\\n'\n" +
		"} >> \"$record/invocations\"\n" +
		"if [ \"$1\" = \"--version\" ]; then printf 'git version 2.44.0\\n'; exit 0; fi\n" +
		"for argument in \"$@\"; do\n" +
		"  case \"$argument\" in\n" +
		"    for-each-ref) printf 'refs/heads/main 0123456789012345678901234567890123456789\\n'; exit 0;;\n" +
		"    rev-parse) printf '/scripted/admin\\n'; exit 0;;\n" +
		"    cat-file) cat_file=1;;\n" +
		"    -t) kind=1;;\n" +
		"    blob) blob=1;;\n" +
		"    http-backend) printf 'Status: 200\\r\\nContent-Type: application/x-git-upload-pack-advertisement\\r\\n\\r\\nok'; exit 0;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ \"${cat_file:-}\" = 1 ] && [ \"${kind:-}\" = 1 ]; then printf 'commit\\n'; exit 0; fi\n" +
		"if [ \"${cat_file:-}\" = 1 ] && [ \"${blob:-}\" = 1 ]; then printf 'repo repo\\n    R = @engineer\\n'; exit 0; fi\n" +
		"exit 1\n"
	directory := t.TempDir()
	path := filepath.Join(directory, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write scripted git: %v", err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// shellSingleQuote quotes one literal for a POSIX shell.
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// readScriptedInvocations decodes every recorded invocation.
func readScriptedInvocations(t *testing.T, recordDir string) []scriptedGitInvocation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(recordDir, "invocations"))
	if err != nil {
		t.Fatalf("read scripted git records: %v", err)
	}
	var invocations []scriptedGitInvocation
	current := scriptedGitInvocation{env: map[string]string{}}
	inEnvironment := false
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case line == "BEGIN":
			current = scriptedGitInvocation{env: map[string]string{}}
			inEnvironment = false
		case strings.HasPrefix(line, "ARGS "):
			current.args = strings.TrimPrefix(line, "ARGS ")
			inEnvironment = true
		case line == "END":
			invocations = append(invocations, current)
			inEnvironment = false
		case inEnvironment:
			name, value, ok := strings.Cut(line, "=")
			if ok {
				current.env[name] = value
			}
		}
	}
	return invocations
}

// TestGitServeIsolatesExecutorAndBackendEnvironment pins the production
// construction of both native boundaries: the default command executor
// (including every committed-policy read) and the default `git http-backend`
// each receive provisionGitEnvironment's pinned configuration, drop every
// inherited GIT_* variable and keep only the private hook socket that the
// backend must forward.
func TestGitServeIsolatesExecutorAndBackendEnvironment(t *testing.T) {
	root := t.TempDir()
	recordDir := filepath.Join(root, "records")
	if err := os.MkdirAll(recordDir, 0o700); err != nil {
		t.Fatalf("create record directory: %v", err)
	}
	installScriptedGit(t, recordDir)

	hostileHome := filepath.Join(root, "hostile-home")
	hostileGlobal := filepath.Join(root, "hostile-global")
	hostileSystem := filepath.Join(root, "hostile-system")
	t.Setenv("HOME", hostileHome)
	t.Setenv("GIT_CONFIG_GLOBAL", hostileGlobal)
	t.Setenv("GIT_CONFIG_SYSTEM", hostileSystem)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_DIR", filepath.Join(root, "hostile-repository"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(root, "hostile-worktree"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(root, "hostile-index"))
	t.Setenv("GIT_AUTHOR_NAME", "Hostile Person")
	t.Setenv("GIT_AUTHOR_EMAIL", "hostile@evil.example")

	config := wiredConfig(t)
	adminPath := filepath.Join(config.StorageRoot, string(config.TenantID), "admin")
	for _, directory := range []string{"refs/heads", "refs/tags", "objects"} {
		if err := os.MkdirAll(filepath.Join(adminPath, filepath.FromSlash(directory)), 0o700); err != nil {
			t.Fatalf("create admin repository layout: %v", err)
		}
	}
	repositoryPath := filepath.Join(config.StorageRoot, string(config.TenantID), "repo")
	if err := os.MkdirAll(filepath.Join(repositoryPath, "objects"), 0o700); err != nil {
		t.Fatalf("create repository layout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(adminPath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write admin repository HEAD: %v", err)
	}
	config.Executor = nil
	config.Backend = nil
	config.Actors = &fakeActors{identity: git.Identity{Roles: []string{"engineer"}}}
	socketBase, err := os.MkdirTemp("", "k-")
	if err != nil {
		t.Fatalf("create socket base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketBase) })
	config.HookSocketDirectory = socketBase

	server, err := New(config)
	if err != nil {
		t.Fatalf("New with default executor and backend: %v", err)
	}
	defer func() { _ = server.Close() }()

	request := httptest.NewRequest(http.MethodGet, "/repo/info/refs?service=git-upload-pack", nil)
	request.SetBasicAuth("kodama", "credential-1")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("smart-HTTP status = %d; want 200 (body %q)", response.Code, response.Body.String())
	}

	invocations := readScriptedInvocations(t, recordDir)
	var executorInvocation, backendInvocation *scriptedGitInvocation
	for index := range invocations {
		switch {
		case strings.Contains(invocations[index].args, "--version"):
			executorInvocation = &invocations[index]
		case strings.Contains(invocations[index].args, "http-backend"):
			backendInvocation = &invocations[index]
		}
	}
	if executorInvocation == nil {
		t.Fatal("the default executor never ran the startup probe")
	}
	if backendInvocation == nil {
		t.Fatal("the default backend never ran git http-backend")
	}

	requireIsolatedEnvironment(t, "executor", executorInvocation.env, hostileHome, hostileGlobal, hostileSystem, false)
	requireIsolatedEnvironment(t, "backend", backendInvocation.env, hostileHome, hostileGlobal, hostileSystem, true)
	// The backend's pinned configuration is the keep-ref advertisement hiding,
	// never the hostile inherited GIT_CONFIG_COUNT=1.
	if backendInvocation.env["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("backend GIT_CONFIG_COUNT = %q; want 2", backendInvocation.env["GIT_CONFIG_COUNT"])
	}
	if backendInvocation.env["GIT_CONFIG_KEY_0"] != "uploadpack.hideRefs" ||
		backendInvocation.env["GIT_CONFIG_VALUE_0"] != git.KeepRefNamespace ||
		backendInvocation.env["GIT_CONFIG_KEY_1"] != "transfer.hideRefs" ||
		backendInvocation.env["GIT_CONFIG_VALUE_1"] != git.KeepRefNamespace {
		t.Fatalf("backend did not pin both keep-ref hides: %v", backendInvocation.env)
	}
	if got := backendInvocation.env["KODAMA_HOOK_SOCKET"]; got != server.HookSocketPath() {
		t.Fatalf("backend KODAMA_HOOK_SOCKET = %q; want the preserved hook socket %q", got, server.HookSocketPath())
	}
	if got := backendInvocation.env["GIT_PROJECT_ROOT"]; got == "" {
		t.Fatal("backend environment has no GIT_PROJECT_ROOT")
	}
}

// requireIsolatedEnvironment asserts the exact isolation contract for one
// native child environment.
func requireIsolatedEnvironment(t *testing.T, name string, env map[string]string, hostileHome, hostileGlobal, hostileSystem string, hideKeepRefs bool) {
	t.Helper()
	want := map[string]string{
		"LC_ALL":              "C",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_ATTR_NOSYSTEM":   "1",
		"HOME":                hostileHome,
	}
	for key, value := range want {
		if got := env[key]; got != value {
			t.Errorf("%s environment %s = %q; want %q", name, key, got, value)
		}
	}
	for _, forbidden := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR",
	} {
		if value, ok := env[forbidden]; ok {
			t.Errorf("%s environment inherited %s=%q", name, forbidden, value)
		}
	}
	if !hideKeepRefs {
		if value, ok := env["GIT_CONFIG_COUNT"]; ok {
			t.Errorf("%s environment inherited GIT_CONFIG_COUNT=%q", name, value)
		}
	}
	if env["GIT_CONFIG_GLOBAL"] == hostileGlobal || env["GIT_CONFIG_SYSTEM"] == hostileSystem {
		t.Errorf("%s environment kept a hostile configuration source", name)
	}
	if env["PATH"] == "" {
		t.Errorf("%s environment dropped PATH", name)
	}
}
