package factory

// Executable agent workload fixtures.
//
// The fixture builds one fixture-unique image from the harness's pinned workload
// base, copies a statically linked workload binary and a fixture marker into it,
// and runs it as a disposable container. The workload binary has two modes:
//
//   - native composes agentapp.Run with the default PublicModel;
//   - external drives the public model contract directly.
//
// Neither mode receives an injected model port. Both reach the served edge
// through the workload alias with a real issued service credential, so the real
// authorizer, configured model gateway and usage persistence stay in the path.
// The fixture starts no served execution: these are ordinary container
// commands, and no settlement is claimed or simulated.
//
// The image is built and run by immutable identity. The shared base is only read
// through the Containerfile FROM; the fixture never tags, pushes or removes it,
// and cleanup removes exactly the fixture's own name and verifies the base
// image ID is unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/agent"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// WorkloadHost returns the host default-route address a workload edge binds.
// It is the exported form of the harness address helper for fixture services
// that a Shell-launched workload, not the harness itself, must reach.
func WorkloadHost(t testing.TB) string { return workloadListenHost(t) }

const (
	// AgentWorkloadNative runs the native agent application composition.
	AgentWorkloadNative = "native"
	// AgentWorkloadExternal drives the public model contract directly.
	AgentWorkloadExternal = "external"

	// AgentWorkloadCredentialPath is the mounted public credential file inside
	// the workload. It mirrors the native workload's reserved mount.
	AgentWorkloadCredentialPath = "/kodama/secrets/KODAMA_TOKEN"
	// AgentWorkloadCheckpointTokenPath is the mounted external checkpoint
	// service token. It is a fixture capability token, never a product
	// credential.
	AgentWorkloadCheckpointTokenPath = "/kodama/secrets/KODAMA_CHECKPOINT_TOKEN"

	agentWorkloadImagePath      = "/usr/local/bin/kodama-agent-workload"
	agentWorkloadMarkerPath     = "/etc/kodama-agent-workload-marker"
	agentWorkloadCAFilePath     = "/kodama/fixture-ca.pem"
	agentWorkloadBuildTimeout   = 10 * time.Minute
	agentWorkloadRunTimeout     = 3 * time.Minute
	agentWorkloadCleanupLimit   = 2 * time.Minute
	agentWorkloadCredentialMode = os.FileMode(0o600)
)

// agentWorkloadPlan is the pure identity of one fixture image. It performs no
// build or container call.
type agentWorkloadPlan struct {
	FixtureID     string
	Base          runtimesupport.PinnedImage
	Tag           string
	Marker        string
	Containerfile string
}

// newAgentWorkloadPlan derives fixture-unique image names from one pinned base.
// An empty fixture identity, an unsafe identity character or an unpinned base is
// refused so two fixtures can never share a build tag or marker.
func newAgentWorkloadPlan(fixtureID string, base runtimesupport.PinnedImage) (agentWorkloadPlan, error) {
	trimmed := strings.TrimSpace(fixtureID)
	if trimmed == "" {
		return agentWorkloadPlan{}, errors.New("factory: agent workload image needs a fixture identity")
	}
	for index := 0; index < len(trimmed); index++ {
		character := trimmed[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return agentWorkloadPlan{}, fmt.Errorf("factory: agent workload fixture identity %q is not a safe image component", trimmed)
		}
	}
	reference := strings.TrimSpace(base.Reference)
	if reference == "" || !strings.Contains(reference, "@sha256:") {
		return agentWorkloadPlan{}, fmt.Errorf("factory: agent workload base %q is not digest-pinned", reference)
	}
	plan := agentWorkloadPlan{
		FixtureID: trimmed,
		Base:      base,
		Tag:       "localhost/kodama-agent-workload-" + trimmed + ":workload",
		Marker:    "kodama agent workload " + trimmed,
	}
	if plan.Tag == base.Tag || plan.Tag == reference {
		return agentWorkloadPlan{}, fmt.Errorf("factory: agent workload tag %q collides with the shared base", plan.Tag)
	}
	plan.Containerfile = "FROM " + reference + "\n" +
		"LABEL kodama.test/agent-workload=\"" + trimmed + "\"\n" +
		"COPY agentworkload " + agentWorkloadImagePath + "\n" +
		"COPY marker " + agentWorkloadMarkerPath + "\n"
	return plan, nil
}

