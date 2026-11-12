package examples_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
)

// servedInvocation is one embedded `kodactl call <operation>` request body from
// a shipped example, rendered with deterministic placeholder values so the real
// public codecs can decode it.
type servedInvocation struct {
	Operation       string
	Body            string
	Arguments       []string
	ExpectedVersion bool
	IdempotencyKey  bool
	Command         string
}

// servedCallPattern matches one `kodactl call <operation>` invocation.
var servedCallPattern = regexp.MustCompile(`kodactl[ \t]+call[ \t]+([A-Za-z0-9_.-]+)`)

// servedInputMarker introduces the JSON body of one call. Examples build it
// with printf so a durable value can be substituted without a shell JSON
// encoder.
const servedInputMarker = `--input="$(printf '`

// TestTaskHandoffExampleUsesServedCodecs pins that every request body embedded
// in the task handoff example decodes through the real public codecs. The
// example teaches an operator how to call the served API, so a body that only
// looks like a task request (for example `task`/`assignee` member names) is
// a broken recipe: the strict codec would refuse it before any effect.
func TestTaskHandoffExampleUsesServedCodecs(t *testing.T) {
	const rel = "pipelines/task-handoff.yaml"
	invocations := exampleServedInvocations(t, rel)
	if len(invocations) == 0 {
		t.Fatalf("%s embeds no `kodactl call` request body", rel)
	}
	seen := make(map[string]bool, len(invocations))
	for _, invocation := range invocations {
		t.Run(invocation.Operation, func(t *testing.T) {
			seen[invocation.Operation] = true
			codec := exampleOperationCodec(t, invocation.Operation)
			body := json.RawMessage(invocation.Body)
			if !json.Valid(body) {
				t.Fatalf("%s %s request body is not valid JSON: %s", rel, invocation.Operation, invocation.Body)
			}
			// A kodactl call has no route path, so the input must carry every
			// route-derived field itself: this is the MCP carriage rule.
			if _, err := api.DecodeMCPOperationRequest(invocation.Operation, body); err != nil {
				t.Fatalf("%s %s request body %s does not decode through the served MCP codec: %v", rel, invocation.Operation, invocation.Body, err)
			}
			// The REST edge decodes the same typed body strictly and fills the
			// route-derived identity. Both edges must accept the published body.
			params := make(map[string]string, len(codec.RouteParams))
			for _, param := range codec.RouteParams {
				params[param.Name] = exampleRouteValue(param.Field)
			}
			if _, err := api.DecodeRESTRequest(invocation.Operation, body, params); err != nil {
				t.Fatalf("%s %s request body %s does not decode through the served REST codec: %v", rel, invocation.Operation, invocation.Body, err)
			}
			// The shared envelope is strict too: a version must be positive and
			// a receipt-bearing operation must carry a usable caller key.
			envelope := map[string]any{"input": body}
			if invocation.ExpectedVersion {
				envelope["expected_version"] = uint64(7)
			}
			if invocation.IdempotencyKey {
				envelope["idempotency_key"] = "example-call-key"
			}
			encoded, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("%s %s envelope is not encodable: %v", rel, invocation.Operation, err)
			}
			if _, err := api.DecodeOperationRequest(invocation.Operation, encoded); err != nil {
				t.Fatalf("%s %s request envelope does not decode through the served envelope codec: %v", rel, invocation.Operation, err)
			}
			if codec.RequiresIdempotencyKey && !invocation.IdempotencyKey {
				t.Fatalf("%s %s is receipt-bearing but the example omits --idempotency-key", rel, invocation.Operation)
			}
		})
	}
	for _, operation := range []string{"task.read", "task.ask"} {
		if !seen[operation] {
			t.Fatalf("%s must embed a served %s request body", rel, operation)
		}
	}
}

