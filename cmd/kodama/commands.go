package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/agent"
	agentapp "github.com/optimiware/kodama/internal/userland/agent/app"
	authapp "github.com/optimiware/kodama/internal/userland/app/auth"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/actors"
	"github.com/optimiware/kodama/internal/userland/services"
	servicesapp "github.com/optimiware/kodama/internal/userland/services/app"
)

// newReadVerifier constructs the smart-HTTP read actor verifier. It is a small
// injectable seam, following provisionTenant, so command tests can pin the
// composition without building the public client.
var newReadVerifier = func(verification git.VerificationClient, resourceID func(string) (string, error)) (git.ReadActorVerifier, error) {
	return git.NewReadVerifier(verification, resourceID)
}

// runGitApp starts the composed smart-HTTP Git service. Tests replace it to
// capture the production configuration without opening a listener.
var runGitApp = gitapp.Run

// runAuthApp starts the composed OIDC provider and social login broker. Tests
// replace it to capture the configuration without opening a listener.
var runAuthApp = authapp.Run

// newPublicTransport builds the public HTTP transport used by the Git and agent
// processes. The mounted credential file is read on every attempt.
func newPublicTransport(baseURL, credentialFile string) (*client.HTTPTransport, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("a public base URL is required")
	}
	if strings.TrimSpace(credentialFile) == "" {
		return nil, fmt.Errorf("a mounted credential file is required")
	}
	retry := client.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	if err := retry.Validate(); err != nil {
		return nil, err
	}
	return &client.HTTPTransport{
		BaseURL:      baseURL,
		RoundTripper: http.DefaultTransport,
		Source:       client.FileCredentialSource{Path: credentialFile},
		Retry:        retry,
	}, nil
}

