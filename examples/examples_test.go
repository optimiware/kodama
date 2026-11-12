// Package examples_test pins the example contract.
//
// The examples are static, API-postable fixtures: every shipped pipeline YAML
// is decoded and validated by the real admission parser (`pipeline.Decode`)
// and every container image is checked against the immutable-reference seam
// (`pipeline.ParseImageReference`). Request assignments are checked against
// the declared grammar (`role/<current human role>` or `service/<service
// id>`). Knowledge Markdown is checked against the knowledge bounds and
// credential refusal shapes without importing the Userland skill package.
// Delivery prose is checked for the pinned-image, external-registry,
// digest-attribution, retry and readiness statements.
//
// Declarations pinned here:
//   - examples/pipelines/{plain,external-harness,planned-review,task-handoff,
//     service-response,push-test,build,test,promote,rollback,readiness}.yaml
//     exist, decode, and satisfy the structural expectations below. The
//     inventory also accepts .yml and is exact, so a deleted or silently
//     skipped example fails.
//   - every container image is `repository@sha256:<64 hex>`.
//   - request assignments use exactly the declared role/service grammar.
//   - examples/testdata/invalid/*.yaml (and .yml) are minimal negative fixtures
//     that the real parser must refuse with fault.InvalidInput.
//   - examples/knowledge/** is bounded Markdown with no credential material.
//   - examples/delivery/README.md carries the registry, digest, retry,
//     idempotency, KODAMA_* context and readiness statements.
package examples_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// requiredPipelines is the shipped pipeline example inventory.
// It is an exact manifest: every entry must exist (a deletion fails), every
// shipped .yaml or .yml file must be listed, and every listed file is decoded
// and validated. Extra example pipelines are permitted only by adding them to
// this inventory.
var requiredPipelines = []string{
	"plain.yaml",
	"external-harness.yaml",
	"planned-review.yaml",
	"task-handoff.yaml",
	"service-response.yaml",
	"push-test.yaml",
	"build.yaml",
	"test.yaml",
	"promote.yaml",
	"rollback.yaml",
	"readiness.yaml",
}

// TestExamplePipelinesDecode is the shared gate: every shipped example is a
// valid admission definition with pinned images and the declared assignment
// grammar.
func TestExamplePipelinesDecode(t *testing.T) {
	shipped := sortedExampleYAML(t, "pipelines")
	if len(shipped) == 0 {
		t.Fatalf("examples/pipelines has no YAML examples; want %v", requiredPipelines)
	}
	requireSameExampleInventory(t, "examples/pipelines", shipped, requiredPipelines)
	for _, name := range shipped {
		rel := "pipelines/" + name
		t.Run(name, func(t *testing.T) {
			decoded := decodeExample(t, rel)
			if len(decoded.Actions) == 0 {
				t.Fatalf("%s declares no actions", rel)
			}
			assertPinnedImages(t, rel, decoded)
			for _, action := range decoded.Actions {
				if action.Request != nil {
					assertAssignment(t, rel, action)
				}
			}
		})
	}
}