// TestExampleHandoffRetainsDurableReferencesAndVersion pins the handoff
// invariants alongside the served request field names: the example persists
// durable context first, reads the task lifecycle version from task.read (the
// task.json snapshot has none), asks with that exact expected version and
// carries only durable commit/digest references.
func TestExampleHandoffRetainsDurableReferencesAndVersion(t *testing.T) {
	const rel = "pipelines/task-handoff.yaml"
	decoded := decodeExample(t, rel)
	if len(decoded.Actions) < 2 {
		t.Fatalf("%s declares %d actions; want a persist action and an ask action", rel, len(decoded.Actions))
	}
	persist := decoded.Actions[0]
	if persist.Container == nil || len(persist.Container.Outputs) == 0 {
		t.Fatalf("%s action %q must persist durable context and declare its reference output", rel, persist.Name)
	}
	if !hasOutput(persist, "context_commit") {
		t.Fatalf("%s action %q must declare the durable context_commit output", rel, persist.Name)
	}

	invocations := exampleServedInvocations(t, rel)
	var askInvocation *servedInvocation
	for index := range invocations {
		if invocations[index].Operation == "task.ask" {
			askInvocation = &invocations[index]
			break
		}
	}
	if askInvocation == nil {
		t.Fatalf("%s embeds no task.ask request body", rel)
	}
	value, err := api.DecodeMCPOperationRequest("task.ask", json.RawMessage(askInvocation.Body))
	if err != nil {
		t.Fatalf("%s task.ask body %s does not decode through the served codec: %v", rel, askInvocation.Body, err)
	}
	request, ok := value.(api.TaskAskRequest)
	if !ok {
		t.Fatalf("%s task.ask codec returned %T; want api.TaskAskRequest", rel, value)
	}
	if request.TaskID == "" {
		t.Fatalf("%s task.ask body must name the current task as task_id", rel)
	}
	if !strings.HasPrefix(request.Assignment, "role/") && !strings.HasPrefix(request.Assignment, "service/") {
		t.Fatalf("%s task.ask assignment %q must use the role/<role> or service/<id> grammar", rel, request.Assignment)
	}
	if len(request.References) != 2 {
		t.Fatalf("%s task.ask references = %v; want the durable commit and digest references", rel, request.References)
	}
	if !strings.HasPrefix(request.References[0], "commit=") || !strings.HasPrefix(request.References[1], "digest=") {
		t.Fatalf("%s task.ask references = %v; want commit= then digest=", rel, request.References)
	}
	// The printf argument order is the only binding between a durable value and
	// its reference: commit takes the pushed context commit and digest takes the
	// admitted digest input. A swapped argument pair must fail here.
	if len(askInvocation.Arguments) != 3 {
		t.Fatalf("%s task.ask printf has %d arguments; want task id, context commit and digest", rel, len(askInvocation.Arguments))
	}
	wantArguments := []string{
		"$KODAMA_TASK",
		"$(cat /kodama/outputs/persist/context_commit)",
		"$(cat /kodama/inputs/digest)",
	}
	for index, want := range wantArguments {
		if askInvocation.Arguments[index] != want {
			t.Fatalf("%s task.ask printf argument %d = %q; want %q (commit <- context_commit output, digest <- digest input)", rel, index, askInvocation.Arguments[index], want)
		}
	}

	ask := decoded.Actions[len(decoded.Actions)-1]
	commands := strings.Join(ask.Container.Commands, "\n")
	var allCommands []string
	for _, action := range decoded.Actions {
		if action.Container != nil {
			allCommands = append(allCommands, action.Container.Commands...)
		}
	}
	pipelineCommands := strings.Join(allCommands, "\n")
	for _, input := range []string{"commit", "digest"} {
		if !strings.Contains(pipelineCommands, "/kodama/inputs/"+input) {
			t.Fatalf("%s must pass the admitted durable input /kodama/inputs/%s", rel, input)
		}
	}
	if !strings.Contains(commands, "/kodama/inputs/digest") {
		t.Fatalf("%s task.ask must pass the admitted durable input /kodama/inputs/digest", rel)
	}
	if !strings.Contains(commands, "/kodama/outputs/persist/context_commit") {
		t.Fatalf("%s task.ask must pass the durable output /kodama/outputs/persist/context_commit", rel)
	}
	readIndex := strings.Index(commands, "kodactl call task.read")
	askIndex := strings.Index(commands, "kodactl call task.ask")
	if readIndex < 0 || askIndex < 0 || readIndex > askIndex {
		t.Fatalf("%s must read the task before asking for it", rel)
	}
	if !strings.Contains(commands, `"lifecycle_version"`) {
		t.Fatalf("%s must read the task lifecycle_version before asking", rel)
	}
	if !strings.Contains(commands, `--expected-version="$task_version"`) {
		t.Fatalf("%s task.ask must pass the version read from task.read as --expected-version", rel)
	}
	if strings.Contains(commands, "/kodama/task.json") {
		t.Fatalf("%s must not read the lifecycle version from the versionless /kodama/task.json snapshot", rel)
	}
	if !strings.Contains(commands, `--idempotency-key="ask:${KODAMA_TASK}:$(cat /kodama/outputs/persist/context_commit)"`) {
		t.Fatalf("%s task.ask must derive a stable --idempotency-key from $KODAMA_TASK and the durable context commit", rel)
	}
	for _, disposable := range []string{"/workspace/", "file://", "/tmp/", "/var/tmp/"} {
		if strings.Contains(commands, disposable) {
			t.Fatalf("%s task.ask references disposable path %q; handoffs carry durable references", rel, disposable)
		}
	}
}