// AgentWorkloadImage is the built, fixture-unique image.
type AgentWorkloadImage struct {
	// ImageID is the immutable local image identity the fixture asserts before
	// every run.
	ImageID string
	// Digest is the built manifest digest (sha256:<64 hex>) in the local store.
	Digest string
	// Reference is the digest-pinned image reference
	// (localhost/kodama-agent-workload-<id>@sha256:<64 hex>) launched by the
	// fixture and suitable for a container action.
	Reference string
	// Tag is the fixture's unique local build name. It is the only name cleanup
	// removes.
	Tag string
	// BaseReference is the digest-pinned shared base named by the Containerfile.
	BaseReference string
	// BaseImageID is the shared base's identity before the build. Cleanup
	// proves it is unchanged.
	BaseImageID string
	// Marker is the fixture-unique marker baked into the image. The workload
	// echoes it, so a run of the shared base cannot pass.
	Marker string
}

// AgentWorkload builds and runs the fixture image for one harness.
type AgentWorkload struct {
	h     *Harness
	image AgentWorkloadImage

	mu       sync.Mutex
	sequence int
}

// EnsureAgentWorkload builds the fixture image exactly once per call and
// registers cleanup. A build failure is fatal: an unusable workload image is
// never handed to a test as if it were runnable.
func EnsureAgentWorkload(t *testing.T, harness *Harness) *AgentWorkload {
	t.Helper()
	if harness == nil {
		t.Fatalf("factory: agent workload needs a harness")
	}
	plan, err := newAgentWorkloadPlan(harness.FixtureID(), harness.Image)
	if err != nil {
		t.Fatalf("factory: plan the agent workload image: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentWorkloadBuildTimeout)
	defer cancel()
	image, err := buildAgentWorkloadImage(ctx, t, plan)
	if err != nil {
		cleanupAgentWorkloadImage(t, plan, image.BaseImageID)
		t.Fatalf("factory: build the agent workload image: %v", err)
	}
	t.Cleanup(func() { cleanupAgentWorkloadImage(t, plan, image.BaseImageID) })
	return &AgentWorkload{h: harness, image: image}
}

// Image returns the built image identity.
func (workload *AgentWorkload) Image() AgentWorkloadImage { return workload.image }

// buildAgentWorkloadImage compiles the workload binary, builds the image and
// verifies the immutable identity.
func buildAgentWorkloadImage(ctx context.Context, t *testing.T, plan agentWorkloadPlan) (image AgentWorkloadImage, err error) {
	t.Helper()
	directory := t.TempDir()
	binary := filepath.Join(directory, "agentworkload")
	root, err := findModuleRoot()
	if err != nil {
		return AgentWorkloadImage{}, err
	}
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binary, "./test/support/factory/agentworkload")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		return AgentWorkloadImage{}, fmt.Errorf("build the agent workload binary: %w: %s", buildErr, strings.TrimSpace(string(output)))
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return AgentWorkloadImage{}, fmt.Errorf("built agent workload binary %q is not an executable file", binary)
	}
	if err := os.WriteFile(filepath.Join(directory, "marker"), []byte(plan.Marker), 0o644); err != nil {
		return AgentWorkloadImage{}, fmt.Errorf("write the agent workload marker: %w", err)
	}
	containerfile := filepath.Join(directory, "Containerfile")
	if err := os.WriteFile(containerfile, []byte(plan.Containerfile), 0o644); err != nil {
		return AgentWorkloadImage{}, fmt.Errorf("write the agent workload Containerfile: %w", err)
	}
	baseID, err := agentWorkloadImageID(ctx, plan.Base.Reference)
	if err != nil {
		return AgentWorkloadImage{}, err
	}
	iidFile := filepath.Join(directory, "image.id")
	result, err := runtimesupport.RunPodman(ctx, "build",
		"--pull=never", "--network=none", "--no-cache", "--rm",
		"--format", "oci", "--iidfile", iidFile,
		"-t", plan.Tag, "-f", containerfile, directory)
	if err != nil {
		return AgentWorkloadImage{}, fmt.Errorf("build the agent workload image from %s: %w (%s)",
			plan.Base.Reference, err, strings.TrimSpace(string(result.Stderr)))
	}
	identity, err := os.ReadFile(iidFile)
	if err != nil {
		return AgentWorkloadImage{}, fmt.Errorf("read the built agent workload image identity: %w", err)
	}
	imageID := normalizeAgentWorkloadImageID(string(identity))
	if imageID == "" {
		return AgentWorkloadImage{}, errors.New("built agent workload image has no identity")
	}
	if imageID == normalizeAgentWorkloadImageID(baseID) {
		return AgentWorkloadImage{}, fmt.Errorf("agent workload image identity equals the shared base identity %s; the build did not create a distinct image", baseID)
	}
	digest, err := agentWorkloadImageDigest(ctx, plan.Tag)
	if err != nil {
		return AgentWorkloadImage{}, err
	}
	reference, err := agentWorkloadPinnedReference(plan.Tag, digest)
	if err != nil {
		return AgentWorkloadImage{}, err
	}
	resolvedID, err := agentWorkloadImageID(ctx, reference)
	if err != nil {
		return AgentWorkloadImage{}, err
	}
	if resolvedID != imageID {
		return AgentWorkloadImage{}, fmt.Errorf("agent workload pinned reference %s resolves to %s; want %s", reference, resolvedID, imageID)
	}
	return AgentWorkloadImage{
		ImageID:       imageID,
		Digest:        digest,
		Reference:     reference,
		Tag:           plan.Tag,
		BaseReference: plan.Base.Reference,
		BaseImageID:   baseID,
		Marker:        plan.Marker,
	}, nil
}