// runGitServeEntry runs `kodama git serve`.
func runGitServeEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("git serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tenant := flags.String("tenant", envOr("KODAMA_GIT_TENANT", ""), "tenant identity")
	listen := flags.String("listen", envOr("KODAMA_GIT_LISTEN", "127.0.0.1:8081"), "smart-HTTP listen address")
	storageRoot := flags.String("storage-root", envOr("KODAMA_GIT_STORAGE_ROOT", ""), "repository storage root")
	journalDirectory := flags.String("journal-dir", envOr("KODAMA_GIT_JOURNAL_DIR", ""), "receive journal directory")
	// The access policy is the committed conf/git.conf of the tenant's
	// provisioned admin repository. There is no --policy flag and no
	// KODAMA_GIT_POLICY environment variable: neither is read, and --policy is
	// an unknown-flag usage error.
	baseURL := flags.String("base-url", envOr("KODAMA_BASE_URL", ""), "public base URL")
	credentialFile := flags.String("credential-file", envOr("KODAMA_CREDENTIAL_FILE", ""), "mounted service credential file")
	defaultMaxBodyBytes, maxBodyEnvErr := envInt64Strict("KODAMA_GIT_MAX_BODY_BYTES", 0)
	defaultMaxResponseBytes, maxResponseEnvErr := envInt64Strict("KODAMA_GIT_MAX_RESPONSE_BYTES", 0)
	maxBodyBytes := flags.Int64("max-body-bytes", defaultMaxBodyBytes, "maximum smart-HTTP request body bytes")
	maxResponseBytes := flags.Int64("max-response-bytes", defaultMaxResponseBytes, "maximum smart-HTTP response bytes")
	timeout := flags.Duration("timeout", envDuration("KODAMA_GIT_TIMEOUT", 0), "per-exchange timeout")
	resources := stringList{}
	flags.Var(&resources, "resource", "repository=resource-id (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if maxBodyEnvErr != nil {
		usage(stderr, maxBodyEnvErr.Error())
		return 2
	}
	if maxResponseEnvErr != nil {
		usage(stderr, maxResponseEnvErr.Error())
		return 2
	}
	if *maxBodyBytes < 0 {
		usage(stderr, "--max-body-bytes must not be negative")
		return 2
	}
	if *maxResponseBytes < 0 {
		usage(stderr, "--max-response-bytes must not be negative")
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if strings.TrimSpace(*tenant) == "" || strings.TrimSpace(*storageRoot) == "" ||
		strings.TrimSpace(*journalDirectory) == "" {
		usage(stderr, "git serve requires --tenant, --storage-root and --journal-dir")
		return 2
	}
	resourceIDs := make(map[string]string, len(resources))
	for _, resource := range resources {
		name, value, ok := strings.Cut(resource, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
			usage(stderr, fmt.Sprintf("invalid --resource %q; want repository=resource-id", resource))
			return 2
		}
		resourceIDs[name] = value
	}
	// Public verification is mandatory: a Git edge that cannot verify its
	// actors and ref updates must refuse startup rather than serve unverified
	// traffic. There is no optional-transport branch.
	if strings.TrimSpace(*baseURL) == "" || strings.TrimSpace(*credentialFile) == "" {
		usage(stderr, "git serve requires --base-url and --credential-file")
		return 2
	}
	transport, err := newPublicTransport(*baseURL, *credentialFile)
	if err != nil {
		usage(stderr, err.Error())
		return 2
	}
	verification, err := gitapp.NewVerificationClient(transport)
	if err != nil {
		fmt.Fprintln(stderr, "kodama: git serve: actor verification is not configured")
		return 1
	}
	// The read verifier resolves the registered resource of the requested
	// tenant-relative repository. A repository without a mapping refuses with
	// the unauthenticated class, so the edge answers the uniform 401 challenge
	// and never reveals whether the repository exists; the repository name is
	// never substituted for its resource identity.
	resolveResource := func(repository string) (string, error) {
		resourceID, ok := resourceIDs[repository]
		if !ok || strings.TrimSpace(resourceID) == "" {
			return "", git.UnregisteredRepositoryError(repository)
		}
		return resourceID, nil
	}
	readVerifier, err := newReadVerifier(verification, resolveResource)
	if err != nil || readVerifier == nil {
		fmt.Fprintln(stderr, "kodama: git serve: actor verification is not configured")
		return 1
	}
	// One registry serves both smart-HTTP admission and the native hooks: the
	// handler places only an opaque handle in the backend environment and the
	// hook resolves it here.
	registry, err := actors.NewRegistry(actors.Config{TenantID: id.TenantID(*tenant)})
	if err != nil {
		fmt.Fprintln(stderr, "kodama: git serve: actor handle registry is not configured")
		return 1
	}
	defer func() { _ = registry.Close() }()
	config := gitapp.Config{
		TenantID:         id.TenantID(*tenant),
		ListenAddress:    *listen,
		StorageRoot:      *storageRoot,
		JournalDirectory: *journalDirectory,
		BaseURL:          *baseURL,
		CredentialFile:   *credentialFile,
		ResourceIDs:      resourceIDs,
		MaxBodyBytes:     *maxBodyBytes,
		MaxResponseBytes: *maxResponseBytes,
		Timeout:          *timeout,
		Transport:        transport,
		Verify:           verification,
		Actors:           readVerifier,
		ActorHandles:     registry,
		HookActors:       registry,
	}
	if err := runGitApp(ctx, config); err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	return 0
}

// envInt64Strict reads one optional non-negative byte-count setting and refuses
// negative or malformed values rather than silently falling back.
func envInt64Strict(name string, fallback int64) (int64, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer byte count", name)
	}
	return parsed, nil
}