// exampleServedInvocations extracts every embedded `kodactl call` request body
// from one example pipeline and renders its printf placeholders with
// deterministic values. A call without exactly one input is a fixture defect:
// the public codecs cannot check a body that is not published.
func exampleServedInvocations(t *testing.T, rel string) []servedInvocation {
	t.Helper()
	decoded := decodeExample(t, rel)
	var invocations []servedInvocation
	for _, action := range decoded.Actions {
		if action.Container == nil {
			continue
		}
		for _, command := range action.Container.Commands {
			invocations = append(invocations, parseServedInvocations(t, rel, string(action.Name), command)...)
		}
	}
	return invocations
}

// parseServedInvocations parses the `kodactl call` bodies of one shell command.
func parseServedInvocations(t *testing.T, rel, action, command string) []servedInvocation {
	t.Helper()
	matches := servedCallPattern.FindAllStringSubmatchIndex(command, -1)
	invocations := make([]servedInvocation, 0, len(matches))
	for index, match := range matches {
		operation := command[match[2]:match[3]]
		segmentStart := match[1]
		segmentEnd := len(command)
		if index+1 < len(matches) {
			segmentEnd = matches[index+1][0]
		}
		segment := command[segmentStart:segmentEnd]
		marker := strings.Index(segment, servedInputMarker)
		if marker < 0 {
			t.Fatalf("%s action %q %s call has no --input=<json> body", rel, action, operation)
		}
		formatStart := segmentStart + marker + len(servedInputMarker)
		formatEnd := strings.IndexByte(command[formatStart:], '\'')
		if formatEnd < 0 {
			t.Fatalf("%s action %q %s input has an unterminated printf format", rel, action, operation)
		}
		formatEnd += formatStart
		format := command[formatStart:formatEnd]
		openParen := strings.Index(command[segmentStart+marker:], "$(")
		if openParen < 0 {
			t.Fatalf("%s action %q %s input is not a printf command substitution", rel, action, operation)
		}
		closeParen := matchingParen(command, segmentStart+marker+openParen+1, formatStart, formatEnd)
		if closeParen < 0 {
			t.Fatalf("%s action %q %s input has an unterminated printf command substitution", rel, action, operation)
		}
		arguments := shellWords(command[formatEnd+1 : closeParen])
		body := renderServedInput(t, rel, action, operation, format, arguments)
		invocations = append(invocations, servedInvocation{
			Operation:       operation,
			Body:            body,
			Arguments:       arguments,
			ExpectedVersion: strings.Contains(segment, "--expected-version="),
			IdempotencyKey:  strings.Contains(segment, "--idempotency-key="),
			Command:         command,
		})
	}
	return invocations
}

