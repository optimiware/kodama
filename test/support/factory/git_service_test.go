package factory

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/actors"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// GitRepositoryAdmin and GitRepositoryProject name the two repositories the
// host Git composition provisions and registers. The tests below use the same
// identities the harness uses so a renamed repository cannot pass unnoticed.
func TestWithGitServiceIsOptInAndEnablesTLS(t *testing.T) {
	var options Options
	if options.GitService {
		t.Fatalf("zero Options enabled the Git service")
	}
	if tlsSelected(options) {
		t.Fatalf("zero Options selected TLS")
	}

	WithGitService()(&options)
	if !options.GitService {
		t.Fatalf("WithGitService did not select the Git service")
	}
	if options.TLS {
		t.Fatalf("WithGitService mutated the explicit TLS option instead of deriving it")
	}
	if options.OIDC {
		t.Fatalf("WithGitService enabled OIDC")
	}
	if !tlsSelected(options) {
		t.Fatalf("WithGitService did not select the private-CA TLS edge")
	}

	if !tlsSelected(Options{TLS: true}) {
		t.Fatalf("WithTLS no longer selects TLS")
	}
	if tlsSelected(Options{TLS: false, GitService: false}) {
		t.Fatalf("plain options selected TLS")
	}

	var nilOption Option
	options.apply(nilOption)
	if !options.GitService {
		t.Fatalf("a nil option changed the Git selection")
	}
}