// runAgentEntry runs `kodama agent`.
func runAgentEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseURL := flags.String("base-url", envOr("KODAMA_BASE_URL", ""), "public base URL")
	credentialFile := flags.String("credential-file", envOr("KODAMA_CREDENTIAL_FILE", ""), "mounted execution credential file")
	project := flags.String("project", envOr("KODAMA_PROJECT", ""), "project identity")
	task := flags.String("task", envOr("KODAMA_TASK", ""), "task identity")
	execution := flags.String("execution", envOr("KODAMA_EXECUTION", ""), "execution identity")
	model := flags.String("model", envOr("KODAMA_MODEL", ""), "model identity")
	knowledgeRoot := flags.String("knowledge-root", envOr("KODAMA_KNOWLEDGE_ROOT", ""), "rooted knowledge directory")
	checkpointRoot := flags.String("checkpoint-dir", envOr("KODAMA_CHECKPOINT_DIR", ""), "durable checkpoint directory")
	restore := flags.String("restore", "", "checkpoint reference to restore")
	retainRaw := flags.Bool("retain-raw", false, "retain raw model/tool content")
	messages := stringList{}
	knowledge := stringList{}
	tools := stringList{}
	flags.Var(&messages, "message", "run message (repeatable)")
	flags.Var(&knowledge, "knowledge", "knowledge path (repeatable)")
	flags.Var(&tools, "tool", "allowed tool name (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if strings.TrimSpace(*task) == "" || strings.TrimSpace(*execution) == "" || strings.TrimSpace(*model) == "" || len(messages) == 0 {
		usage(stderr, "agent requires --task, --execution, --model and at least one --message")
		return 2
	}
	if strings.TrimSpace(*project) == "" {
		usage(stderr, "agent requires --project")
		return 2
	}
	if strings.TrimSpace(*checkpointRoot) == "" {
		usage(stderr, "agent requires --checkpoint-dir")
		return 2
	}
	if len(knowledge) > 0 && strings.TrimSpace(*knowledgeRoot) == "" {
		usage(stderr, "agent requires --knowledge-root when --knowledge is supplied")
		return 2
	}
	transport, err := newPublicTransport(*baseURL, *credentialFile)
	if err != nil {
		usage(stderr, err.Error())
		return 2
	}
	runInput := agent.RunInput{
		TaskID:      *task,
		ExecutionID: *execution,
		Model:       *model,
		Knowledge:   knowledge,
		Tools:       tools,
		RestoreRef:  *restore,
	}
	for _, message := range messages {
		runInput.Messages = append(runInput.Messages, agent.Message{Role: "user", Content: message})
	}
	config := agentapp.Config{
		Run:            runInput,
		ProjectID:      *project,
		KnowledgeRoot:  *knowledgeRoot,
		CheckpointRoot: *checkpointRoot,
		RetainRaw:      *retainRaw,
		Out:            stdout,
		Transport:      transport,
	}
	if err := agentapp.Run(ctx, config); err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	return 0
}

