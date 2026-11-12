package factory

// Container consumption of the factory Git service.
//
// git_service.go composes the host Git smart-HTTP service and registers only
// the host origin. This file adds the half a real execution needs:
//
//   - the deterministic fixture image, built from messaging.PinnedFixtureBase
//     and published by digest through the real local registry, then materialized
//     locally. The image is Git- and curl-capable; the shared base is never
//     tagged, pushed or removed by this fixture.
//   - a workload relay built by newWorkloadEdge. The Git listener stays on
//     loopback; only the relay listens on the host default-route address that
//     rootless pasta maps to host.containers.internal. That workload origin is
//     added to both registered bindings through resource.binding.update.
//   - authored workload commands that materialize the public CA inside
//     /workspace, read /kodama/secrets/KODAMA_TOKEN for each Git authentication
//     and pass it only through Git's --config-env header mechanism. Redirects
//     and tracing are off and TLS verification is never disabled.
//
// Nothing here provisions authority: the Shell still verifies the execution
// credential, the real managed hooks journal and report the update, and the
// fixture only reads those durable facts back.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

const (
	// GitWorkloadRef is the work-branch ref the fixture policy grants the
	// execution role. It is never the protected main ref.
	GitWorkloadRef = "refs/heads/work/i04-g6b"

	gitWorkloadNameSuffix   = "git"
	gitWorkloadImageTag     = "workload"
	gitWorkloadTokenPath    = "/kodama/secrets/KODAMA_TOKEN"
	gitWorkloadCAFile       = "/workspace/kodama-factory-ca.pem"
	gitWorkloadWorkspace    = "/workspace"
	gitWorkloadRepository   = "/workspace/git-workload-repository"
	gitWorkloadProtectedRef = "refs/heads/main"
	gitWorkloadAuthEnv      = "KODAMA_GIT_AUTHORIZATION"
	gitWorkloadBuildTimeout = 10 * time.Minute
	gitWorkloadCleanupLimit = 3 * time.Minute
)

// GitWorkloadImage is the digest-pinned fixture image this fixture built,
// published and materialized locally.
type GitWorkloadImage struct {
	// RunnableReference is the registry repository@sha256 reference the
	// workload action runs.
	RunnableReference string
	ManifestDigest    string
	// TagReference is the mutable registry tag the publish created. It exists
	// only because a push must name a tag; the action uses RunnableReference.
	TagReference string
	Repository   string
	Tag          string
	// LocalTag is the fixture's unique local build name.
	LocalTag string
	// BaseReference is the single pinned SCM base the Containerfile names.
	BaseReference string
	// GitVersion is the workload image's own `git --version` line.
	GitVersion string
}