// TestGitServiceConfigUsesProductionVerificationGraph pins the composed graph:
// the public transport and file credential source, the real verification
// client, the shared actor registry, the strict repository-to-resource mapping
// and the untouched product defaults for the backend, executor, observer and
// journal.
func TestGitServiceConfigUsesProductionVerificationGraph(t *testing.T) {
	root := t.TempDir()
	credentialFile := filepath.Join(root, "credential")
	if err := os.WriteFile(credentialFile, []byte("fixture-service-credential\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	listener := listenLoopback(t)
	socketDirectory := filepath.Join(root, "socket")
	socketPath := filepath.Join(socketDirectory, "hook.sock")

	roundTripper := &http.Transport{}
	resourceIDs := map[string]string{
		GitAdminRepository:   "resource-i04-admin",
		GitProjectRepository: "resource-i04-project-web",
	}
	plan, err := newGitServicePlan(gitServicePlanSpec{
		TenantID:         testTenantID(),
		BaseURL:          "https://127.0.0.1:1",
		CredentialFile:   credentialFile,
		RoundTripper:     roundTripper,
		ResourceIDs:      resourceIDs,
		StorageRoot:      filepath.Join(root, "repositories"),
		JournalDirectory: filepath.Join(root, "journal"),
		HookBinary:       filepath.Join(root, "kodama"),
		HookSocketPath:   socketPath,
		Listener:         listener,
	})
	if err != nil {
		t.Fatalf("newGitServicePlan: %v", err)
	}
	t.Cleanup(func() { _ = plan.registry.Close() })

	transport, ok := any(plan.transport).(*client.HTTPTransport)
	if !ok {
		t.Fatalf("plan transport = %T; want *client.HTTPTransport", plan.transport)
	}
	if transport.BaseURL != "https://127.0.0.1:1" {
		t.Fatalf("transport base URL = %q", transport.BaseURL)
	}
	if transport.RoundTripper != roundTripper {
		t.Fatalf("transport round tripper was replaced")
	}
	source, ok := transport.Source.(client.FileCredentialSource)
	if !ok {
		t.Fatalf("transport source = %T; want client.FileCredentialSource", transport.Source)
	}
	if source.Path != credentialFile {
		t.Fatalf("file credential source path = %q; want %q", source.Path, credentialFile)
	}
	wantRetry := client.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	if transport.Retry != wantRetry {
		t.Fatalf("transport retry = %+v; want the production policy %+v", transport.Retry, wantRetry)
	}

	if _, ok := any(plan.verification).(*gitapp.VerificationClient); !ok {
		t.Fatalf("plan verification = %T; want *gitapp.VerificationClient", plan.verification)
	}
	if _, ok := any(plan.readVerifier).(*git.ReadVerifier); !ok {
		t.Fatalf("plan read verifier = %T; want *git.ReadVerifier", plan.readVerifier)
	}
	if _, ok := any(plan.registry).(*actors.Registry); !ok {
		t.Fatalf("plan registry = %T; want *actors.Registry", plan.registry)
	}

	config := plan.config
	if config.TenantID != testTenantID() {
		t.Fatalf("config tenant = %q", config.TenantID)
	}
	if config.BaseURL != "https://127.0.0.1:1" || config.CredentialFile != credentialFile {
		t.Fatalf("config public credentials = %q/%q", config.BaseURL, config.CredentialFile)
	}
	if config.Transport != client.Transport(transport) {
		t.Fatalf("config transport is not the plan's public transport")
	}
	if config.Verify != git.VerificationClient(plan.verification) {
		t.Fatalf("config verification is not the plan's real client")
	}
	if config.Actors != git.ReadActorVerifier(plan.readVerifier) {
		t.Fatalf("config read verifier is not the plan's concrete read verifier")
	}
	// One registry is both the smart-HTTP handle issuer and the hook actor
	// source. Two registries would mint a handle the hook can never resolve.
	if config.ActorHandles != git.ActorHandleIssuer(plan.registry) {
		t.Fatalf("config actor handles are not the shared registry")
	}
	if config.HookActors != git.ActorSource(plan.registry) {
		t.Fatalf("config hook actors are not the shared registry")
	}
	if any(config.ActorHandles) != any(config.HookActors) {
		t.Fatalf("smart HTTP and hooks use different actor registries")
	}
	if config.HookBinary != filepath.Join(root, "kodama") {
		t.Fatalf("config hook binary = %q", config.HookBinary)
	}
	if config.HookSocketPath != socketPath {
		t.Fatalf("config hook socket path = %q; want %q", config.HookSocketPath, socketPath)
	}
	if config.Listener != listener {
		t.Fatalf("config listener was replaced")
	}
	// The product defaults stay in force: an injected executor, backend or
	// observer would replace the real native-Git enforcement point.
	if config.Executor != nil || config.Backend != nil || config.Observer != nil {
		t.Fatalf("config replaced a product default: executor=%v backend=%v observer=%v",
			config.Executor != nil, config.Backend != nil, config.Observer != nil)
	}
	if config.Clock != nil || config.Retention != nil {
		t.Fatalf("config substituted clock or retention")
	}
	if len(config.ResourceIDs) != 2 ||
		config.ResourceIDs[GitAdminRepository] != "resource-i04-admin" ||
		config.ResourceIDs[GitProjectRepository] != "resource-i04-project-web" {
		t.Fatalf("config resource IDs = %#v", config.ResourceIDs)
	}

	// Strict mapping: a repository resolves to its registered resource ID and
	// never to its own name; an unregistered repository refuses.
	resolved, err := plan.resolveResource(GitProjectRepository)
	if err != nil {
		t.Fatalf("resolve registered repository: %v", err)
	}
	if resolved != "resource-i04-project-web" || resolved == GitProjectRepository {
		t.Fatalf("resolved resource = %q; want the registered resource identity", resolved)
	}
	for _, repository := range []string{"project-i04/unregistered", ""} {
		_, err := plan.resolveResource(repository)
		if err == nil {
			t.Fatalf("resolver accepted repository %q", repository)
		}
		// Production refuses an unmapped repository with the unauthenticated
		// class so the edge answers a uniform 401 instead of a 500 that would
		// reveal whether the repository exists.
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("resolve %q error = %v (class %q ok=%v); want fault.Unauthenticated",
				repository, err, class, ok)
		}
		if !errors.Is(err, git.ErrRepositoryUnregistered) {
			t.Fatalf("resolve %q error = %v; want git.ErrRepositoryUnregistered", repository, err)
		}
	}

	// The plan copied the mapping: a later caller mutation cannot change what
	// the served composition resolves.
	resourceIDs[GitProjectRepository] = "resource-mutated"
	if plan.config.ResourceIDs[GitProjectRepository] != "resource-i04-project-web" {
		t.Fatalf("plan aliased the caller's resource mapping")
	}
}

// TestGitServicePlanRefusesAnOverlongHookSocketPath pins the fixture's
// pre-effect socket guard: a hook socket path beyond the product's conservative
// sun_path bound is refused with a named cause instead of failing later inside
// net.Listen with an opaque error.
func TestGitServicePlanRefusesAnOverlongHookSocketPath(t *testing.T) {
	root := t.TempDir()
	credentialFile := filepath.Join(root, "credential")
	if err := os.WriteFile(credentialFile, []byte("fixture-service-credential\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	listener := listenLoopback(t)
	t.Cleanup(func() { _ = listener.Close() })
	overlong := filepath.Join(root, strings.Repeat("a", native.MaxSocketPathBytes))
	plan, err := newGitServicePlan(gitServicePlanSpec{
		TenantID:         testTenantID(),
		BaseURL:          "https://127.0.0.1:1",
		CredentialFile:   credentialFile,
		RoundTripper:     http.DefaultTransport,
		ResourceIDs:      map[string]string{GitAdminRepository: "resource-i04-admin"},
		StorageRoot:      filepath.Join(root, "repositories"),
		JournalDirectory: filepath.Join(root, "journal"),
		HookBinary:       filepath.Join(root, "kodama"),
		HookSocketPath:   overlong,
		Listener:         listener,
	})
	if err == nil {
		_ = plan.registry.Close()
		t.Fatalf("plan accepted a %d-byte hook socket path", len(overlong))
	}
	if !errors.Is(err, ErrHookSocketPathTooLong) {
		t.Fatalf("overlong hook socket error = %v; want ErrHookSocketPathTooLong", err)
	}
}

// TestGitPolicyParsesAndAuthorizesTheFixtureRules pins the served policy the
// integration fixture pushes through the admin repository: it parses with the
// product grammar, lets only the admin fast-forward the trusted policy ref
// without approval, and expresses the engineer/execution split and the main
// approval requirement the hook scenarios rely on.
func TestGitPolicyParsesAndAuthorizesTheFixtureRules(t *testing.T) {
	config, err := git.ParseConfig([]byte(GitPolicy))
	if err != nil {
		t.Fatalf("the fixture policy does not parse: %v", err)
	}
	admin, ok := config.Policy(GitAdminRepository)
	if !ok {
		t.Fatalf("the fixture policy does not declare %q", GitAdminRepository)
	}
	if decision, err := admin.Authorize(git.AccessRequest{
		Identity: git.Identity{Roles: []string{"admin"}},
		Ref:      "refs/heads/main",
		Update:   git.UpdateFastForward,
	}); err != nil || decision != git.DecisionAllow {
		t.Fatalf("admin policy push decision = %q, %v; want allow", decision, err)
	}
	if _, guarded := admin.ApprovalFor("refs/heads/main"); guarded {
		t.Fatalf("the admin policy push requires an approval no human push can supply")
	}

	project, ok := config.Policy(GitProjectRepository)
	if !ok {
		t.Fatalf("the fixture policy does not declare %q", GitProjectRepository)
	}
	cases := []struct {
		name     string
		identity git.Identity
		ref      string
		update   git.UpdateKind
		want     git.Decision
	}{
		{"engineer creates a work branch", git.Identity{Roles: []string{"engineer"}}, "refs/heads/work/g6a", git.UpdateCreate, git.DecisionAllow},
		{"execution creates a work branch", git.Identity{Execution: true}, "refs/heads/work/g6a", git.UpdateCreate, git.DecisionAllow},
		{"engineer cannot update main", git.Identity{Roles: []string{"engineer"}}, "refs/heads/main", git.UpdateFastForward, git.DecisionDeny},
		{"execution updates main", git.Identity{Execution: true}, "refs/heads/main", git.UpdateFastForward, git.DecisionAllow},
	}
	for _, testCase := range cases {
		decision, err := project.Authorize(git.AccessRequest{
			Identity: testCase.identity,
			Ref:      testCase.ref,
			Update:   testCase.update,
		})
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if decision != testCase.want {
			t.Fatalf("%s: decision = %q; want %q", testCase.name, decision, testCase.want)
		}
	}
	role, guarded := project.ApprovalFor("refs/heads/main")
	if !guarded || role != "reviewer" {
		t.Fatalf("main approval = %q/%v; want reviewer/true", role, guarded)
	}
	if decision, err := project.AuthorizeRead(git.ReadRequest{
		Identity: git.Identity{Roles: []string{"engineer"}},
	}); err != nil || decision != git.DecisionAllow {
		t.Fatalf("engineer repository read = %q, %v; want allow", decision, err)
	}
}

// TestGitServicePlanWrapsTheListenerWithTLS pins the served transport: the
// composition hands the TLS wrapper, not the raw listener, to the product Run
// entry point, so the HTTPS host URL is never plain HTTP.
func TestGitServicePlanWrapsTheListenerWithTLS(t *testing.T) {
	root := t.TempDir()
	credentialFile := filepath.Join(root, "credential")
	if err := os.WriteFile(credentialFile, []byte("fixture-service-credential\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	tlsFixture := newTLSFixture(t, "127.0.0.1")
	raw := listenLoopback(t)
	plan, err := newGitServicePlan(gitServicePlanSpec{
		TenantID:         testTenantID(),
		BaseURL:          "https://" + raw.Addr().String(),
		CredentialFile:   credentialFile,
		RoundTripper:     tlsFixture.Client().Transport,
		ResourceIDs:      map[string]string{GitAdminRepository: "resource-i04-admin"},
		StorageRoot:      filepath.Join(root, "repositories"),
		JournalDirectory: filepath.Join(root, "journal"),
		HookBinary:       filepath.Join(root, "kodama"),
		HookSocketPath:   filepath.Join(root, "socket", "hook.sock"),
		Listener:         raw,
		TLS:              tlsFixture.ServerConfig(),
	})
	if err != nil {
		t.Fatalf("newGitServicePlan: %v", err)
	}
	t.Cleanup(func() { _ = plan.registry.Close() })
	if plan.config.Listener == raw {
		t.Fatalf("the plan handed the raw listener to Run; the advertised HTTPS URL would be plain HTTP")
	}

	served := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}),
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = served.Serve(plan.config.Listener) }()
	t.Cleanup(func() { _ = served.Close() })

	response, err := tlsFixture.Client().Get("https://" + raw.Addr().String() + "/")
	if err != nil {
		t.Fatalf("CA-trusting HTTPS request failed: %v", err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read HTTPS response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close HTTPS response: %v", err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTPS status = %d; want 204", response.StatusCode)
	}

	// Plain HTTP bytes must not be accepted as an HTTP exchange.
	connection, err := net.DialTimeout("tcp", raw.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial the TLS listener: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("GET / HTTP/1.1\r\nHost: fixture\r\n\r\n")); err != nil {
		t.Fatalf("write plain HTTP bytes: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, err := io.ReadAll(io.LimitReader(connection, 1024))
	if err == nil && strings.HasPrefix(string(data), "HTTP/1.1 204") {
		t.Fatalf("the Git listener served a plain HTTP request with the fixture handler: %q", string(data))
	}
	if err == nil && strings.HasPrefix(string(data), "HTTP/") && !strings.Contains(string(data), "HTTPS server") {
		t.Fatalf("the Git listener answered a plain HTTP request unexpectedly: %q", string(data))
	}
}

// TestGitServicePlanRequiresPrivateCredentialFile pins the credential file
// handling: a world-readable file is refused instead of silently used.
func TestGitServicePlanRequiresPrivateCredentialFile(t *testing.T) {
	root := t.TempDir()
	listener := listenLoopback(t)
	makePlan := func(credentialFile string) (*gitServicePlan, error) {
		return newGitServicePlan(gitServicePlanSpec{
			TenantID:         testTenantID(),
			BaseURL:          "https://127.0.0.1:1",
			CredentialFile:   credentialFile,
			RoundTripper:     http.DefaultTransport,
			ResourceIDs:      map[string]string{GitAdminRepository: "resource-i04-admin"},
			StorageRoot:      filepath.Join(root, "repositories"),
			JournalDirectory: filepath.Join(root, "journal"),
			HookBinary:       filepath.Join(root, "kodama"),
			HookSocketPath:   filepath.Join(root, "socket", "hook.sock"),
			Listener:         listener,
		})
	}

	exposed := filepath.Join(root, "exposed")
	if err := os.WriteFile(exposed, []byte("fixture-credential\n"), 0o644); err != nil {
		t.Fatalf("write exposed credential: %v", err)
	}
	if plan, err := makePlan(exposed); err == nil {
		_ = plan.registry.Close()
		t.Fatalf("plan accepted a world-readable credential file")
	}

	private := filepath.Join(root, "private")
	if err := os.WriteFile(private, []byte("fixture-credential\n"), 0o600); err != nil {
		t.Fatalf("write private credential: %v", err)
	}
	plan, err := makePlan(private)
	if err != nil {
		t.Fatalf("plan refused a private credential file: %v", err)
	}
	_ = plan.registry.Close()
}

// TestGitServiceRejectsKubernetesBeforeProvisioning pins the bounded runtime
// selection: a Kubernetes factory refuses WithGitService before any
// provisioning step instead of handing the workload a host-local reference.
func TestGitServiceRejectsKubernetesBeforeProvisioning(t *testing.T) {
	err := checkGitRuntimeVariant(runtimeVariantKubernetes, true)
	if err == nil {
		t.Fatalf("Kubernetes factory accepted the Git service")
	}
	if !strings.Contains(err.Error(), "Podman") {
		t.Fatalf("refusal %q does not name the supported runtime", err.Error())
	}
	if err := checkGitRuntimeVariant(runtimeVariantPodman, true); err != nil {
		t.Fatalf("Podman factory refused the Git service: %v", err)
	}
	if err := checkGitRuntimeVariant(runtimeVariantKubernetes, false); err != nil {
		t.Fatalf("Kubernetes factory without the Git service refused: %v", err)
	}
}

// TestGitServiceStartupFailureClosesOwnedResources pins the failed-start
// cleanup: a Run that exits before readiness closes the registry, the listener
// and the private socket directory, and reports the startup cause.
func TestGitServiceStartupFailureClosesOwnedResources(t *testing.T) {
	root := t.TempDir()
	credentialFile := filepath.Join(root, "credential")
	if err := os.WriteFile(credentialFile, []byte("fixture-service-credential\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	socketDirectory := filepath.Join(root, "socket")
	if err := os.MkdirAll(socketDirectory, 0o700); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	listener := listenLoopback(t)
	plan, err := newGitServicePlan(gitServicePlanSpec{
		TenantID:         testTenantID(),
		BaseURL:          "https://127.0.0.1:1",
		CredentialFile:   credentialFile,
		RoundTripper:     http.DefaultTransport,
		ResourceIDs:      map[string]string{GitAdminRepository: "resource-i04-admin"},
		StorageRoot:      filepath.Join(root, "repositories"),
		JournalDirectory: filepath.Join(root, "journal"),
		HookBinary:       filepath.Join(root, "kodama"),
		HookSocketPath:   filepath.Join(socketDirectory, "hook.sock"),
		Listener:         listener,
	})
	if err != nil {
		t.Fatalf("newGitServicePlan: %v", err)
	}

	startupCause := errors.New("fixture startup refused")
	run := func(context.Context, gitapp.Config) error { return startupCause }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = launchGitService(ctx, plan, nil, run, func(context.Context) error { return errors.New("not ready") }, time.Second)
	if err == nil {
		t.Fatalf("launchGitService accepted a failed startup")
	}
	if !errors.Is(err, startupCause) {
		t.Fatalf("startup error = %v; want the Run cause", err)
	}
	if _, err := plan.registry.IssueActor(context.Background(), "probe"); !errors.Is(err, actors.ErrClosed) {
		t.Fatalf("registry after failed startup: %v; want ErrClosed", err)
	}
	if _, err := listener.Accept(); err == nil {
		t.Fatalf("listener still accepts after failed startup")
	}
	if _, err := os.Stat(socketDirectory); !os.IsNotExist(err) {
		t.Fatalf("socket directory survived failed startup: %v", err)
	}
}

// TestGitServiceCloseJoinsBeforeShellShutdown pins the shutdown order: Close
// cancels and joins the Git service before the Shell endpoint is torn down, a
// non-joining Run is reported as an error, and Close is idempotent.
func TestGitServiceCloseJoinsBeforeShellShutdown(t *testing.T) {
	plan := testPlan(t)
	shellDone := make(chan struct{})
	joined := make(chan struct{})
	run := func(ctx context.Context, _ gitapp.Config) error {
		<-ctx.Done()
		close(joined)
		return nil
	}
	service, err := launchGitService(context.Background(), plan, shellDone, run,
		func(context.Context) error { return nil }, 5*time.Second)
	if err != nil {
		t.Fatalf("launchGitService: %v", err)
	}
	select {
	case <-joined:
		t.Fatalf("Run joined before Close")
	default:
	}
	if err := service.close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-joined:
	default:
		t.Fatalf("Close returned before the Run goroutine joined")
	}
	if service.ShellStoppedFirst() {
		t.Fatalf("Git shut down after the Shell endpoint was already closed")
	}
	// The Shell endpoint is still usable at this point in the harness Close
	// sequence, so closing it now must not change the recorded order.
	close(shellDone)
	if service.ShellStoppedFirst() {
		t.Fatalf("recorded Shell shutdown order changed after the fact")

	}
	if err := service.close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// A Run that ignores cancellation is bounded and reported, never joined
	// silently, and it must not be treated as a successful shutdown.
	blockedPlan := testPlan(t)
	release := make(chan struct{})
	stuck := func(context.Context, gitapp.Config) error {
		<-release
		return nil
	}
	stuckService, err := launchGitService(context.Background(), blockedPlan, nil, stuck,
		func(context.Context) error { return nil }, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("launch stuck service: %v", err)
	}
	err = stuckService.close()
	if err == nil || !strings.Contains(err.Error(), "did not join") {
		t.Fatalf("Close of a non-joining Run = %v; want a join failure", err)
	}
	close(release)
}

// testPlan builds a minimal valid plan for the lifecycle tests. Its listener
// and registry are closed by the caller through Close.
func testPlan(t *testing.T) *gitServicePlan {
	t.Helper()
	root := t.TempDir()
	credentialFile := filepath.Join(root, "credential")
	if err := os.WriteFile(credentialFile, []byte("fixture-service-credential\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	listener := listenLoopback(t)
	plan, err := newGitServicePlan(gitServicePlanSpec{
		TenantID:         testTenantID(),
		BaseURL:          "https://127.0.0.1:1",
		CredentialFile:   credentialFile,
		RoundTripper:     http.DefaultTransport,
		ResourceIDs:      map[string]string{GitAdminRepository: "resource-i04-admin"},
		StorageRoot:      filepath.Join(root, "repositories"),
		JournalDirectory: filepath.Join(root, "journal"),
		HookBinary:       filepath.Join(root, "kodama"),
		HookSocketPath:   filepath.Join(root, "socket", "hook.sock"),
		Listener:         listener,
	})
	if err != nil {
		t.Fatalf("newGitServicePlan: %v", err)
	}
	t.Cleanup(func() { _ = plan.registry.Close() })
	return plan
}

// listenLoopback binds one loopback listener the plan can serve.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// testTenantID returns the fixture tenant identity.
func testTenantID() id.TenantID { return id.TenantID(TenantID) }