// runServicesEntry runs `kodama services`.
func runServicesEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("services", flag.ContinueOnError)
	flags.SetOutput(stderr)
	kind := flags.String("kind", envOr("KODAMA_SERVICE_KIND", ""), "service adapter kind")
	service := flags.String("service", envOr("KODAMA_SERVICE_ID", ""), "service identity")
	tenant := flags.String("tenant", envOr("KODAMA_SERVICE_TENANT", ""), "tenant identity")
	project := flags.String("project", envOr("KODAMA_SERVICE_PROJECT", ""), "project identity")
	baseURL := flags.String("base-url", envOr("KODAMA_BASE_URL", ""), "public base URL")
	credentialFile := flags.String("credential-file", envOr("KODAMA_CREDENTIAL_FILE", ""), "mounted service credential file")
	schedule := flags.Duration("schedule", envDuration("KODAMA_SERVICE_SCHEDULE", 0), "timer schedule")
	rescan := flags.Duration("rescan", envDuration("KODAMA_SERVICE_RESCAN", 30*time.Second), "rescan interval")
	stateDir := flags.String("state-dir", envOr("KODAMA_SERVICE_STATE_DIR", ""), "merge coordinator durable state directory")
	gitURL := flags.String("git-url", envOr("KODAMA_SERVICE_GIT_URL", ""), "merge coordinator Git base URL")
	message := flags.String("message", envOr("KODAMA_EMAIL_MESSAGE", ""), "one RFC822 message file, or - for stdin")
	pipeline := flags.String("pipeline", envOr("KODAMA_EMAIL_PIPELINE", ""), "email intake pipeline identity")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if strings.TrimSpace(*message) != "" {
		return runServicesEmailIntake(ctx, *kind, *tenant, *project, *pipeline, *message,
			*baseURL, *credentialFile, stdout, stderr)
	}
	if strings.TrimSpace(*kind) == "" || strings.TrimSpace(*service) == "" ||
		strings.TrimSpace(*tenant) == "" || strings.TrimSpace(*project) == "" {
		usage(stderr, "services requires --kind, --service, --tenant and --project")
		return 2
	}
	if !services.ServiceKind(*kind).Valid() {
		usage(stderr, fmt.Sprintf("unknown service kind %q", *kind))
		return 2
	}
	transport, err := newPublicTransport(*baseURL, *credentialFile)
	if err != nil {
		usage(stderr, err.Error())
		return 2
	}
	if services.ServiceKind(*kind) == services.KindMergeCoordinator {
		if strings.TrimSpace(*stateDir) == "" || strings.TrimSpace(*gitURL) == "" {
			usage(stderr, "merge coordinator requires --state-dir and --git-url")
			return 2
		}
		config := servicesapp.MergeConfig{
			ServiceID:      id.PrincipalID(*service),
			TenantID:       id.TenantID(*tenant),
			ProjectID:      id.ProjectID(*project),
			StateDirectory: *stateDir,
			GitURL:         *gitURL,
			CredentialFile: *credentialFile,
			RescanInterval: *rescan,
			Transport:      transport,
		}
		if err := servicesapp.RunMergeCoordinator(ctx, config); err != nil {
			fmt.Fprintf(stderr, "kodama: %v\n", err)
			return 1
		}
		return 0
	}
	config := servicesapp.Config{
		Kind:           services.ServiceKind(*kind),
		ServiceID:      id.PrincipalID(*service),
		TenantID:       id.TenantID(*tenant),
		ProjectID:      id.ProjectID(*project),
		Schedule:       *schedule,
		RescanInterval: *rescan,
		Transport:      transport,
	}
	if err := servicesapp.Run(ctx, config); err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	return 0
}

// runServicesEmailIntake runs the one-shot `kodama services --kind email`
// intake. The message source is read before the public transport is used, so an
// unreadable source is a local refusal, never an ambiguous mutation.
func runServicesEmailIntake(ctx context.Context, kind, tenant, project, pipeline, message, baseURL, credentialFile string, stdout, stderr io.Writer) int {
	if kind != string(services.KindEmail) {
		usage(stderr, "email intake requires --kind email")
		return 2
	}
	if strings.TrimSpace(pipeline) == "" {
		usage(stderr, "email intake requires --pipeline")
		return 2
	}
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(project) == "" {
		usage(stderr, "email intake requires --tenant and --project")
		return 2
	}
	var source io.Reader = os.Stdin
	if message != "-" {
		file, err := os.Open(message)
		if err != nil {
			fmt.Fprintf(stderr, "kodama: email message: %v\n", err)
			return 1
		}
		defer file.Close()
		source = file
	}
	transport, err := newPublicTransport(baseURL, credentialFile)
	if err != nil {
		usage(stderr, err.Error())
		return 2
	}
	result, err := servicesapp.RunEmailIntake(ctx, servicesapp.EmailIntakeConfig{
		TenantID:   id.TenantID(tenant),
		ProjectID:  id.ProjectID(project),
		PipelineID: pipeline,
		Message:    source,
		Transport:  transport,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	// Only the committed identities are reported, and only to the injected
	// stdout: never the message identity, subject, body, envelope, digest or
	// credential. A failed write is a fixed refusal with a nonzero exit.
	if _, err := fmt.Fprintf(stdout, "task_id=%s execution_id=%s\n", result.TaskID, result.ExecutionID); err != nil {
		fmt.Fprintln(stderr, "kodama: email intake: writing the result failed")
		return 1
	}
	return 0
}