// agentWorkloadImageDigest reads the manifest digest of one local image.
func agentWorkloadImageDigest(ctx context.Context, reference string) (string, error) {
	result, err := runtimesupport.RunPodman(ctx, "image", "inspect", "--format", "{{.Digest}}", reference)
	if err != nil {
		return "", fmt.Errorf("inspect image digest %s: %w (%s)", reference, err, strings.TrimSpace(string(result.Stderr)))
	}
	digest := strings.TrimSpace(string(result.Stdout))
	if !strings.HasPrefix(digest, "sha256:") || len(strings.TrimPrefix(digest, "sha256:")) != 64 {
		return "", fmt.Errorf("image %s has invalid digest %q", reference, digest)
	}
	return digest, nil
}

// agentWorkloadPinnedReference builds the immutable repository@sha256:<digest>
// reference from a local tag and its resolved digest.
func agentWorkloadPinnedReference(tag, digest string) (string, error) {
	trimmedTag := strings.TrimSpace(tag)
	trimmedDigest := strings.TrimSpace(digest)
	if trimmedTag == "" || !strings.HasPrefix(trimmedDigest, "sha256:") || len(strings.TrimPrefix(trimmedDigest, "sha256:")) != 64 {
		return "", fmt.Errorf("factory: invalid agent workload tag %q or digest %q", tag, digest)
	}
	repository := trimmedTag
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}
	if repository == "" {
		return "", fmt.Errorf("factory: agent workload tag %q has no repository", tag)
	}
	return repository + "@" + trimmedDigest, nil
}

// agentWorkloadImageID reads one local image identity.
func agentWorkloadImageID(ctx context.Context, reference string) (string, error) {
	result, err := runtimesupport.RunPodman(ctx, "image", "inspect", "--format", "{{.Id}}", reference)
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w (%s)", reference, err, strings.TrimSpace(string(result.Stderr)))
	}
	identity := normalizeAgentWorkloadImageID(string(result.Stdout))
	if identity == "" {
		return "", fmt.Errorf("image %s has no identity", reference)
	}
	return identity, nil
}

// normalizeAgentWorkloadImageID reduces an image identity to its bare digest so
// `podman image inspect` output and the build iidfile compare exactly.
func normalizeAgentWorkloadImageID(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "sha256:")
}