// matchingParen returns the index of the parenthesis that closes open, skipping
// the single-quoted printf format so parentheses inside the JSON body do not
// disturb the depth count.
func matchingParen(command string, open, skipStart, skipEnd int) int {
	depth := 0
	for index := open; index < len(command); index++ {
		if index >= skipStart && index <= skipEnd {
			index = skipEnd
			continue
		}
		switch command[index] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

// shellWords splits the argument text of one printf invocation. Double-quoted
// words keep their nested `$(cat ...)` command substitutions intact; line
// continuations and whitespace separate words.
func shellWords(text string) []string {
	var words []string
	index := 0
	for index < len(text) {
		for index < len(text) {
			if text[index] == '\\' && index+1 < len(text) && (text[index+1] == '\n' || text[index+1] == '\r') {
				index += 2
				continue
			}
			if strings.ContainsRune(" \t\r\n", rune(text[index])) {
				index++
				continue
			}
			break
		}
		if index >= len(text) {
			break
		}
		if text[index] == '"' {
			index++
			depth := 0
			var builder strings.Builder
			for index < len(text) {
				character := text[index]
				if character == '\\' && index+1 < len(text) {
					builder.WriteByte(text[index+1])
					index += 2
					continue
				}
				if character == '$' && index+1 < len(text) && text[index+1] == '(' {
					depth++
					builder.WriteString("$(")
					index += 2
					continue
				}
				if character == ')' && depth > 0 {
					depth--
					builder.WriteByte(')')
					index++
					continue
				}
				if character == '"' && depth == 0 {
					index++
					break
				}
				builder.WriteByte(character)
				index++
			}
			words = append(words, builder.String())
			continue
		}
		start := index
		for index < len(text) && !strings.ContainsRune(" \t\r\n", rune(text[index])) {
			index++
		}
		words = append(words, text[start:index])
	}
	return words
}

// renderServedInput substitutes one deterministic value per printf %s so the
// published body can be handed to the real codecs. A placeholder/argument count
// mismatch is a fixture defect: the body cannot be rendered as published.
func renderServedInput(t *testing.T, rel, action, operation, format string, arguments []string) string {
	t.Helper()
	placeholders := strings.Count(format, "%s")
	if placeholders != len(arguments) {
		t.Fatalf("%s action %q %s input has %d printf placeholders but %d arguments", rel, action, operation, placeholders, len(arguments))
	}
	body := format
	for index, argument := range arguments {
		body = strings.Replace(body, "%s", exampleArgumentValue(argument, index), 1)
	}
	return body
}

// exampleArgumentValue renders one deterministic, contract-valid value for a
// printf argument. The exact durable value is checked by the invariant test;
// the codecs only need a valid non-empty value to exercise the wire shape.
func exampleArgumentValue(argument string, index int) string {
	switch {
	case argument == "$KODAMA_TASK":
		return "task-handoff-1"
	case strings.Contains(argument, "context_commit"):
		return "0123456789abcdef0123456789abcdef01234567"
	case strings.Contains(argument, "digest"):
		return "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	default:
		return fmt.Sprintf("example-value-%d", index)
	}
}

// exampleRouteValue renders the route-derived value for one canonical request
// field. The example bodies carry the same value, so REST binding agrees.
func exampleRouteValue(field string) string {
	switch field {
	case "task_id":
		return "task-handoff-1"
	default:
		return "example-route-value"
	}
}

// exampleOperationCodec returns the served codec of one operation.
func exampleOperationCodec(t *testing.T, operation string) api.OperationCodec {
	t.Helper()
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec
		}
	}
	t.Fatalf("operation %q has no served codec", operation)
	return api.OperationCodec{}
}