// TestExampleInventoryCoversYAMLSpellingsAndDeletions pins the inventory
// contract: the .yml spelling the real `kodactl pipeline apply` accepts is
// enumerated exactly like .yaml, dotfiles and unrelated files are ignored, and
// a deleted or unexpected shipped example is reported by name instead of being
// silently dropped from validation.
func TestExampleInventoryCoversYAMLSpellingsAndDeletions(t *testing.T) {
	t.Run("yaml and yml spellings", func(t *testing.T) {
		directory := t.TempDir()
		writeExampleFile := func(name, body string) {
			t.Helper()
			path := filepath.Join(directory, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("create test directory for %s: %v", name, err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("write test fixture %s: %v", name, err)
			}
		}
		writeExampleFile("alpha.yaml", "actions: []\n")
		writeExampleFile("beta.yml", "actions: []\n")
		writeExampleFile(".hidden.yml", "actions: []\n")
		writeExampleFile("notes.txt", "not an example\n")
		writeExampleFile("nested/gamma.yml", "actions: []\n")

		got := sortedFilesWithExtensions(t, directory, ".yaml", ".yml")
		requireSameExampleInventory(t, "temporary example inventory",
			got, []string{"alpha.yaml", "beta.yml"})
	})

	t.Run("shipped inventory and deletions", func(t *testing.T) {
		shipped := sortedExampleYAML(t, "pipelines")
		requireSameExampleInventory(t, "examples/pipelines", shipped, requiredPipelines)

		// A deletion removes exactly one manifest entry. The check must name it
		// rather than accept the shorter inventory.
		if missing := exampleMissing(requiredPipelines, shipped[1:]); len(missing) != 1 {
			t.Fatalf("a deleted shipped example reported %v; want the one missing entry", missing)
		}
	})
}

// TestPlainExampleIsTaskFreeContainerWork checks the plain, task-free example:
// no task, no declared inputs, ordinary container actions only.
func TestPlainExampleIsTaskFreeContainerWork(t *testing.T) {
	const rel = "pipelines/plain.yaml"
	decoded := decodeExample(t, rel)
	assertInputs(t, rel, decoded)
	if len(decoded.Actions) == 0 {
		t.Fatalf("%s declares no actions", rel)
	}
	for _, action := range decoded.Actions {
		if actionKind(t, rel, action) != "container" {
			t.Fatalf("%s action %q must be a container action for task-free work", rel, action.Name)
		}
		if len(action.Container.Commands) == 0 {
			t.Fatalf("%s action %q declares no commands", rel, action.Name)
		}
	}
}

// TestExternalHarnessRunsOrdinaryCommands checks that the external harness
// example is an ordinary container command, never a runner declaration.
func TestExternalHarnessRunsOrdinaryCommands(t *testing.T) {
	const rel = "pipelines/external-harness.yaml"
	decoded := decodeExample(t, rel)
	for _, action := range decoded.Actions {
		if actionKind(t, rel, action) != "container" {
			t.Fatalf("%s action %q must be a container action", rel, action.Name)
		}
		if len(action.Container.Commands) == 0 {
			t.Fatalf("%s action %q declares no commands; an external harness is an ordinary command", rel, action.Name)
		}
	}
}

// TestPlannedReviewDeclaresApprovalRequest checks the planned-review example: a
// request action assigned to a current human role, with a fixed operation,
// resource and an earlier candidate output in its immutable subject.
func TestPlannedReviewDeclaresApprovalRequest(t *testing.T) {
	const rel = "pipelines/planned-review.yaml"
	decoded := decodeExample(t, rel)
	var review *definition.Action
	for index := range decoded.Actions {
		if decoded.Actions[index].Request != nil {
			if review != nil {
				t.Fatalf("%s declares more than one request action", rel)
			}
			review = &decoded.Actions[index]
		}
	}
	if review == nil {
		t.Fatalf("%s declares no planned review request action", rel)
	}
	assertAssignment(t, rel, *review)
	operation := literalField(t, rel, *review, "operation")
	if operation == "" {
		t.Fatalf("%s review request has an empty operation", rel)
	}
	resource := subjectField(t, rel, *review, "resource")
	switch {
	case resource.Kind == definition.SubjectLiteral && resource.Literal != "":
		assertResourceID(t, rel, "resource", resource.Literal)
	case resource.Kind == definition.SubjectReference && resource.Reference.Input:
		// A planned review may name the resource from a declared input.
	default:
		t.Fatalf("%s review subject resource must be a literal or a declared input reference", rel)
	}

	foundOutput := false
	for _, field := range review.Request.Subject.Fields() {
		if field.Kind == definition.SubjectReference && !field.Reference.Input {
			foundOutput = true
			break
		}
	}
	if !foundOutput {
		t.Fatalf("%s review subject must reference an earlier action output as the reviewed candidate", rel)
	}
}

// TestTaskHandoffReferencesDurableContext checks that the task handoff example
// is ordinary container work: it persists durable context first and then asks
// through the task-ask operation using KODAMA_TASK and durable references. An
// ask is a conversation message that cancels the current execution; it creates
// no request action, carries no approval evidence, and cannot rely on
// disposable workspace state.
func TestTaskHandoffReferencesDurableContext(t *testing.T) {
	const rel = "pipelines/task-handoff.yaml"
	decoded := decodeExample(t, rel)
	if len(decoded.Actions) < 2 {
		t.Fatalf("%s declares %d actions; want a persist action and an ask action", rel, len(decoded.Actions))
	}
	for _, action := range decoded.Actions {
		if action.Request != nil {
			t.Fatalf("%s action %q is a request action; a conversation ask creates no request action", rel, action.Name)
		}
		if actionKind(t, rel, action) != "container" {
			t.Fatalf("%s action %q must be a container action", rel, action.Name)
		}
		if len(action.Container.Commands) == 0 {
			t.Fatalf("%s action %q declares no commands", rel, action.Name)
		}
	}
	if len(decoded.Inputs) == 0 {
		t.Fatalf("%s declares no durable context inputs", rel)
	}

	raw := string(readExample(t, rel))
	body := strings.ToLower(raw)
	for _, input := range decoded.Inputs {
		materialized := "/kodama/inputs/" + string(input)
		if !strings.Contains(raw, materialized) {
			t.Fatalf("%s must reference the admitted durable input %s", rel, materialized)
		}
	}

	persist := decoded.Actions[0]
	if len(persist.Container.Outputs) == 0 {
		t.Fatalf("%s action %q must declare a durable reference output", rel, persist.Name)
	}
	ask := decoded.Actions[len(decoded.Actions)-1]
	commands := strings.Join(ask.Container.Commands, "\n")
	if !strings.Contains(commands, "$KODAMA_TASK") {
		t.Fatalf("%s ask action must target the current task through $KODAMA_TASK", rel)
	}
	// The real CLI dispatches served operations only through `call`; there is no
	// `kodactl task ...` subcommand.
	if !strings.Contains(commands, "kodactl call task.ask") {
		t.Fatalf("%s ask action must invoke the served operation as `kodactl call task.ask`", rel)
	}
	if strings.Contains(commands, "task ask") {
		t.Fatalf("%s ask action invokes the nonexistent `kodactl task ask` subcommand", rel)
	}
	// /kodama/task.json carries no lifecycle version, so the example must read
	// the current task first and pass the returned version as expected-version.
	if !strings.Contains(commands, `kodactl call task.read`) {
		t.Fatalf("%s ask action must read the current task with `kodactl call task.read`; /kodama/task.json carries no lifecycle version", rel)
	}
	if !strings.Contains(commands, `--expected-version="$task_version"`) {
		t.Fatalf("%s ask action must pass the read task version as --expected-version", rel)
	}
	if !strings.Contains(commands, "--input=") {
		t.Fatalf("%s ask action must pass its JSON input with --input=<json>", rel)
	}
	for _, field := range []string{`"task_id"`, `"question"`, `"assignment"`, `"references"`} {
		if !strings.Contains(commands, field) {
			t.Fatalf("%s ask action input must carry the %s field", rel, field)
		}
	}
	for _, reference := range []string{`"commit=%s"`, `"digest=%s"`} {
		if !strings.Contains(commands, reference) {
			t.Fatalf("%s ask action input must carry the durable %s reference", rel, reference)
		}
	}
	// A stable key derived from the task and the durable context makes an
	// identical retry converge instead of asking twice.
	if !strings.Contains(commands, `--idempotency-key="ask:${KODAMA_TASK}:$(cat /kodama/outputs/persist/context_commit)"`) {
		t.Fatalf("%s ask action must derive a stable --idempotency-key from $KODAMA_TASK and the durable context commit", rel)
	}
	for _, output := range persist.Container.Outputs {
		materialized := "/kodama/outputs/" + string(persist.Name) + "/" + string(output)
		if !strings.Contains(commands, materialized) {
			t.Fatalf("%s ask action must pass the durable reference %s", rel, materialized)
		}
	}
	if !strings.Contains(body, "requires a task-associated execution") {
		t.Fatalf("%s must state that it requires a task-associated execution", rel)
	}
	for _, marker := range []string{"/api/v1/tasks/", "/ask"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("%s must document the task-ask operation (%q)", rel, marker)
		}
	}
	if !strings.Contains(body, "cancels this execution") {
		t.Fatalf("%s must state that an ask cancels the current execution", rel)
	}

	for _, disposable := range []string{"/workspace/", "file://", "/tmp/", "/var/tmp/"} {
		if strings.Contains(raw, disposable) {
			t.Fatalf("%s references disposable path %q; handoffs carry durable references", rel, disposable)
		}
	}
}