// cleanupAgentWorkloadImage removes exactly the fixture's build name and proves
// the shared base still reports its recorded identity.
func cleanupAgentWorkloadImage(t *testing.T, plan agentWorkloadPlan, baseImageID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), agentWorkloadCleanupLimit)
	defer cancel()
	result, err := runtimesupport.RunPodman(ctx, "rmi", "--force", plan.Tag)
	if err != nil {
		detail := strings.ToLower(err.Error() + " " + string(result.Stderr) + " " + string(result.Stdout))
		if !strings.Contains(detail, "no such image") && !strings.Contains(detail, "image not known") && !strings.Contains(detail, "not found") {
			t.Errorf("factory: remove agent workload image %s: %v", plan.Tag, err)
		}
	}
	if baseImageID == "" {
		return
	}
	current, err := agentWorkloadImageID(ctx, plan.Base.Reference)
	if err != nil {
		t.Errorf("factory: re-inspect the shared base %s after workload cleanup: %v", plan.Base.Reference, err)
		return
	}
	if current != baseImageID {
		t.Errorf("factory: shared base %s identity changed from %s to %s; the workload fixture must not mutate it",
			plan.Base.Reference, baseImageID, current)
	}
}

// AgentWorkloadRun is one workload attempt.
type AgentWorkloadRun struct {
	Mode        string
	ProjectID   string
	TaskID      string
	ExecutionID string
	Model       string
	Messages    []string
	RestoreRef  string
	// Checkpoint is the external durable store the workload restores from and
	// commits to.
	Checkpoint *AgentCheckpointService
	// Credential is the issued public API credential the workload mounts.
	Credential string
}

// AgentWorkloadOutput is the workload's structured stdout.
type AgentWorkloadOutput struct {
	Mode                        string `json:"mode"`
	ImageMarker                 string `json:"image_marker"`
	Status                      string `json:"status"`
	CheckpointRef               string `json:"checkpoint_ref,omitempty"`
	RestoredRef                 string `json:"restored_ref,omitempty"`
	Restored                    bool   `json:"restored"`
	RestoredMessages            int    `json:"restored_messages"`
	ToolCalls                   int    `json:"tool_calls"`
	WorkspaceClean              bool   `json:"workspace_clean"`
	CredentialMounted           bool   `json:"credential_mounted"`
	CheckpointCredentialMounted bool   `json:"checkpoint_credential_mounted"`
	Content                     string `json:"content,omitempty"`
	Error                       string `json:"error,omitempty"`
}

// AgentWorkloadResult is one completed workload run.
type AgentWorkloadResult struct {
	ExitCode      int
	Stdout        string
	Stderr        string
	Output        AgentWorkloadOutput
	ContainerName string
	ContainerGone bool
}

// agentWorkloadRunSpec is the complete command-planning input. It carries no
// credential value.
type agentWorkloadRunSpec struct {
	Mode                     string
	ProjectID                string
	TaskID                   string
	ExecutionID              string
	Model                    string
	Messages                 []string
	RestoreRef               string
	APIURL                   string
	CheckpointURL            string
	CredentialFile           string
	CheckpointCredentialFile string
	CAFile                   string
	Labels                   map[string]string
	Sequence                 int
}

// agentWorkloadCommand is one planned podman invocation.
type agentWorkloadCommand struct {
	Name string
	Args []string
}