// cleanupNames returns the exact local names this fixture created. The shared
// base is never included: it is read through the Containerfile FROM and the
// fixture never tags, pushes or removes it.
func (image GitWorkloadImage) cleanupNames() []string {
	names := make([]string, 0, 3)
	for _, name := range []string{image.LocalTag, image.TagReference, image.RunnableReference} {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	return names
}

// gitWorkloadPlan is the pure, pre-effect identity of one workload image and
// its names. It performs no build, pull or registry call.
type gitWorkloadPlan struct {
	ShardID    string
	Base       messaging.FixtureBase
	BaseTag    string
	BaseDigest string
	LocalTag   string
	Repository string
	Marker     string
}

// newGitWorkloadPlan derives fixture-unique image names from the single pinned
// fixture base. An empty identity is refused so two fixtures can never share a
// build tag, registry repository or marker.
func newGitWorkloadPlan(shardID string) (gitWorkloadPlan, error) {
	trimmed := strings.TrimSpace(shardID)
	if trimmed == "" {
		return gitWorkloadPlan{}, errors.New("workload image needs a fixture identity")
	}
	base, err := messaging.PinnedFixtureBase()
	if err != nil {
		return gitWorkloadPlan{}, fmt.Errorf("workload image base: %w", err)
	}
	plan := gitWorkloadPlan{
		ShardID:    trimmed,
		Base:       base,
		BaseTag:    base.Tag,
		BaseDigest: base.Digest,
		LocalTag:   messaging.FixtureLocalTag(trimmed, gitWorkloadNameSuffix),
		Repository: messaging.FixtureRepository(trimmed, gitWorkloadNameSuffix),
		Marker:     "kodama-git-workload " + trimmed + "\n",
	}
	if plan.LocalTag == base.Tag || plan.Repository == base.Reference {
		return gitWorkloadPlan{}, fmt.Errorf("workload image names collide with the shared base")
	}
	return plan, nil
}

// gitWorkload owns the built image, its builder (which tracks every exact name
// the fixture created) and the local registry that serves it.
type gitWorkload struct {
	registry *messaging.Registry
	builder  *messaging.FixtureBuilder
	plan     gitWorkloadPlan
}

// EnsureWorkloadImage builds, publishes and materializes the workload image
// exactly once per fixture. The work is bounded on its own context and every
// created name is removed again when the fixture fails before the image is
// usable. A build, publish or run failure is fatal to the test: an unusable
// workload image must never be handed to a test as if it were runnable.
func (fixture *GitFixture) EnsureWorkloadImage(t *testing.T) *GitWorkloadImage {
	t.Helper()
	if fixture == nil || fixture.h == nil {
		t.Fatalf("factory: workload image needs a Git fixture")
	}
	fixture.workloadMu.Lock()
	defer fixture.workloadMu.Unlock()
	if fixture.Image != nil {
		return fixture.Image
	}
	registry := messaging.StartRegistry(t, false)
	plan, err := newGitWorkloadPlan(registry.Shard.ID)
	if err != nil {
		registry.Shard.Close()
		t.Fatalf("factory: plan the Git workload image: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitWorkloadBuildTimeout)
	defer cancel()
	builder, image, err := buildGitWorkloadImage(ctx, registry, plan, t.Logf)
	if err != nil {
		registry.Shard.Close()
		t.Fatalf("factory: build the Git workload image: %v", err)
	}
	fixture.workload = &gitWorkload{registry: registry, builder: builder, plan: plan}
	fixture.Image = &image
	return fixture.Image
}

// buildGitWorkloadImage resolves the committed base, builds the deterministic
// fixture, publishes it through the real registry and runs the pushed digest
// once to materialize those exact bytes locally. Any failure removes every
// name the attempt created and closes the registry; the shared base is never
// touched.
func buildGitWorkloadImage(ctx context.Context, registry *messaging.Registry, plan gitWorkloadPlan, logf func(string, ...any)) (built *messaging.FixtureBuilder, image GitWorkloadImage, err error) {
	builder := messaging.NewFixtureBuilder(messaging.WithFixtureLogger(logf))
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), gitWorkloadCleanupLimit)
		defer cancel()
		_ = builder.Cleanup(cleanupCtx)
	}
	digest := registry.Shard.RequireImage(ctx, plan.BaseTag)
	if digest != plan.BaseDigest {
		cleanup()
		return nil, GitWorkloadImage{}, fmt.Errorf("fixture base %s resolved to %s; the Containerfile pin is %s",
			plan.BaseTag, digest, plan.BaseDigest)
	}
	base, err := messaging.NewFixtureBase(plan.BaseTag, digest)
	if err != nil {
		cleanup()
		return nil, GitWorkloadImage{}, fmt.Errorf("resolve the immutable fixture base: %w", err)
	}
	imageBuild, err := builder.Build(ctx, base, messaging.FixtureInputs{Marker: plan.Marker}, plan.LocalTag)
	if err != nil {
		cleanup()
		return nil, GitWorkloadImage{}, err
	}
	publication, err := builder.Publish(ctx, registry, imageBuild, plan.Repository, gitWorkloadImageTag)
	if err != nil {
		cleanup()
		return nil, GitWorkloadImage{}, err
	}
	marker, err := builder.RunMarker(ctx, registry, publication.RunnableReference)
	if err != nil {
		cleanup()
		return nil, GitWorkloadImage{}, err
	}
	if strings.TrimSpace(marker) != strings.TrimSpace(plan.Marker) {
		cleanup()
		return nil, GitWorkloadImage{}, fmt.Errorf("published workload image ran marker %q; want %q", marker, strings.TrimSpace(plan.Marker))
	}
	version, err := builder.GitVersion(ctx, imageBuild.Tag)
	if err != nil {
		cleanup()
		return nil, GitWorkloadImage{}, err
	}
	return builder, GitWorkloadImage{
		RunnableReference: publication.RunnableReference,
		ManifestDigest:    publication.ManifestDigest,
		TagReference:      publication.Reference,
		Repository:        publication.Repository,
		Tag:               publication.Tag,
		LocalTag:          imageBuild.Tag,
		BaseReference:     imageBuild.Base.Reference,
		GitVersion:        version,
	}, nil
}