// TestExampleHandoffReadsVersionFromJSONOutput pins that the handoff example
// reads the task lifecycle version from the real kodactl JSON rendering.
//
// kodactl renders JSON by writing the exact result body, while the default
// table rendering writes `lifecycle_version<TAB>7`. The example's extraction
// expression only matches the JSON spelling, so the task.read invocation must
// pin JSON output; without that pin task_version stays empty, the action exits
// with "the current task version is unavailable" and the ask is never sent.
func TestExampleHandoffReadsVersionFromJSONOutput(t *testing.T) {
	const rel = "pipelines/task-handoff.yaml"
	decoded := decodeExample(t, rel)
	ask := decoded.Actions[len(decoded.Actions)-1]
	commands := strings.Join(ask.Container.Commands, "\n")

	readIndex := strings.Index(commands, "kodactl call task.read")
	if readIndex < 0 {
		t.Fatalf("%s must read the current task with `kodactl call task.read`", rel)
	}
	if askIndex := strings.Index(commands, "kodactl call task.ask"); askIndex < 0 || askIndex < readIndex {
		t.Fatalf("%s must read the task before asking for it", rel)
	}

	// The table format is the CLI default, so an unpinned read renders the
	// version in a form the extraction expression cannot match.
	if !strings.Contains(commands, "KODAMA_OUTPUT=json kodactl call task.read") {
		t.Fatalf("%s task.read must pin JSON output with KODAMA_OUTPUT=json; the default table rendering carries no matchable lifecycle_version", rel)
	}

	script := handoffReadSedScript(t, rel, commands)
	jsonOutput := renderKodactlTaskRead(t, cli.OutputJSON)
	if got := runSed(t, script, jsonOutput); got != "7" {
		t.Fatalf("%s sed %q on the real task.read JSON %q = %q; want \"7\"", rel, script, jsonOutput, got)
	}
	tableOutput := renderKodactlTaskRead(t, cli.OutputTable)
	if !strings.Contains(tableOutput, "lifecycle_version\t7") {
		t.Fatalf("task.read table rendering %q does not carry the lifecycle_version row; the fixture is wrong", tableOutput)
	}
	if got := runSed(t, script, tableOutput); got != "" {
		t.Fatalf("%s sed %q matched the default table rendering %q (= %q); the JSON pin is load-bearing", rel, script, tableOutput, got)
	}
}