// newAgentWorkloadCommand renders the exact run command. The credential values
// are passed only so the planner can refuse an empty value and assert it never
// leaks into argv; the command carries mounted file paths instead.
func newAgentWorkloadCommand(image AgentWorkloadImage, spec agentWorkloadRunSpec, apiToken, checkpointToken string) (agentWorkloadCommand, error) {
	switch spec.Mode {
	case AgentWorkloadNative, AgentWorkloadExternal:
	default:
		return agentWorkloadCommand{}, fmt.Errorf("factory: unknown agent workload mode %q", spec.Mode)
	}
	for name, value := range map[string]string{
		"project":               spec.ProjectID,
		"task":                  spec.TaskID,
		"execution":             spec.ExecutionID,
		"model":                 spec.Model,
		"api url":               spec.APIURL,
		"checkpoint url":        spec.CheckpointURL,
		"credential file":       spec.CredentialFile,
		"checkpoint credential": spec.CheckpointCredentialFile,
		"fixture label":         spec.Labels[FixtureLabel],
	} {
		if strings.TrimSpace(value) == "" {
			return agentWorkloadCommand{}, fmt.Errorf("factory: agent workload run has no %s", name)
		}
	}
	if len(spec.Messages) == 0 {
		return agentWorkloadCommand{}, errors.New("factory: agent workload run has no messages")
	}
	if strings.TrimSpace(apiToken) == "" || strings.TrimSpace(checkpointToken) == "" {
		return agentWorkloadCommand{}, errors.New("factory: agent workload run needs both mounted credentials")
	}
	if spec.RestoreRef != "" && !agent.ValidCheckpointRef(spec.RestoreRef) {
		return agentWorkloadCommand{}, fmt.Errorf("factory: agent workload restore reference %q is invalid", spec.RestoreRef)
	}
	if strings.TrimSpace(image.Tag) == "" || strings.TrimSpace(image.ImageID) == "" || strings.TrimSpace(image.Reference) == "" {
		return agentWorkloadCommand{}, errors.New("factory: agent workload image is not built")
	}
	if !strings.Contains(image.Reference, "@sha256:") {
		return agentWorkloadCommand{}, fmt.Errorf("factory: agent workload image reference %q is not digest-pinned", image.Reference)
	}
	name := fmt.Sprintf("kodama-agent-workload-%s-%s-%d", spec.Labels[FixtureLabel], spec.Mode, spec.Sequence)
	labels := make(map[string]string, len(spec.Labels)+1)
	maps.Copy(labels, spec.Labels)
	labels["kodama.test/agent-workload"] = spec.Mode
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := []string{"run", "--rm", "--pull=never", "--name", name}
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args,
		"--volume", spec.CredentialFile+":"+AgentWorkloadCredentialPath+":ro",
		"--volume", spec.CheckpointCredentialFile+":"+AgentWorkloadCheckpointTokenPath+":ro")
	args = append(args, "--volume", spec.CAFile+":"+agentWorkloadCAFilePath+":ro")
	args = append(args,
		image.Reference,
		agentWorkloadImagePath,
		"-mode", spec.Mode,
		"-base-url", spec.APIURL,
		"-checkpoint-url", spec.CheckpointURL,
		"-credential-file", AgentWorkloadCredentialPath,
		"-checkpoint-credential-file", AgentWorkloadCheckpointTokenPath,
		"-project", spec.ProjectID,
		"-task", spec.TaskID,
		"-execution", spec.ExecutionID,
		"-model", spec.Model,
		"-ca-file", agentWorkloadCAFilePath,
	)
	for _, message := range spec.Messages {
		args = append(args, "-message", message)
	}
	if spec.RestoreRef != "" {
		args = append(args, "-restore", spec.RestoreRef)
	}
	for index, argument := range args {
		if strings.Contains(argument, apiToken) || strings.Contains(argument, checkpointToken) {
			return agentWorkloadCommand{}, fmt.Errorf("factory: agent workload argument %d carries a credential value", index)
		}
	}
	return agentWorkloadCommand{Name: name, Args: args}, nil
}