// WorkloadRepositoryURL returns the container-reachable smart-HTTP URL of one
// repository.
func (fixture *GitFixture) WorkloadRepositoryURL(repository string) string {
	return fixture.WorkloadURL + "/" + repository + ".git"
}

// WorkloadClonePushCommands authors the container commands that clone the
// project repository through the workload relay, commit one file and push it to
// ref. The public CA is written inside /workspace by the commands themselves;
// the execution token is read from its product mount for each Git
// authentication and supplied only through --config-env.
func (fixture *GitFixture) WorkloadClonePushCommands(t testing.TB, ref string) ([]string, error) {
	t.Helper()
	if fixture == nil || fixture.h == nil || fixture.h.TLS == nil {
		return nil, errors.New("workload Git commands need the private-CA TLS fixture")
	}
	return workloadClonePushCommands(workloadGitSpec{
		URL:       fixture.WorkloadRepositoryURL(GitProjectRepository),
		CAPEM:     fixture.h.TLS.CAPEM,
		WorkRef:   ref,
		Directory: gitWorkloadRepository,
	})
}

// workloadGitSpec is the complete authored-command input. It carries no
// credential: the commands read the mounted execution token themselves.
type workloadGitSpec struct {
	URL       string
	CAPEM     []byte
	WorkRef   string
	Directory string
}