// taskReadTransport answers one canned operation result, so the real kodactl
// call path can render a task.read result without a server.
type taskReadTransport struct{ result api.Result }

// Invoke returns the canned result.
func (transport taskReadTransport) Invoke(context.Context, api.Invocation) (api.Result, error) {
	return transport.result, nil
}

// renderKodactlTaskRead renders a real task.read result through the real
// `kodactl call` path, including the real output renderer and public codec.
func renderKodactlTaskRead(t *testing.T, format cli.OutputFormat) string {
	t.Helper()
	codec := exampleOperationCodec(t, "task.read")
	body, err := codec.EncodeResult(api.TaskReadResult{
		TaskID:           "task-handoff-1",
		Title:            "Continue the durable handoff",
		State:            "open",
		ContentVersion:   3,
		LifecycleVersion: 7,
		Initiator:        "human-1",
	})
	if err != nil {
		t.Fatalf("encode task.read result: %v", err)
	}
	var out, errOut bytes.Buffer
	code := cli.Run(context.Background(),
		cli.Config{ServerURL: "http://127.0.0.1:8080", Output: format},
		[]string{"call", "task.read", `--input={"task_id":"task-handoff-1"}`},
		cli.Ports{Client: taskReadTransport{result: api.Result{Operation: "task.read", Body: body}}, Out: &out, Err: &errOut})
	if code != cli.ExitSuccess {
		t.Fatalf("render task.read as %s: exit %d: %s", format, code, errOut.String())
	}
	return out.String()
}

// handoffReadSedScript returns the sed program the handoff read uses to extract
// the lifecycle version.
func handoffReadSedScript(t *testing.T, rel, commands string) string {
	t.Helper()
	const marker = "sed -n '"
	start := strings.Index(commands, marker)
	if start < 0 {
		t.Fatalf("%s task.read must extract the version with `sed -n`", rel)
	}
	start += len(marker)
	end := strings.IndexByte(commands[start:], '\'')
	if end < 0 {
		t.Fatalf("%s task.read has an unterminated sed program", rel)
	}
	return commands[start : start+end]
}