// Run executes one workload attempt in a disposable container and returns its
// structured outcome. A non-zero container exit is reported, not fatal: a
// missing-checkpoint scenario expects it.
func (workload *AgentWorkload) Run(t *testing.T, run AgentWorkloadRun) AgentWorkloadResult {
	t.Helper()
	if workload == nil || workload.h == nil {
		t.Fatalf("factory: agent workload is not built")
	}
	if run.Checkpoint == nil {
		t.Fatalf("factory: agent workload run needs the external checkpoint service")
	}
	if workload.h.TLS == nil {
		t.Fatalf("factory: agent workload run requires the private-CA TLS edge; a cleartext edge is loopback-only and never workload-reachable")
	}
	workload.mu.Lock()
	workload.sequence++
	sequence := workload.sequence
	workload.mu.Unlock()

	directory := t.TempDir()
	credentialFile := filepath.Join(directory, "KODAMA_TOKEN")
	if err := writeAgentWorkloadCredentialFile(credentialFile, run.Credential); err != nil {
		t.Fatalf("factory: write the workload credential: %v", err)
	}
	checkpointFile := filepath.Join(directory, "KODAMA_CHECKPOINT_TOKEN")
	if err := writeAgentWorkloadCredentialFile(checkpointFile, run.Checkpoint.Token()); err != nil {
		t.Fatalf("factory: write the checkpoint credential: %v", err)
	}
	caFile := filepath.Join(directory, "fixture-ca.pem")
	if err := os.WriteFile(caFile, workload.h.TLS.CAPEM, 0o600); err != nil {
		t.Fatalf("factory: write the workload fixture CA: %v", err)
	}
	spec := agentWorkloadRunSpec{
		Mode:                     run.Mode,
		ProjectID:                run.ProjectID,
		TaskID:                   run.TaskID,
		ExecutionID:              run.ExecutionID,
		Model:                    run.Model,
		Messages:                 append([]string(nil), run.Messages...),
		RestoreRef:               run.RestoreRef,
		APIURL:                   workload.h.WorkloadURL,
		CheckpointURL:            run.Checkpoint.WorkloadURL(),
		CredentialFile:           credentialFile,
		CheckpointCredentialFile: checkpointFile,
		CAFile:                   caFile,
		Labels:                   workload.h.fixture.labels(),
		Sequence:                 sequence,
	}
	command, err := newAgentWorkloadCommand(workload.image, spec, run.Credential, run.Checkpoint.Token())
	if err != nil {
		t.Fatalf("factory: plan the agent workload run: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), agentWorkloadRunTimeout)
	defer cancel()
	current, err := agentWorkloadImageID(ctx, workload.image.Reference)
	if err != nil {
		t.Fatalf("factory: re-inspect the agent workload image before the run: %v", err)
	}
	if normalizeAgentWorkloadImageID(current) != normalizeAgentWorkloadImageID(workload.image.ImageID) {
		t.Fatalf("factory: agent workload reference %s resolves to %s; want the fixture identity %s",
			workload.image.Reference, current, workload.image.ImageID)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), agentWorkloadCleanupLimit)
		defer cleanupCancel()
		_, _ = runtimesupport.RunPodman(cleanupCtx, "rm", "--force", command.Name)
	})

	result, _ := runtimesupport.RunPodman(ctx, command.Args...)
	output := AgentWorkloadOutput{}
	if encoded := strings.TrimSpace(string(result.Stdout)); encoded != "" {
		_ = json.Unmarshal([]byte(encoded), &output)
	}
	gone, err := agentWorkloadContainerGone(ctx, command.Name)
	if err != nil {
		t.Fatalf("factory: probe the agent workload container %s: %v", command.Name, err)
	}
	return AgentWorkloadResult{
		ExitCode:      result.ExitCode,
		Stdout:        string(result.Stdout),
		Stderr:        string(result.Stderr),
		Output:        output,
		ContainerName: command.Name,
		ContainerGone: gone,
	}
}

// agentWorkloadContainerGone reports whether no container with this exact name
// remains. A missing container is the disposable attempt's expected end state.
func agentWorkloadContainerGone(ctx context.Context, name string) (bool, error) {
	result, err := runtimesupport.RunPodman(ctx, "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, fmt.Errorf("list containers named %s: %w", name, err)
	}
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		if strings.TrimSpace(line) == name {
			return false, nil
		}
	}
	return true, nil
}

// writeAgentWorkloadCredentialFile writes one owner-only credential file. An
// empty value is refused so a workload never starts unauthenticated.
func writeAgentWorkloadCredentialFile(path, value string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("factory: credential file path is empty")
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("factory: credential value is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the credential directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(value), agentWorkloadCredentialMode); err != nil {
		return fmt.Errorf("write the credential file: %w", err)
	}
	if err := os.Chmod(path, agentWorkloadCredentialMode); err != nil {
		return fmt.Errorf("restrict the credential file: %w", err)
	}
	return nil
}