// workloadClonePushCommands renders the exact fail-fast command list. The
// runtime executes it with /bin/sh -e; it performs no tracing and never writes
// the token anywhere.
func workloadClonePushCommands(spec workloadGitSpec) ([]string, error) {
	if err := validateWorkloadGitURL(spec.URL); err != nil {
		return nil, err
	}
	if err := validateWorkloadCAPEM(spec.CAPEM); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(spec.WorkRef, "refs/heads/work/") {
		return nil, fmt.Errorf("workload ref %q is not a granted work branch", spec.WorkRef)
	}
	if !filepath.IsAbs(spec.Directory) || !strings.HasPrefix(spec.Directory, "/workspace/") {
		return nil, fmt.Errorf("workload repository directory %q is not inside /workspace", spec.Directory)
	}
	directory := shellQuote(spec.Directory)
	// The heredoc terminator is only recognized on its own line, so a PEM that
	// does not end with a newline gets exactly one appended.
	caMaterial := string(spec.CAPEM)
	if !strings.HasSuffix(caMaterial, "\n") {
		caMaterial += "\n"
	}
	commands := []string{
		"cat > " + shellQuote(gitWorkloadCAFile) + " <<'KODAMA_WORKLOAD_CA'\n" + caMaterial + "KODAMA_WORKLOAD_CA",
		"export GIT_SSL_CAINFO=" + shellQuote(gitWorkloadCAFile),
		"export GIT_CONFIG_NOSYSTEM=1",
		"export GIT_CONFIG_GLOBAL=/dev/null",
		"export GIT_ATTR_NOSYSTEM=1",
		"export GIT_TERMINAL_PROMPT=0",
		"export GIT_TRACE=0 GIT_TRACE_PACKET=0 GIT_TRACE_CURL=0 GIT_CURL_VERBOSE=0",
		// The CA option proves the TLS chain with the same trust material Git
		// uses. No --fail: a 401 without an actor credential is expected and
		// this is a TLS trust check, not an authorization probe.
		"curl --silent --show-error --output /dev/null --proto '=https' --cacert \"$GIT_SSL_CAINFO\" " +
			shellQuote(spec.URL+"/info/refs?service=git-upload-pack"),
		// Each invocation reads the mounted token afresh and supplies the
		// derived Basic header only through Git's environment-based config.
		"git_kodama() {\n" +
			"  " + gitWorkloadAuthEnv + "=\"Authorization: Basic $(printf 'kodama:%s' \"$(cat " + shellQuote(gitWorkloadTokenPath) + ")\" | base64 -w0)\" \\\n" +
			"  git -c http.followRedirects=false --config-env=http.extraHeader=" + gitWorkloadAuthEnv + " \"$@\"\n" +
			"}",
		"rm -rf -- " + directory,
		"git_kodama clone --quiet " + shellQuote(spec.URL) + " " + directory,
		"cd " + directory,
		"printf 'git workload commit for %s\\n' " + shellQuote(spec.WorkRef) + " > workload-commit.txt",
		"git add -- workload-commit.txt",
		"git -c user.name=kodama -c user.email=kodama@example.invalid commit --quiet -m 'G6-B workload push'",
		"git_kodama push --quiet origin " + shellQuote("HEAD:"+spec.WorkRef),
		// The same mounted execution credential may not move the protected
		// main ref without the committed approval. The managed hook must
		// refuse it; an accepted push is the authority defect this guard
		// exists for.
		"if git_kodama push --quiet origin " + shellQuote("HEAD:"+gitWorkloadProtectedRef) + " >/dev/null 2>&1; then\n" +
			"  printf 'protected-ref-accepted\\n' >&2\n" +
			"  exit 1\n" +
			"fi",
		"printf 'protected-ref-refused\\n'",
	}
	// The scan runs last, over the whole workspace the workload can write. A
	// hit or a scan error returns non-zero through the pipeline, so the
	// fail-fast action stops before printing its positive control.
	return append(commands, workloadScanCommands(gitWorkloadTokenPath, gitWorkloadWorkspace)...), nil
}

// workloadScanCommands renders the fail-closed credential scan the workload
// runs on its own workspace. The raw token and its Basic encoding are fed to
// grep on stdin (`-f -`); no secret ever appears in an argument. Each scan is a
// pipeline whose last element is a function that returns 0 for no match, 1 for
// a hit and 2 for a scan failure; `set -e` therefore fails the action on either
// a hit or an error, before the positive control prints. The scan covers the
// whole workspace — repository configuration, remote URLs and any copied file —
// and excludes only the public CA file by name.
func workloadScanCommands(tokenPath, workspace string) []string {
	token := shellQuote(tokenPath)
	root := shellQuote(workspace)
	exclude := shellQuote(filepath.Base(gitWorkloadCAFile))
	scan := "scan_workspace_for_credential() {\n" +
		"  scan_label=$1\n" +
		"  shift\n" +
		"  scan_status=0\n" +
		"  grep -rqF \"$@\" -f - --exclude=" + exclude + " " + root + " || scan_status=$?\n" +
		"  case $scan_status in\n" +
		"    0) printf 'repository-config-leak: %s\\n' \"$scan_label\" >&2; return 1 ;;\n" +
		"    1) return 0 ;;\n" +
		"    *) printf 'repository-config-scan-failed: %s status %s\\n' \"$scan_label\" \"$scan_status\" >&2; return 2 ;;\n" +
		"  esac\n" +
		"}"
	return []string{
		// A missing or empty token must fail closed instead of feeding grep an
		// empty pattern list.
		"test -s " + token,
		scan,
		"cat " + token + " | tr -d '\\n' | scan_workspace_for_credential raw-token",
		"cat " + token + " | tr -d '\\n' | sed 's/^/kodama:/' | base64 -w0 | scan_workspace_for_credential basic-token",
		"printf '%s\\n' 'extraHeader' | scan_workspace_for_credential extra-header -i",
		"printf 'repository-config-clean\\n'",
	}
}