// runSed applies one sed program to input exactly as the shell command would.
func runSed(t *testing.T, script, input string) string {
	t.Helper()
	command := exec.Command("sed", "-n", script)
	command.Stdin = strings.NewReader(input)
	var out, errOut bytes.Buffer
	command.Stdout = &out
	command.Stderr = &errOut
	if err := command.Run(); err != nil {
		t.Fatalf("run sed program %q: %v: %s", script, err, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

// kodactlInvocation matches one `kodactl <subcommand>` token pair in a shipped
// shell command, including command substitutions such as `$(kodactl ...)`.
var kodactlInvocation = regexp.MustCompile(`(?:^|[\s;&|(${])kodactl[ \t]+([A-Za-z0-9_.-]+)`)

// kodactlCallOperation matches one `kodactl call <operation>` invocation.
var kodactlCallOperation = regexp.MustCompile(`(?:^|[\s;&|(${])kodactl[ \t]+call[ \t]+([A-Za-z0-9_.-]+)`)

// TestExampleKodactlInvocationsMatchRealCLI rejects any example that teaches a
// kodactl subcommand the real CLI does not dispatch. `kodactl task ask` is the
// concrete case: the CLI exposes served operations only through `call`,
// and `call` must name a registered public operation.
func TestExampleKodactlInvocationsMatchRealCLI(t *testing.T) {
	for _, name := range sortedExampleYAML(t, "pipelines") {
		rel := "pipelines/" + name
		decoded := decodeExample(t, rel)
		for _, action := range decoded.Actions {
			if action.Container == nil {
				continue
			}
			for _, command := range action.Container.Commands {
				calls := 0
				for _, match := range kodactlInvocation.FindAllStringSubmatch(command, -1) {
					subcommand := match[1]
					if !realKodactlSubcommand(t, subcommand) {
						t.Fatalf("%s action %q invokes `kodactl %s`; the real CLI has no %q subcommand", rel, action.Name, subcommand, subcommand)
					}
					if subcommand == "call" {
						calls++
					}
				}
				operations := kodactlCallOperation.FindAllStringSubmatch(command, -1)
				if len(operations) != calls {
					t.Fatalf("%s action %q has %d `kodactl call` invocations but %d named operations", rel, action.Name, calls, len(operations))
				}
				for _, match := range operations {
					if _, ok := api.LookupRoute(match[1]); !ok {
						t.Fatalf("%s action %q calls unknown operation %q; want a served public operation", rel, action.Name, match[1])
					}
				}
			}
		}
	}
}

// realKodactlSubcommand asks the real CLI dispatcher whether it recognizes one
// subcommand. An unrecognized name is the only path that prints
// "unknown command".
func realKodactlSubcommand(t *testing.T, subcommand string) bool {
	t.Helper()
	var errOut bytes.Buffer
	cli.Run(context.Background(), cli.Config{}, []string{subcommand}, cli.Ports{Out: io.Discard, Err: &errOut})
	return !strings.Contains(errOut.String(), "unknown command")
}

// TestServiceResponseExampleUsesServiceAssignment checks the service response
// example: one explicit service assignment with declared response outputs.
func TestServiceResponseExampleUsesServiceAssignment(t *testing.T) {
	const rel = "pipelines/service-response.yaml"
	decoded := decodeExample(t, rel)
	var response *definition.Action
	for index := range decoded.Actions {
		if decoded.Actions[index].Request != nil {
			response = &decoded.Actions[index]
			break
		}
	}
	if response == nil {
		t.Fatalf("%s declares no service response request action", rel)
	}
	assertAssignment(t, rel, *response)
	if !strings.HasPrefix(response.Request.Assignment, "service/") {
		t.Fatalf("%s response assignment %q must name an explicit service", rel, response.Request.Assignment)
	}
	if len(response.Request.Outputs) == 0 {
		t.Fatalf("%s service response declares no response outputs", rel)
	}
	if response.Request.Subject.Len() == 0 {
		t.Fatalf("%s service response declares no subject", rel)
	}
}

// TestPushExampleDeclaresExactPushInputs checks the push test example: the
// parser validates repository/service/on.push internally, and the declared
// inputs are exactly the declared push-supplied set.
func TestPushExampleDeclaresExactPushInputs(t *testing.T) {
	const rel = "pipelines/push-test.yaml"
	decoded := decodeExample(t, rel)
	want := []string{"repository", "ref", "commit", "previous_commit"}
	got := inputKeys(decoded)
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedGot)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if len(sortedGot) != len(sortedWant) {
		t.Fatalf("%s push inputs = %v; want exactly %v", rel, got, want)
	}
	for index := range sortedWant {
		if sortedGot[index] != sortedWant[index] {
			t.Fatalf("%s push inputs = %v; want exactly %v", rel, got, want)
		}
	}
	for _, action := range decoded.Actions {
		if actionKind(t, rel, action) != "container" {
			t.Fatalf("%s action %q must be a container action", rel, action.Name)
		}
	}
}

// TestBuildExamplePublishesDigestOutput checks the split build example: one
// build action that declares the digest-pinned image output.
func TestBuildExamplePublishesDigestOutput(t *testing.T) {
	const rel = "pipelines/build.yaml"
	decoded := decodeExample(t, rel)
	assertKinds(t, rel, decoded, "container")
	build := decoded.Actions[0]
	if !hasOutput(build, "image") {
		t.Fatalf("%s build action must declare the image output", rel)
	}
	if len(build.Container.Commands) == 0 {
		t.Fatalf("%s build action declares no commands", rel)
	}
	if hasInput(decoded, "image") {
		t.Fatalf("%s build action must not take an image input; it publishes the candidate", rel)
	}
}

// TestTestExampleTestsForwardedDigest checks the split test example: it consumes
// the forwarded image input and does not republish it.
func TestTestExampleTestsForwardedDigest(t *testing.T) {
	const rel = "pipelines/test.yaml"
	decoded := decodeExample(t, rel)
	assertKinds(t, rel, decoded, "container")
	if !hasInput(decoded, "image") {
		t.Fatalf("%s test action must take the forwarded image input", rel)
	}
	if hasOutput(decoded.Actions[0], "image") {
		t.Fatalf("%s test action must not republish the image output", rel)
	}
	if len(decoded.Actions[0].Container.Commands) == 0 {
		t.Fatalf("%s test action declares no commands", rel)
	}
}

// TestPromoteExampleApprovesExactCandidate checks the promotion recipe:
// release -> approve exact ref/base/candidate -> integrate, with no rebuild.
func TestPromoteExampleApprovesExactCandidate(t *testing.T) {
	const rel = "pipelines/promote.yaml"
	decoded := decodeExample(t, rel)
	assertKinds(t, rel, decoded, "container", "request", "container")
	if !hasInput(decoded, "image") {
		t.Fatalf("%s promotion must take the built image digest as an input", rel)
	}
	release := decoded.Actions[0]
	for _, output := range []string{"candidate", "expected_base"} {
		if !hasOutput(release, output) {
			t.Fatalf("%s release action must declare the %q output", rel, output)
		}
	}
	approval := decoded.Actions[1]
	assertAssignment(t, rel, approval)
	if !strings.HasPrefix(approval.Request.Assignment, "role/") {
		t.Fatalf("%s promotion approval must be a human role assignment", rel)
	}
	if operation := literalField(t, rel, approval, "operation"); operation != "git.ref.update" {
		t.Fatalf("%s approval operation = %q; want git.ref.update", rel, operation)
	}
	assertResourceID(t, rel, "resource", literalField(t, rel, approval, "resource"))
	if ref := literalField(t, rel, approval, "ref"); !strings.HasPrefix(ref, "refs/") {
		t.Fatalf("%s approval ref = %q; want an exact refs/... ref", rel, ref)
	}
	outputReference(t, rel, approval, "expected_base", string(release.Name), "expected_base")
	outputReference(t, rel, approval, "candidate", string(release.Name), "candidate")

	integrate := decoded.Actions[2]
	if len(integrate.Container.Commands) == 0 {
		t.Fatalf("%s integrate action declares no commands", rel)
	}
}

// TestRollbackExamplePromotesOlderDigest checks the rollback recipe: a new
// promotion of an older digest through the same approved protected update, with
// no rebuild and no automatic data rollback.
func TestRollbackExamplePromotesOlderDigest(t *testing.T) {
	const rel = "pipelines/rollback.yaml"
	decoded := decodeExample(t, rel)
	assertKinds(t, rel, decoded, "container", "request", "container")
	if !hasInput(decoded, "image") {
		t.Fatalf("%s rollback must take the older image digest as an input", rel)
	}
	release := decoded.Actions[0]
	for _, output := range []string{"candidate", "expected_base"} {
		if !hasOutput(release, output) {
			t.Fatalf("%s rollback release action must declare the %q output", rel, output)
		}
	}
	if hasOutput(release, "image") {
		t.Fatalf("%s rollback must not rebuild the image", rel)
	}
	approval := decoded.Actions[1]
	assertAssignment(t, rel, approval)
	if operation := literalField(t, rel, approval, "operation"); operation != "git.ref.update" {
		t.Fatalf("%s rollback approval operation = %q; want git.ref.update", rel, operation)
	}
	outputReference(t, rel, approval, "expected_base", string(release.Name), "expected_base")
	outputReference(t, rel, approval, "candidate", string(release.Name), "candidate")
}

// TestPromoteAndRollbackShareDeployTarget checks that promotion and rollback
// target the same registered deploy resource and exact ref, and that the built
// digest key is forwarded unchanged across the split recipes.
func TestPromoteAndRollbackShareDeployTarget(t *testing.T) {
	promote := decodeExample(t, "pipelines/promote.yaml")
	rollback := decodeExample(t, "pipelines/rollback.yaml")

	promoteApproval := promote.Actions[1]
	rollbackApproval := rollback.Actions[1]
	promoteResource := literalField(t, "pipelines/promote.yaml", promoteApproval, "resource")
	rollbackResource := literalField(t, "pipelines/rollback.yaml", rollbackApproval, "resource")
	if promoteResource != rollbackResource {
		t.Fatalf("promote deploy resource %q != rollback deploy resource %q", promoteResource, rollbackResource)
	}
	promoteRef := literalField(t, "pipelines/promote.yaml", promoteApproval, "ref")
	rollbackRef := literalField(t, "pipelines/rollback.yaml", rollbackApproval, "ref")
	if promoteRef != rollbackRef {
		t.Fatalf("promote ref %q != rollback ref %q", promoteRef, rollbackRef)
	}

	build := decodeExample(t, "pipelines/build.yaml")
	if !hasOutput(build.Actions[0], "image") {
		t.Fatalf("build example must publish the image output")
	}
	for _, rel := range []string{"pipelines/promote.yaml", "pipelines/rollback.yaml"} {
		decoded := decodeExample(t, rel)
		if !hasInput(decoded, "image") {
			t.Fatalf("%s must consume the image digest as an input", rel)
		}
	}
}

// TestReadinessExampleObservesTargetHealth checks the readiness reconciliation
// example: a container action records an explicit readiness result instead of
// treating command exit as proof of health.
func TestReadinessExampleObservesTargetHealth(t *testing.T) {
	const rel = "pipelines/readiness.yaml"
	decoded := decodeExample(t, rel)
	found := false
	for _, action := range decoded.Actions {
		if action.Container != nil && hasOutput(action, "ready") {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s must declare a container action with the readiness output", rel)
	}
}

// hasInput reports whether the pipeline declares an input key.
func hasInput(decoded definition.Pipeline, key string) bool {
	for _, candidate := range decoded.Inputs {
		if string(candidate) == key {
			return true
		}
	}
	return false
}

// invalidPipelines are the minimal negative fixtures the real parser must
// refuse with fault.InvalidInput. Each file isolates exactly one unsupported
// construct.
var invalidPipelines = []string{
	"unknown-field.yaml",     // runner-style extension field
	"forward-reference.yaml", // subject references a later action
	"undeclared-input-reference.yaml",
	"both-kinds.yaml",           // container and request in one action
	"push-glob-ref.yaml",        // on.push ref with a wildcard
	"reserved-env.yaml",         // KODAMA_URL shadowing
	"reserved-secret.yaml",      // KODAMA_TOKEN shadowing
	"interpolated-subject.yaml", // expression instead of a direct lookup
	"duplicate-action.yaml",     // duplicate action names
	"no-actions.yaml",           // empty action list
	"yaml-alias.yaml",           // alias expansion
}

// TestInvalidExamplePipelinesAreRefused checks that the negative fixtures are
// refused by the real parser for a typed invalid-input reason. These are
// testdata, never shipped examples.
func TestInvalidExamplePipelinesAreRefused(t *testing.T) {
	for _, name := range invalidPipelines {
		rel := "testdata/invalid/" + name
		t.Run(name, func(t *testing.T) {
			_, err := decodeRaw(t, rel)
			if err == nil {
				t.Fatalf("%s must be refused by pipeline.Decode", rel)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("%s refusal class = %v (ok=%v); want %s: %v", rel, class, ok, fault.InvalidInput, err)
			}
		})
	}
}