// validateWorkloadGitURL accepts only a credential-free HTTPS repository URL.
func validateWorkloadGitURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("workload Git URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("workload Git URL %q is not HTTPS", value)
	}
	if parsed.User != nil {
		return fmt.Errorf("workload Git URL %q carries credentials", value)
	}
	if parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("workload Git URL %q must name only a host, port and repository path", value)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return fmt.Errorf("workload Git URL %q has no repository path", value)
	}
	return nil
}

// validateWorkloadCAPEM requires exactly one parseable certificate.
func validateWorkloadCAPEM(value []byte) error {
	block, rest := pem.Decode(value)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("workload CA material is not a PEM certificate")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("workload CA material carries more than one certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("workload CA certificate: %w", err)
	}
	return nil
}

// shellQuote renders one single-quoted POSIX shell word.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// workloadGitURL returns the container-reachable Git origin for one relay
// port. It is always the host workload alias: advertising a loopback listener
// never reaches a rootless pasta container.
func workloadGitURL(port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("workload Git relay port %d is out of range", port)
	}
	return fmt.Sprintf("https://host.containers.internal:%d", port), nil
}

// addWorkloadTrustedOrigin adds the relay workload origin to both registered
// bindings through the versioned public update and reads the result back. The
// host origin stays registered.
func addWorkloadTrustedOrigin(h *Harness, registration *gitServiceRegistration, hostURL, workloadURL string) error {
	if registration == nil {
		return errors.New("git service registration is missing")
	}
	if strings.TrimSpace(hostURL) == "" || strings.TrimSpace(workloadURL) == "" {
		return errors.New("git service binding origin is empty")
	}
	type bindingUpdate struct {
		id       string
		resource string
		metadata map[string]string
		version  *uint64
	}
	updates := []bindingUpdate{
		{
			id:       gitAdminBindingID,
			resource: gitAdminResourceID,
			metadata: map[string]string{"repository": GitAdminRepository, "default_branch": "main"},
			version:  &registration.adminBindingVersion,
		},
		{
			id:       gitProjectBindingID,
			resource: gitProjectResourceID,
			metadata: map[string]string{"repository": GitProjectRepository, "default_branch": "main"},
			version:  &registration.projectBindingVersion,
		},
		{
			id:       gitDeployBindingID,
			resource: gitDeployResourceID,
			metadata: map[string]string{"repository": GitDeployRepository, "default_branch": "deploy"},
			version:  &registration.deployBindingVersion,
		},
	}
	for _, update := range updates {
		var committed api.ResourceBindingUpdateResult
		if err := gitRest(h, h.ProjectToken, "resource.binding.update", api.ResourceBindingUpdateRequest{
			BindingID:        update.id,
			ResourceID:       update.resource,
			EnforcingService: gitServiceID,
			TrustedOrigins:   []string{hostURL, workloadURL},
			Operations:       []string{gitReadOperation, gitRefUpdateOperation},
			ReportableEvents: []string{gitRefUpdatedReportType},
			Metadata:         update.metadata,
		}, update.version, "workload-origin-"+update.id, &committed); err != nil {
			return err
		}
		*update.version = committed.Version
	}
	for _, bindingID := range []string{gitAdminBindingID, gitProjectBindingID, gitDeployBindingID} {
		var binding api.ResourceBindingReadResult
		if err := gitRest(h, h.ProjectToken, "resource.binding.read", api.ResourceBindingReadRequest{
			BindingID: bindingID,
		}, nil, "", &binding); err != nil {
			return err
		}
		if !hasExactString(binding.TrustedOrigins, hostURL) || !hasExactString(binding.TrustedOrigins, workloadURL) {
			return fmt.Errorf("binding %s trusted origins = %v; want the host and workload origins", bindingID, binding.TrustedOrigins)
		}
	}
	return nil
}

// hasExactString reports whether values contains wanted exactly.
func hasExactString(values []string, wanted string) bool {
	return slices.Contains(values, wanted)
}

// stopWorkload stops the fixture's workload-side support resources. It removes
// no image name: the image names are removed only after the Shell has stopped.
func (fixture *GitFixture) stopWorkload(context.Context) error {
	if fixture == nil {
		return nil
	}
	fixture.workloadMu.Lock()
	workload := fixture.workload
	fixture.workloadMu.Unlock()
	if workload == nil || workload.registry == nil {
		return nil
	}
	workload.registry.Shard.Close()
	return nil
}

// closeEdge joins the Git service Run call and the workload relay. The caller
// must invoke it while the Shell still serves.
func (fixture *GitFixture) closeEdge() error {
	if fixture == nil {
		return nil
	}
	var problems []error
	if fixture.service != nil {
		if err := fixture.service.close(); err != nil {
			problems = append(problems, err)
		}
	}
	if fixture.relay != nil {
		fixture.relay.Close()
	}
	return errors.Join(problems...)
}

// removeWorkloadImages removes every exact name this fixture created and proves
// no fixture name survives. It is idempotent and runs only after the Shell has
// stopped, so no running workload can still reference the image.
func (fixture *GitFixture) removeWorkloadImages(ctx context.Context) error {
	if fixture == nil {
		return nil
	}
	fixture.workloadMu.Lock()
	workload := fixture.workload
	fixture.workloadMu.Unlock()
	if workload == nil || workload.builder == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, gitWorkloadCleanupLimit)
	defer cancel()
	var problems []error
	if err := workload.builder.Cleanup(cleanupCtx); err != nil {
		problems = append(problems, fmt.Errorf("remove workload image names: %w", err))
	}
	prefixes := []string{
		messaging.FixtureLocalPrefix(workload.plan.ShardID),
		workload.registry.Host() + "/" + workload.plan.Repository,
	}
	for _, prefix := range prefixes {
		names, err := workload.builder.LocalImageNames(cleanupCtx, prefix)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if len(names) != 0 {
			problems = append(problems, fmt.Errorf("workload image names with prefix %q remain after cleanup: %v", prefix, names))
		}
	}
	return errors.Join(problems...)
}

// gitFixtureShutdown is the one Close sequence for a Git fixture. The phases
// are ordered because a workload may still be using the service, a hook may
// still need the Shell, and an image must not be removed while a workload can
// still start.
type gitFixtureShutdown struct {
	StopWorkload func(context.Context) error
	CloseEdge    func() error
	CloseShell   func(context.Context) error
	RemoveImages func(context.Context) error
}

// run executes every non-nil phase in order and joins their errors. A failed
// phase never skips a later cleanup phase.
func (shutdown gitFixtureShutdown) run(ctx context.Context) ([]string, error) {
	steps := []struct {
		name string
		call func() error
	}{
		{name: "workload", call: func() error {
			if shutdown.StopWorkload == nil {
				return nil
			}
			return shutdown.StopWorkload(ctx)
		}},
		{name: "edge", call: func() error {
			if shutdown.CloseEdge == nil {
				return nil
			}
			return shutdown.CloseEdge()
		}},
		{name: "shell", call: func() error {
			if shutdown.CloseShell == nil {
				return nil
			}
			return shutdown.CloseShell(ctx)
		}},
		{name: "images", call: func() error {
			if shutdown.RemoveImages == nil {
				return nil
			}
			return shutdown.RemoveImages(ctx)
		}},
	}
	completed := make([]string, 0, len(steps))
	var problems []error
	for _, step := range steps {
		if step.call == nil {
			continue
		}
		if err := step.call(); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", step.name, err))
		}
		completed = append(completed, step.name)
	}
	return completed, errors.Join(problems...)
}
