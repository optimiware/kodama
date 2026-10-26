package pipeline_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func readFixtureV2(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesV2, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var v2Goldens = []string{"service-test", "integration-build", "argv-matrix", "argv-matrix-explicit-empty", "edges-and-endpoints", "source-free-cleanup", "order-gates", "agent-gate"}

// Test_v1alpha2_format_disabled_by_default: registration/validation of a
// kodama/v1alpha2 document is an explicit opt-in. The zero-value Config keeps
// the installation on kodama/v1alpha1 only, the refusal runs no lookup and names
// no document content, while reading the same bytes (stored definitions) works
// regardless of the flag. PX-21.
func Test_v1alpha2_format_disabled_by_default(t *testing.T) {
	e := newEnv(t) // zero-value EnableV1Alpha2
	if e.v.FormatEnabled(pipelinev1alpha2.APIVersion) || !e.v.FormatEnabled("kodama/v1alpha1") || e.v.FormatEnabled("kodama/v9") {
		t.Fatal("default validator must enable v1alpha1 only")
	}
	src := readFixtureV2(t, "valid/service-test.yaml")
	_, err := e.v.ValidateYAML(t.Context(), scope, actor, src)
	expectCode(t, err, pipeline.CodeFormatDisabled)
	if stageOf(err) != pipeline.StageSemantic || len(codes(err)) != 1 {
		t.Fatalf("format_disabled must be the single semantic finding, got %v", err)
	}
	if e.refs.calls != 0 || e.profiles.calls != 0 {
		t.Fatal("a disabled format must not reach any lookup")
	}
	for _, d := range err.(*pipeline.ValidationError).Diagnostics {
		if strings.Contains(d.Message, "service-test") || strings.Contains(d.Message, "registry.example") {
			t.Fatalf("message echoes document content: %q", d.Message)
		}
	}

	t.Run("reading is independent of the registration flag", func(t *testing.T) {
		on := newEnvV2(t).mustValidate(t, string(src))
		doc, err := pipeline.ParseJSON(on.Canonical(), pipeline.Options{MaxBytes: 1 << 20})
		if err != nil || !doc.IsV1Alpha2() {
			t.Fatalf("raw decoding of a stored v1alpha2 document failed: %v", err)
		}
		stored, err := pipeline.NewValidatedFromCanonical(on.Canonical(), on.Digest())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stored.Document(); err != nil {
			t.Fatal(err)
		}
		// The disabled validator still parses (syntax) the document.
		if _, err := e.v.ParseYAML(src); err != nil {
			t.Fatalf("disabled validator must still parse: %v", err)
		}
	})

	t.Run("a syntactically invalid document is not hidden behind the flag", func(t *testing.T) {
		_, err := e.validateJSON(mutate(t, "spec.services.db.hostPort", 80))
		expectCode(t, err, pipeline.CodeUnknownField)
	})

	t.Run("zero-value validator wiring accepts v1alpha1 unchanged", func(t *testing.T) {
		e.mustValidate(t, doc(container("a")))
	})
}

// Test_v1alpha2_canonical_goldens: every valid fixture validates, matches its
// golden canonical bytes and digest, round-trips through the stored-content
// reader, and the contract's published reference vector is reproduced
// independently (digest computed by the contract authors with Python).
// PX-17, PX-20, PX-21.
func Test_v1alpha2_canonical_goldens(t *testing.T) {
	e := newEnvV2(t)
	for _, name := range v2Goldens {
		t.Run(name, func(t *testing.T) {
			v := e.mustValidate(t, string(readFixtureV2(t, "valid/"+name+".yaml")))
			canonPath := filepath.Join(fixturesV2, "canonical", name+".json")
			digestPath := filepath.Join(fixturesV2, "canonical", name+".digest")
			if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(canonPath, v.Canonical(), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(digestPath, []byte(v.Digest()+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want := readFixtureV2(t, "canonical/"+name+".json")
			if !bytes.Equal(v.Canonical(), want) {
				t.Fatalf("canonical bytes changed\n got: %s\nwant: %s", v.Canonical(), want)
			}
			wantDigest := strings.TrimSpace(string(readFixtureV2(t, "canonical/"+name+".digest")))
			if string(v.Digest()) != wantDigest || pipeline.DigestOf(want) != v.Digest() {
				t.Fatalf("digest %s, want %s", v.Digest(), wantDigest)
			}
			stored, err := pipeline.NewValidatedFromCanonical(want, pipeline.Digest(wantDigest))
			if err != nil {
				t.Fatalf("stored content rejected: %v", err)
			}
			if !bytes.Equal(stored.Canonical(), want) {
				t.Fatal("stored round trip changed the bytes")
			}
			if !strings.HasPrefix(string(want), `{"apiVersion":"kodama/v1alpha2","kind":"Pipeline"`) || strings.ContainsAny(string(want), "\n\t") {
				t.Fatalf("canonical form is not sorted and compact: %s", want)
			}
		})
	}

	t.Run("contract reference vector", func(t *testing.T) {
		v := e.mustValidate(t, string(readFixtureV2(t, "valid/service-test.yaml")))
		const contractDigest = "sha256:368c9636851797bb0812589cae5297122b3d799fc96522194764de5a574910e2"
		if string(v.Digest()) != contractDigest {
			t.Fatalf("digest %s differs from the digest published in pipeline-format-v1alpha2.md", v.Digest())
		}
		jsonV := e.mustValidateJSON(t, readFixtureV2(t, "valid/service-test.json"))
		if jsonV.Digest() != v.Digest() {
			t.Fatal("YAML and JSON spellings must converge")
		}
	})

	t.Run("a v1alpha1 digest never collides with the same content in v1alpha2", func(t *testing.T) {
		a := e.mustValidate(t, doc(container("a")))
		b := e.mustValidate(t, strings.Replace(strings.Replace(doc(container("a")), "v1alpha1", "v1alpha2", 1), "spec:\n", "spec:\n  timeoutSeconds: 600\n", 1))
		if a.Digest() == b.Digest() {
			t.Fatal("the digest includes apiVersion")
		}
	})
}

// Test_v1alpha2_digest_independent_of_run_source_and_policy: the canonical form
// fills only contract-constant defaults. Step workspace, spec.runWorkspace,
// service command, environment and when stay absent, so one definition has one
// digest for source-backed and source-free runs. PX-21, WS-13.
func Test_v1alpha2_digest_independent_of_run_source_and_policy(t *testing.T) {
	e := newEnvV2(t)
	base := e.mustValidateJSON(t, mutate(t))
	c := string(base.Canonical())
	for _, absent := range []string{`"workspace"`, `"runWorkspace"`, `"environment"`, `"when"`} {
		if strings.Contains(c, absent) {
			t.Errorf("%s must not be filled into the canonical form: %s", absent, c)
		}
	}
	if strings.Contains(c, `"command":null`) || strings.Count(c, `"command"`) != 1 {
		t.Errorf("an absent service command must stay absent (only the step command appears): %s", c)
	}

	withRead := e.mustValidateJSON(t, mutate(t, "spec.steps.0.workspace", map[string]any{"access": "read"}))
	withNone := e.mustValidateJSON(t, mutate(t, "spec.steps.0.workspace", map[string]any{"access": "none"}))
	withWrite := e.mustValidateJSON(t, mutate(t, "spec.steps.0.workspace", map[string]any{"access": "write"}))
	digests := map[pipeline.Digest]string{base.Digest(): "absent"}
	for name, v := range map[string]pipeline.Validated{"read": withRead, "none": withNone, "write": withWrite} {
		if prev, dup := digests[v.Digest()]; dup {
			t.Errorf("explicit %s and %s must have different digests", name, prev)
		}
		digests[v.Digest()] = name
	}
	withSize := e.mustValidateJSON(t, mutate(t, "spec.runWorkspace", map[string]any{"sizeMiB": 2048}))
	if withSize.Digest() == base.Digest() || !strings.Contains(string(withSize.Canonical()), `"runWorkspace":{"sizeMiB":2048}`) {
		t.Fatal("an explicit scratch size is part of the definition; an absent one is not")
	}

	t.Run("contract-constant defaults equal their explicit spelling", func(t *testing.T) {
		explicit := e.mustValidateJSON(t, mutate(t,
			"spec.inputs", map[string]any{"region": map[string]any{"type": "string", "default": "eu", "required": false}},
			"spec.services.db.role", "general",
			"spec.services.db.args", []any{}, "spec.services.db.env", map[string]any{}, "spec.services.db.secretFiles", map[string]any{},
			"spec.services.db.storage", []any{}, "spec.services.db.egress", []any{},
			"spec.services.db.ports.sql.protocol", "TCP",
			"spec.services.db.resources.shmMiB", 64,
			"spec.services.db.readiness.periodSeconds", 2, "spec.services.db.readiness.probeTimeoutSeconds", 5, "spec.services.db.readiness.lossSeconds", 30,
			"spec.steps.0.needs", []any{}, "spec.steps.0.env.EXTRA", nil, "spec.steps.0.egress", []any{}, "spec.steps.0.outputs", map[string]any{},
			"spec.steps.0.secretFiles", map[string]any{},
			"spec.steps.0.retry", map[string]any{"maxAttempts": 1, "backoffSeconds": 0, "on": []any{}}))
		if explicit.Digest() != base.Digest() {
			t.Fatalf("explicit contract defaults must equal omitted defaults:\n%s\n%s", explicit.Canonical(), base.Canonical())
		}
		noNetwork := e.mustValidateJSON(t, mutate(t, "spec.network", map[string]any{}, "spec.network.edges", []any{}, "spec.services", nil, "spec.steps.0.env", nil))
		again := e.mustValidateJSON(t, mutate(t, "spec.network", nil, "spec.services", nil, "spec.steps.0.env", nil))
		if noNetwork.Digest() != again.Digest() {
			t.Fatal("absent network equals network with no edges")
		}
		zeroShm := e.mustValidateJSON(t, mutate(t, "spec.services.db.resources.shmMiB", 0))
		if zeroShm.Digest() == base.Digest() || !strings.Contains(string(zeroShm.Canonical()), `"shmMiB":0`) {
			t.Fatal("an explicit shmMiB 0 is distinct from the default 64")
		}
	})

	t.Run("input defaults do not depend on admission input values", func(t *testing.T) {
		a := e.mustValidateJSON(t, mutate(t, "spec.inputs.region.default", "eu"))
		b := e.mustValidateJSON(t, mutate(t, "spec.inputs.region.default", "us"))
		if a.Digest() == b.Digest() || a.Digest() != base.Digest() {
			t.Fatal("the default is definition content and nothing else is injected")
		}
	})
}

// Test_v1alpha2_argv_omission_equivalence: absent args and args: [] are one
// value with one digest; absent command is preserved and differs from any
// command; command: [] is rejected; the four argv rows each have a valid
// fixture and the matrix resolves them as the contract states. PX-17, PX-21.
func Test_v1alpha2_argv_omission_equivalence(t *testing.T) {
	e := newEnvV2(t)
	omitted := e.mustValidate(t, string(readFixtureV2(t, "valid/argv-matrix.yaml")))
	explicit := e.mustValidate(t, string(readFixtureV2(t, "valid/argv-matrix-explicit-empty.yaml")))
	if omitted.Digest() != explicit.Digest() || !bytes.Equal(omitted.Canonical(), explicit.Canonical()) {
		t.Fatalf("args omitted and args: [] must canonicalize identically:\n%s\n%s", omitted.Canonical(), explicit.Canonical())
	}
	doc, err := omitted.Document()
	if err != nil {
		t.Fatal(err)
	}
	for name, wantCommand := range map[string]bool{"defaults": false, "argsonly": false, "commandonly": true, "both": true} {
		svc := doc.Spec.Services[name]
		if (svc.Command != nil) != wantCommand {
			t.Errorf("%s: command presence = %v, want %v", name, svc.Command != nil, wantCommand)
		}
		if svc.Args == nil {
			t.Errorf("%s: canonical form fills absent args as []", name)
		}
	}
	if !strings.Contains(string(omitted.Canonical()), `"args":[],"egress":[],"env":{},"image":"registry.example.invalid/svc-defaults@`) {
		t.Fatalf("defaults service must carry args: [] and no command: %s", omitted.Canonical())
	}

	t.Run("command is never filled and its absence is meaningful", func(t *testing.T) {
		withCmd := e.mustValidateJSON(t, mutate(t, "spec.services.cache.command", []any{"/bin/cache"}))
		if withCmd.Digest() == e.mustValidateJSON(t, mutate(t)).Digest() {
			t.Fatal("declaring a command changes the definition")
		}
	})

	t.Run("command: [] is rejected", func(t *testing.T) {
		_, err := e.validateJSON(mutate(t, "spec.services.db.command", []any{}))
		expectCode(t, err, pipeline.CodeInvalidValue)
		if !hasDiag(err, pipeline.CodeInvalidValue, "services.db.command") {
			t.Fatalf("wrong path: %v", err)
		}
		_, err = e.validateJSON(mutate(t, "spec.steps.0.command", []any{}))
		if err == nil {
			t.Fatal("a step command is required and non-empty")
		}
	})

	t.Run("the four-way matrix", func(t *testing.T) {
		ep, cmd := []string{"/entry"}, []string{"default-cmd"}
		for name, tc := range map[string]struct {
			command, args []string
			want          []string
		}{
			"absent/absent":      {nil, nil, []string{"/entry", "default-cmd"}},
			"absent/empty":       {nil, []string{}, []string{"/entry", "default-cmd"}},
			"absent/non-empty":   {nil, []string{"--a"}, []string{"/entry", "--a"}},
			"present/absent":     {[]string{"/c"}, nil, []string{"/c"}},
			"present/empty":      {[]string{"/c"}, []string{}, []string{"/c"}},
			"present/non-empty":  {[]string{"/c"}, []string{"--a"}, []string{"/c", "--a"}},
			"no image defaults":  {nil, nil, []string{"/entry", "default-cmd"}},
			"entrypoint is kept": {nil, []string{"x", "y"}, []string{"/entry", "x", "y"}},
		} {
			got := pipeline.EffectiveArgv(ep, cmd, tc.command, tc.args)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("%s: got %v want %v", name, got, tc.want)
			}
		}
		if got := pipeline.EffectiveArgv(nil, nil, nil, nil); got == nil || len(got) != 0 {
			t.Errorf("an image with no defaults and no command resolves to an empty argv, not nil: %#v", got)
		}
		// The caller's slices are never aliased by the result.
		command := []string{"/c"}
		got := pipeline.EffectiveArgv(nil, nil, command, nil)
		got[0] = "mutated"
		if command[0] != "/c" {
			t.Fatal("EffectiveArgv must copy")
		}
	})
}

type invalidCase struct {
	name  string
	edits []any
	code  string
	path  string // substring of the diagnostic path
	stage pipeline.Stage
}

func sx(name, code, path string, edits ...any) invalidCase {
	return invalidCase{name: name, edits: edits, code: code, path: path, stage: pipeline.StageSyntax}
}

func sm(name, code, path string, edits ...any) invalidCase {
	return invalidCase{name: name, edits: edits, code: code, path: path, stage: pipeline.StageSemantic}
}

func many(prefix string, n int, v any) []any {
	out := make([]any, n)
	for i := range out {
		if s, ok := v.(func(int) any); ok {
			out[i] = s(i)
		} else {
			out[i] = v
		}
	}
	return out
}

// Test_v1alpha2_invalid_documents: strict rejection of unknown and raw Pod
// controls, workspace.from, mutable images, bad ports, probes, resources,
// storage, edges, egress names, roles, deadlines and argv, each with its stable
// code, a path and the contract's stage. PX-17, PX-20, PX-21, WS-13,
// IT-14.
func Test_v1alpha2_invalid_documents(t *testing.T) {
	e := newEnvV2(t)
	cases := []invalidCase{
		// Unknown and raw controls.
		sx("workspace.from is removed", pipeline.CodeWorkspaceFromRemoved, "steps[0].workspace.from", "spec.steps.0.workspace", map[string]any{"access": "read", "from": "run"}),
		sx("workspace.from naming a step", pipeline.CodeWorkspaceFromRemoved, "steps[0].workspace.from", "spec.steps.0.workspace", map[string]any{"access": "write", "from": "test"}),
		sx("workspace none with from", pipeline.CodeWorkspaceFromRemoved, "workspace.from", "spec.steps.0.workspace", map[string]any{"access": "none", "from": "run"}),
		sx("workspace without access", pipeline.CodeMissingField, "workspace.access", "spec.steps.0.workspace", map[string]any{}),
		sx("workspace bad access", pipeline.CodeInvalidValue, "workspace.access", "spec.steps.0.workspace", map[string]any{"access": "admin"}),
		sx("host port", pipeline.CodeUnknownField, "services.db.hostPort", "spec.services.db.hostPort", 80),
		sx("security context", pipeline.CodeUnknownField, "services.db.securityContext", "spec.services.db.securityContext", map[string]any{"privileged": true}),
		sx("volumes", pipeline.CodeUnknownField, "services.db.volumes", "spec.services.db.volumes", []any{}),
		sx("node selector on step", pipeline.CodeUnknownField, "steps[0].nodeSelector", "spec.steps.0.nodeSelector", map[string]any{}),
		sx("kubeconfig on spec", pipeline.CodeUnknownField, "spec.kubeconfig", "spec.kubeconfig", "x"),
		sx("service network field", pipeline.CodeUnknownField, "services.db.network", "spec.services.db.network", map[string]any{}),
		sx("edge extra field", pipeline.CodeUnknownField, "edges[0].protocol", "spec.network.edges.0.protocol", "TCP"),
		sx("probe headers", pipeline.CodeUnknownField, "readiness.headers", "spec.services.db.readiness.headers", map[string]any{}),
		sx("port extra field", pipeline.CodeUnknownField, "ports.sql.nodePort", "spec.services.db.ports.sql.nodePort", 30000),

		// Images.
		sx("service tag image", pipeline.CodeMutableImage, "services.db.image", "spec.services.db.image", "postgres:16"),
		sx("service latest", pipeline.CodeMutableImage, "services.db.image", "spec.services.db.image", "registry.example.invalid/db:latest"),
		sx("service short digest", pipeline.CodeMutableImage, "services.db.image", "spec.services.db.image", "registry.example.invalid/db@sha256:abc"),
		sx("service missing image", pipeline.CodeMissingField, "services.db.image", "spec.services.db.image", nil),
		sx("step tag image", pipeline.CodeMutableImage, "steps[0].image", "spec.steps.0.image", "golang:1.24"),

		// Required service members.
		sx("service without ports", pipeline.CodeMissingField, "services.db.ports", "spec.services.db.ports", nil),
		sx("service without readiness", pipeline.CodeMissingField, "services.db.readiness", "spec.services.db.readiness", nil),
		sx("service without resources", pipeline.CodeMissingField, "services.db.resources", "spec.services.db.resources", nil),
		sx("empty ports", pipeline.CodeInvalidValue, "services.db.ports", "spec.services.db.ports", map[string]any{}),
		sx("port without number", pipeline.CodeMissingField, "ports.sql.port", "spec.services.db.ports.sql", map[string]any{}),

		// Ports.
		sx("port zero", pipeline.CodeInvalidValue, "ports.sql.port", "spec.services.db.ports.sql.port", 0),
		sx("port too large", pipeline.CodeInvalidValue, "ports.sql.port", "spec.services.db.ports.sql.port", 65536),
		sx("duplicate port number", pipeline.CodeDuplicatePort, "ports.sql.port", "spec.services.db.ports.admin.port", 5432),
		sx("protocol UDP", pipeline.CodeInvalidValue, "ports.sql.protocol", "spec.services.db.ports.sql.protocol", "UDP"),
		sx("lowercase protocol", pipeline.CodeInvalidValue, "ports.sql.protocol", "spec.services.db.ports.sql.protocol", "tcp"),
		sx("port name uppercase", pipeline.CodeInvalidFormat, "services.db.ports", "spec.services.db.ports", map[string]any{"SQL": map[string]any{"port": 5432}}),
		sx("too many ports", pipeline.CodeInvalidValue, "services.db.ports", "spec.services.db.ports", func() any {
			m := map[string]any{}
			for i := 0; i < 9; i++ {
				m[fmt.Sprintf("p%d", i)] = map[string]any{"port": 1000 + i}
			}
			return m
		}()),

		// Probes.
		sx("probe undeclared port", pipeline.CodeUnknownPort, "readiness.tcp", "spec.services.db.readiness.tcp", "nope"),
		sx("http probe undeclared port", pipeline.CodeUnknownPort, "readiness.http.port", "spec.services.db.readiness", map[string]any{"http": map[string]any{"port": "nope", "path": "/"}, "timeoutSeconds": 30}),
		sx("http path without slash", pipeline.CodeInvalidProbe, "readiness.http.path", "spec.services.db.readiness", map[string]any{"http": map[string]any{"port": "sql", "path": "health"}, "timeoutSeconds": 30}),
		sx("http path with space", pipeline.CodeInvalidProbe, "readiness.http.path", "spec.services.db.readiness", map[string]any{"http": map[string]any{"port": "sql", "path": "/a b"}, "timeoutSeconds": 30}),
		sx("http path with fragment", pipeline.CodeInvalidProbe, "readiness.http.path", "spec.services.db.readiness", map[string]any{"http": map[string]any{"port": "sql", "path": "/a#frag"}, "timeoutSeconds": 30}),
		sx("http path with control", pipeline.CodeInvalidProbe, "readiness.http.path", "spec.services.db.readiness", map[string]any{"http": map[string]any{"port": "sql", "path": "/a\u0001b"}, "timeoutSeconds": 30}),
		sx("http path unicode space", pipeline.CodeInvalidProbe, "readiness.http.path", "spec.services.db.readiness", map[string]any{"http": map[string]any{"port": "sql", "path": "/a\u00a0b"}, "timeoutSeconds": 30}),
		sx("http path too long", pipeline.CodeInvalidProbe, "readiness.http.path", "spec.services.db.readiness", map[string]any{"http": map[string]any{"port": "sql", "path": "/" + strings.Repeat("a", 512)}, "timeoutSeconds": 30}),
		sx("empty exec probe", pipeline.CodeInvalidProbe, "readiness.exec", "spec.services.db.readiness", map[string]any{"exec": []any{}, "timeoutSeconds": 30}),
		sx("long exec probe", pipeline.CodeInvalidProbe, "readiness.exec", "spec.services.db.readiness", map[string]any{"exec": many("x", 17, "x"), "timeoutSeconds": 30}),
		sx("exec element too long", pipeline.CodeInvalidProbe, "readiness.exec[0]", "spec.services.db.readiness", map[string]any{"exec": []any{strings.Repeat("x", 1025)}, "timeoutSeconds": 30}),
		sx("two probe kinds", pipeline.CodeInvalidProbe, "services.db.readiness", "spec.services.db.readiness", map[string]any{"tcp": "sql", "exec": []any{"true"}, "timeoutSeconds": 30}),
		sx("no probe kind", pipeline.CodeInvalidProbe, "services.db.readiness", "spec.services.db.readiness", map[string]any{"timeoutSeconds": 30}),
		sx("probe without budget", pipeline.CodeMissingField, "readiness.timeoutSeconds", "spec.services.db.readiness.timeoutSeconds", nil),
		sx("startup budget too small", pipeline.CodeInvalidProbe, "readiness.timeoutSeconds", "spec.services.db.readiness.timeoutSeconds", 4),
		sx("startup budget too large", pipeline.CodeInvalidProbe, "readiness.timeoutSeconds", "spec.services.db.readiness.timeoutSeconds", 601),
		sx("period zero", pipeline.CodeInvalidProbe, "readiness.periodSeconds", "spec.services.db.readiness.periodSeconds", 0),
		sx("period too large", pipeline.CodeInvalidProbe, "readiness.periodSeconds", "spec.services.db.readiness.periodSeconds", 61),
		sx("probe timeout too large", pipeline.CodeInvalidProbe, "readiness.probeTimeoutSeconds", "spec.services.db.readiness.probeTimeoutSeconds", 61),
		sx("probe timeout above budget", pipeline.CodeInvalidProbe, "readiness.probeTimeoutSeconds", "spec.services.db.readiness", map[string]any{"tcp": "sql", "timeoutSeconds": 5, "probeTimeoutSeconds": 10}),
		sx("loss window too small", pipeline.CodeInvalidProbe, "readiness.lossSeconds", "spec.services.db.readiness.lossSeconds", 4),
		sx("loss window too large", pipeline.CodeInvalidProbe, "readiness.lossSeconds", "spec.services.db.readiness.lossSeconds", 601),

		// Resources.
		sx("cpu too small", pipeline.CodeInvalidResources, "resources.cpuMillis", "spec.services.db.resources.cpuMillis", 49),
		sx("cpu too large", pipeline.CodeInvalidResources, "resources.cpuMillis", "spec.services.db.resources.cpuMillis", 16001),
		sx("memory too small", pipeline.CodeInvalidResources, "resources.memoryMiB", "spec.services.db.resources.memoryMiB", 15),
		sx("memory too large", pipeline.CodeInvalidResources, "resources.memoryMiB", "spec.services.db.resources.memoryMiB", 65537),
		sx("ephemeral too small", pipeline.CodeInvalidResources, "resources.ephemeralMiB", "spec.services.db.resources.ephemeralMiB", 15),
		sx("ephemeral too large", pipeline.CodeInvalidResources, "resources.ephemeralMiB", "spec.services.db.resources.ephemeralMiB", 262145),
		sx("shm too large", pipeline.CodeInvalidResources, "resources.shmMiB", "spec.services.db.resources.shmMiB", 4097),
		sx("shm negative", pipeline.CodeInvalidResources, "resources.shmMiB", "spec.services.db.resources.shmMiB", -1),
		sx("resource member missing", pipeline.CodeMissingField, "resources.memoryMiB", "spec.services.db.resources.memoryMiB", nil),
		sx("resources unknown field", pipeline.CodeUnknownField, "resources.gpu", "spec.services.db.resources.gpu", 1),
		sx("scratch too small", pipeline.CodeInvalidResources, "runWorkspace.sizeMiB", "spec.runWorkspace", map[string]any{"sizeMiB": 63}),
		sx("scratch too large", pipeline.CodeInvalidResources, "runWorkspace.sizeMiB", "spec.runWorkspace", map[string]any{"sizeMiB": 1048577}),
		sx("empty runWorkspace", pipeline.CodeMissingField, "runWorkspace.sizeMiB", "spec.runWorkspace", map[string]any{}),

		// Storage.
		sx("relative storage path", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "data", "sizeMiB": 1}}),
		sx("storage dotdot", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/var/../etc", "sizeMiB": 1}}),
		sx("storage trailing slash", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/var/data/", "sizeMiB": 1}}),
		sx("storage double slash", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/var//data", "sizeMiB": 1}}),
		sx("storage dot segment", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/var/./data", "sizeMiB": 1}}),
		sx("storage root", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/", "sizeMiB": 1}}),
		sx("storage control char", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/var/a\u0001b", "sizeMiB": 1}}),
		sx("storage too long", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/" + strings.Repeat("a", 512), "sizeMiB": 1}}),
		sx("storage at /workspace", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/workspace", "sizeMiB": 1}}),
		sx("storage under /workspace", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/workspace/data", "sizeMiB": 1}}),
		sx("storage under /kodama", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/kodama/secrets", "sizeMiB": 1}}),
		sx("storage under /proc", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/proc/x", "sizeMiB": 1}}),
		sx("storage under /sys", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/sys", "sizeMiB": 1}}),
		sx("storage under /dev", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/dev/shm", "sizeMiB": 1}}),
		sx("storage under /run/secrets", pipeline.CodeInvalidStorage, "storage[0].path", "spec.services.db.storage", []any{map[string]any{"path": "/run/secrets/x", "sizeMiB": 1}}),
		sx("storage similar prefix is allowed elsewhere but overlap is not", pipeline.CodeInvalidStorage, "services.db.storage", "spec.services.db.storage", []any{map[string]any{"path": "/var/data", "sizeMiB": 1}, map[string]any{"path": "/var/data/sub", "sizeMiB": 1}}),
		sx("storage ancestor separated by an unrelated sibling", pipeline.CodeInvalidStorage, "services.db.storage", "spec.services.db.storage", []any{map[string]any{"path": "/var/data", "sizeMiB": 1}, map[string]any{"path": "/var/data-other", "sizeMiB": 1}, map[string]any{"path": "/var/data/sub", "sizeMiB": 1}}),
		sx("storage descendant declared first", pipeline.CodeInvalidStorage, "services.db.storage", "spec.services.db.storage", []any{map[string]any{"path": "/var/data/sub", "sizeMiB": 1}, map[string]any{"path": "/var/data-other", "sizeMiB": 1}, map[string]any{"path": "/var/data", "sizeMiB": 1}}),
		sx("duplicate storage path", pipeline.CodeInvalidStorage, "services.db.storage", "spec.services.db.storage", []any{map[string]any{"path": "/var/data", "sizeMiB": 1}, map[string]any{"path": "/var/data", "sizeMiB": 2}}),
		sx("five volumes", pipeline.CodeInvalidStorage, "services.db.storage", "spec.services.db.storage", many("v", 5, func(i int) any { return map[string]any{"path": fmt.Sprintf("/data/%d", i), "sizeMiB": 1} })),
		sx("volume size zero", pipeline.CodeInvalidStorage, "storage[0].sizeMiB", "spec.services.db.storage", []any{map[string]any{"path": "/var/data", "sizeMiB": 0}}),
		sx("volume size too large", pipeline.CodeInvalidStorage, "storage[0].sizeMiB", "spec.services.db.storage", []any{map[string]any{"path": "/var/data", "sizeMiB": 262145}}),
		sx("volume without size", pipeline.CodeMissingField, "storage[0].sizeMiB", "spec.services.db.storage", []any{map[string]any{"path": "/var/data"}}),

		// Edges.
		sx("edge from unknown service", pipeline.CodeUnknownService, "edges[0].from", "spec.network.edges.0.from", "ghost"),
		sx("edge to unknown service", pipeline.CodeUnknownService, "edges[0].to", "spec.network.edges.0.to", "ghost"),
		sx("self edge", pipeline.CodeInvalidEdge, "edges[0]", "spec.network.edges.0.from", "db"),
		sx("edge undeclared port", pipeline.CodeUnknownPort, "edges[0].ports[0]", "spec.network.edges.0.ports", []any{"nope"}),
		sx("edge port of the source service", pipeline.CodeUnknownPort, "edges[0].ports[0]", "spec.network.edges.0.ports", []any{"kv"}),
		sx("edge without ports", pipeline.CodeInvalidEdge, "edges[0].ports", "spec.network.edges.0.ports", []any{}),
		sx("edge duplicate ports", pipeline.CodeInvalidEdge, "edges[0].ports", "spec.network.edges.0.ports", []any{"sql", "sql"}),
		sx("duplicate pair", pipeline.CodeInvalidEdge, "edges[1]", "spec.network.edges", []any{map[string]any{"from": "cache", "to": "db", "ports": []any{"sql"}}, map[string]any{"from": "cache", "to": "db", "ports": []any{"admin"}}}),
		sx("too many edges", pipeline.CodeInvalidEdge, "network.edges", "spec.network.edges", many("e", 33, map[string]any{"from": "cache", "to": "db", "ports": []any{"sql"}})),
		sx("edge without ports field", pipeline.CodeMissingField, "edges[0].ports", "spec.network.edges.0.ports", nil),

		// Egress, roles and counts.
		sx("egress host", pipeline.CodeInvalidEgress, "services.db.egress[0]", "spec.services.db.egress", []any{"registry.example.com"}),
		sx("egress cidr", pipeline.CodeInvalidEgress, "services.db.egress[0]", "spec.services.db.egress", []any{"10.0.0.0/8"}),
		sx("egress port", pipeline.CodeInvalidEgress, "services.db.egress[0]", "spec.services.db.egress", []any{"registry:443"}),
		sx("egress object", pipeline.CodeInvalidType, "services.db.egress[0]", "spec.services.db.egress", []any{map[string]any{"host": "x"}}),
		sx("nine egress names", pipeline.CodeInvalidEgress, "services.db.egress", "spec.services.db.egress", many("e", 9, func(i int) any { return fmt.Sprintf("p%d", i) })),
		sx("duplicate egress name", pipeline.CodeInvalidEgress, "services.db.egress", "spec.services.db.egress", []any{"registry", "registry"}),
		sx("step egress host", pipeline.CodeInvalidEgress, "steps[0].egress[0]", "spec.steps.0.egress", []any{"registry.example.com"}),
		sx("step nine egress names", pipeline.CodeInvalidEgress, "steps[0].egress", "spec.steps.0.egress", many("e", 9, func(i int) any { return fmt.Sprintf("p%d", i) })),
		sx("unknown role", pipeline.CodeInvalidRole, "services.db.role", "spec.services.db.role", "root"),
		sx("two builders", pipeline.CodeInvalidRole, "services.db.role", "spec.services.cache.role", "builder", "spec.services.db.role", "builder"),
		sx("nine services", pipeline.CodeLimitExceeded, "spec.services", "spec.services", func() any {
			m := map[string]any{}
			for i := 0; i < 9; i++ {
				m[fmt.Sprintf("s%d", i)] = map[string]any{"image": "registry.example.invalid/s@" + v2Digest1, "ports": map[string]any{"p": map[string]any{"port": 80}},
					"resources": map[string]any{"cpuMillis": 100, "memoryMiB": 64, "ephemeralMiB": 64}, "readiness": map[string]any{"tcp": "p", "timeoutSeconds": 10}}
			}
			return m
		}(), "spec.network.edges", []any{}, "spec.steps.0.env", nil),
		sx("service name uppercase", pipeline.CodeInvalidFormat, "services", "spec.services", map[string]any{"DB": map[string]any{}}, "spec.network.edges", []any{}, "spec.steps.0.env", nil),
		sx("service name too long", pipeline.CodeInvalidFormat, "services", "spec.services", map[string]any{strings.Repeat("a", 31): map[string]any{}}, "spec.network.edges", []any{}, "spec.steps.0.env", nil),

		// Names as keys.
		sx("service env name starts with a digit", pipeline.CodeInvalidFormat, "services.db.env", "spec.services.db.env", map[string]any{"1BAD": "x"}),
		sx("service secret file name with a slash", pipeline.CodeInvalidFormat, "services.db.secretFiles", "spec.services.db.secretFiles", map[string]any{"a/b": "secret://project/test-db-password"}, "spec.services.db.command", []any{"/bin/db"}),
		sx("input name uppercase", pipeline.CodeInvalidFormat, "spec.inputs", "spec.inputs", map[string]any{"Region": map[string]any{"type": "string"}}),
		sx("output name uppercase", pipeline.CodeInvalidFormat, "steps[0].outputs", "spec.steps.0.outputs", map[string]any{"Report": map[string]any{"type": "string"}}),
		sx("step env name with a dash", pipeline.CodeInvalidFormat, "steps[0].env", "spec.steps.0.env", map[string]any{"A-B": "x"}),

		// Deadlines.
		sx("missing run deadline", pipeline.CodeMissingField, "spec.timeoutSeconds", "spec.timeoutSeconds", nil),
		sx("run deadline too small", pipeline.CodeInvalidTimeout, "spec.timeoutSeconds", "spec.timeoutSeconds", 59, "spec.steps.0.timeoutSeconds", 10),
		sx("run deadline too large", pipeline.CodeInvalidTimeout, "spec.timeoutSeconds", "spec.timeoutSeconds", 604801),
		sx("step timeout above ceiling", pipeline.CodeInvalidTimeout, "steps[0].timeoutSeconds", "spec.steps.0.timeoutSeconds", 604801),
		sm("step timeout above the run deadline", pipeline.CodeStepTimeoutExceedsRun, "steps[0].timeoutSeconds", "spec.steps.0.timeoutSeconds", 1801),

		// Argv.
		sx("empty command", pipeline.CodeInvalidValue, "services.db.command", "spec.services.db.command", []any{}),
		sx("sixty-five command elements", pipeline.CodeInvalidValue, "services.db.command", "spec.services.db.command", many("c", 65, "x")),
		sx("257 args", pipeline.CodeInvalidValue, "services.db.args", "spec.services.db.args", many("a", 257, "x")),
		sx("overlong arg", pipeline.CodeInvalidValue, "services.db.args[0]", "spec.services.db.args", []any{strings.Repeat("x", 8193)}),
		sx("object command", pipeline.CodeInvalidType, "services.db.command", "spec.services.db.command", map[string]any{}),
		sx("string command", pipeline.CodeInvalidType, "services.db.command", "spec.services.db.command", "serve"),
		sx("env over 64", pipeline.CodeLimitExceeded, "services.db.env", "spec.services.db.env", func() any {
			m := map[string]any{}
			for i := 0; i < 65; i++ {
				m[fmt.Sprintf("V%d", i)] = "x"
			}
			return m
		}()),
		sx("secret files over 16", pipeline.CodeLimitExceeded, "services.db.secretFiles", "spec.services.db.secretFiles", func() any {
			m := map[string]any{}
			for i := 0; i < 17; i++ {
				m[fmt.Sprintf("f%d", i)] = "secret://project/test-db-password"
			}
			return m
		}(), "spec.services.db.command", []any{"/bin/db"}),

		// Semantic: secrets and expressions.
		sm("service secret without command", pipeline.CodeServiceSecretRequiresCommand, "services.db.command", "spec.services.db.secretFiles", map[string]any{"pw": "secret://project/test-db-password"}),
		sm("service secret env without command", pipeline.CodeServiceSecretRequiresCommand, "services.db.command", "spec.services.db.env", map[string]any{"PW": map[string]any{"secretRef": "secret://project/test-db-password"}}),
		sm("service secret literal", pipeline.CodeInvalidSecretRef, "services.db.env.PW", "spec.services.db.env", map[string]any{"PW": "secret://project/test-db-password"}),
		sm("service secret denied", pipeline.CodeSecretDenied, "services.db.env.PW.secretRef", "spec.services.db.command", []any{"/bin/db"}, "spec.services.db.env", map[string]any{"PW": map[string]any{"secretRef": "secret://project/file-only"}}),
		sm("service unknown secret", pipeline.CodeSecretDenied, "services.db.secretFiles.pw", "spec.services.db.command", []any{"/bin/db"}, "spec.services.db.secretFiles", map[string]any{"pw": "secret://project/ghost"}),
		sm("step endpoint of an undeclared service", pipeline.CodeUnknownService, "steps[0].env.PGHOST", "spec.steps.0.env.PGHOST", "${{ services.ghost.host }}"),
		sm("step endpoint of an undeclared port", pipeline.CodeUnknownPort, "steps[0].env.PGPORT", "spec.steps.0.env.PGPORT", "${{ services.db.ports.nope }}"),
		sm("endpoint interpolated into text", pipeline.CodeInterpolation, "steps[0].env.PGHOST", "spec.steps.0.env.PGHOST", "host-${{ services.db.host }}"),
		sm("endpoint with extra path", pipeline.CodeInterpolation, "steps[0].env.PGHOST", "spec.steps.0.env.PGHOST", "${{ services.db.hostname }}"),
		sm("endpoint in step arg", pipeline.CodeUnknownService, "steps[0].args[0]", "spec.steps.0.args", []any{"${{ services.ghost.host }}"}),
		sm("endpoint in step command", pipeline.CodeInterpolation, "steps[0].command[0]", "spec.steps.0.command", []any{"${{ services.db.host }}"}),
		sx("endpoint in step image", pipeline.CodeMutableImage, "steps[0].image", "spec.steps.0.image", "${{ services.db.host }}@"+v2Digest1),
		sm("endpoint in output path", pipeline.CodeInterpolation, "outputs.o.path", "spec.steps.0.outputs", map[string]any{"o": map[string]any{"type": "artifact", "path": "/kodama/outputs/${{ services.db.host }}"}}),
		sm("endpoint in gate evidence", pipeline.CodeInterpolation, "steps[1].gate.evidence[0]", "spec.steps", []any{
			map[string]any{"id": "test", "kind": "container", "image": "registry.example.invalid/tools@" + v2Digest1, "command": []any{"/bin/true"}, "timeoutSeconds": 60},
			map[string]any{"id": "review", "kind": "gate", "needs": []any{"test"}, "gate": map[string]any{"evidence": []any{"${{ services.db.host }}"}}}}),
		sm("endpoint in service arg", pipeline.CodeInterpolation, "services.db.args[0]", "spec.services.db.args", []any{"${{ services.cache.host }}"}),
		sm("endpoint in service command", pipeline.CodeInterpolation, "services.db.command[0]", "spec.services.db.command", []any{"${{ services.cache.host }}"}),
		sm("endpoint in probe exec", pipeline.CodeInterpolation, "readiness.exec[0]", "spec.services.db.readiness", map[string]any{"exec": []any{"${{ services.cache.host }}"}, "timeoutSeconds": 30}),
		sm("service env endpoint with its edge", "", "", "spec.services.cache.env.DB", "${{ services.db.host }}"), // valid: control
		sm("service self reference", pipeline.CodeServiceSelfReference, "services.db.env.ME", "spec.services.db.env.ME", "${{ services.db.host }}"),
		sm("service self port reference", pipeline.CodeServiceSelfReference, "services.db.env.ME", "spec.services.db.env.ME", "${{ services.db.ports.sql }}"),
		sm("service step reference", pipeline.CodeServiceStepReference, "services.db.env.X", "spec.services.db.env.X", "${{ steps.test.outputs.o }}"),
		sm("service reference to a step output of a real producer", pipeline.CodeServiceStepReference, "services.cache.env.X", "spec.services.cache.env.X", "${{ steps.test.outputs.commit }}"),
		sm("service endpoint without an edge", pipeline.CodeEdgeUndeclared, "services.db.env.C", "spec.services.db.env.C", "${{ services.cache.host }}"),
		sm("service endpoint of an undeclared port", pipeline.CodeUnknownPort, "services.cache.env.C", "spec.services.cache.env.C", "${{ services.db.ports.nope }}"),
		sm("service endpoint port outside the edge", pipeline.CodeEdgeUndeclared, "services.cache.env.C", "spec.services.cache.env.C", "${{ services.db.ports.admin }}"),
		sm("service endpoint of an unknown service", pipeline.CodeUnknownService, "services.cache.env.C", "spec.services.cache.env.C", "${{ services.ghost.host }}"),
		sm("service endpoint interpolation", pipeline.CodeInterpolation, "services.cache.env.C", "spec.services.cache.env.C", "x-${{ services.db.host }}"),
		sm("service unknown input", pipeline.CodeUnknownInput, "services.cache.env.C", "spec.services.cache.env.C", "${{ inputs.ghost }}"),
		sm("service artifact input", pipeline.CodeTypeMismatch, "services.cache.env.C", "spec.inputs.art", map[string]any{"type": "artifact"}, "spec.services.cache.env.C", "${{ inputs.art }}"),
		sm("parallel writers stay forbidden", pipeline.CodeParallelWriters, "steps[1]", "spec.steps", []any{
			map[string]any{"id": "a", "kind": "container", "image": "registry.example.invalid/tools@" + v2Digest1, "command": []any{"/bin/true"}, "timeoutSeconds": 60, "workspace": map[string]any{"access": "write"}},
			map[string]any{"id": "b", "kind": "container", "image": "registry.example.invalid/tools@" + v2Digest1, "command": []any{"/bin/true"}, "timeoutSeconds": 60, "workspace": map[string]any{"access": "write"}}}),
		sm("a step cannot reference a missing service output", pipeline.CodeUnknownStep, "steps[0].env.X", "spec.steps.0.env.X", "${{ steps.ghost.outputs.o }}"),
		sx("agent step egress", pipeline.CodeUnknownField, "steps[0].egress", "spec.steps", []any{map[string]any{"id": "a", "kind": "agent", "agent": map[string]any{"profile": "coding-v1"}, "timeoutSeconds": 60, "egress": []any{"registry"}}}),
		sx("gate egress", pipeline.CodeGateField, "steps[1].egress", "spec.steps", []any{
			map[string]any{"id": "test", "kind": "container", "image": "registry.example.invalid/tools@" + v2Digest1, "command": []any{"/bin/true"}, "timeoutSeconds": 60},
			map[string]any{"id": "review", "kind": "gate", "needs": []any{"test"}, "egress": []any{"registry"}}}),
		sx("conditional gate", pipeline.CodeConditionalGate, "steps[1].when", "spec.inputs.d", map[string]any{"type": "boolean"}, "spec.steps", []any{
			map[string]any{"id": "test", "kind": "container", "image": "registry.example.invalid/tools@" + v2Digest1, "command": []any{"/bin/true"}, "timeoutSeconds": 60},
			map[string]any{"id": "review", "kind": "gate", "needs": []any{"test"}, "when": map[string]any{"input": "d", "equals": true}}}),
	}
	sentinels := []string{"registry.example.invalid", "registry.example.com", "ghost", "nope", "secret://", "test-db-password", "/var/data"}
	type publishedCase struct {
		Name        string `json:"name"`
		Format      string `json:"format"`
		SchemaValid bool   `json:"schemaValid"`
		Stage       string `json:"stage"`
		Code        string `json:"code"`
		Path        string `json:"path"`
		Document    string `json:"document"`
	}
	var published []publishedCase
	sch, err := pipelinev1alpha2.Schema()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := mutate(t, c.edits...)
			_, err := e.validateJSON(raw)
			if c.code == "" {
				if err != nil {
					t.Fatalf("control must be valid: %v", err)
				}
				return
			}
			inst, jerr := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if jerr != nil {
				t.Fatal(jerr)
			}
			published = append(published, publishedCase{Name: c.name, Format: "json", SchemaValid: sch.Validate(inst) == nil, Stage: string(c.stage), Code: c.code, Path: c.path, Document: string(raw)})
			expectCode(t, err, c.code)
			if !hasDiag(err, c.code, c.path) {
				t.Fatalf("code %s is not reported at a path containing %q: %v", c.code, c.path, err.(*pipeline.ValidationError).Diagnostics)
			}
			if got := stageOf(err); got != c.stage {
				t.Fatalf("stage %s, want %s", got, c.stage)
			}
			for _, d := range err.(*pipeline.ValidationError).Diagnostics {
				if !strings.HasPrefix(d.Path, "$") || d.Message == "" {
					t.Fatalf("malformed diagnostic %+v", d)
				}
				for _, s := range sentinels {
					if strings.Contains(d.Message, s) {
						t.Fatalf("message %q echoes document content %q", d.Message, s)
					}
				}
			}
		})
	}

	body, err := json.MarshalIndent(published, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	golden := filepath.Join(fixturesV2, "cases.json")
	if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readFixtureV2(t, "cases.json"); !bytes.Equal(want, body) {
		t.Fatal("contracts/pipeline/v1alpha2/fixtures/cases.json is out of date; regenerate with KODAMA_UPDATE_GOLDEN=1")
	}
}

// Test_v1alpha2_registration_opt_in: through the registration use case the
// format is refused with format_disabled unless the installation opted in, and
// nothing is persisted for the refused document. PX-21.
func Test_v1alpha2_registration_opt_in(t *testing.T) {
	src := string(readFixtureV2(t, "valid/service-test.yaml"))
	build := func(t *testing.T, e *env) (*pipeline.Service, *pipelinetest.MemRepo) {
		repo := &pipelinetest.MemRepo{}
		svc, err := pipeline.NewService(pipeline.ServiceConfig{Repository: repo, Authority: newAuthority(), Validator: e.v})
		if err != nil {
			t.Fatal(err)
		}
		return svc, repo
	}
	register := func(svc *pipeline.Service, format pipeline.Format, body []byte) (pipeline.RegisterResult, error) {
		return svc.Register(t.Context(), pipeline.RegisterCommand{Scope: scope, Caller: pipeline.Caller{PrincipalID: "u_engineer"}, Format: format, Body: body})
	}

	off, offRepo := build(t, newEnv(t))
	_, err := register(off, pipeline.FormatYAML, []byte(src))
	expectCode(t, err, pipeline.CodeFormatDisabled)
	_, err = register(off, pipeline.FormatJSON, readFixtureV2(t, "valid/service-test.json"))
	expectCode(t, err, pipeline.CodeFormatDisabled)
	if defs, _ := offRepo.List(t.Context(), scope, pipeline.ListQuery{Limit: 10}); len(defs) != 0 {
		t.Fatal("a refused format must persist nothing")
	}
	if res, err := register(off, pipeline.FormatYAML, []byte(doc(container("a")))); err != nil || !res.Created {
		t.Fatalf("v1alpha1 registration must be unchanged: %+v %v", res, err)
	}

	on, _ := build(t, newEnvV2(t))
	first, err := register(on, pipeline.FormatYAML, []byte(src))
	if err != nil || !first.Created || first.Definition.Name != "service-test" {
		t.Fatalf("opted-in registration: %+v %v", first, err)
	}
	again, err := register(on, pipeline.FormatJSON, first.Definition.Content.Canonical())
	if err != nil || again.Created || again.Definition.ID != first.Definition.ID {
		t.Fatalf("the canonical form must dedupe: %+v %v", again, err)
	}
	if _, err := first.Definition.Content.Document(); err != nil {
		t.Fatal(err)
	}
}

// Test_v1alpha2_source_defaults: the effective workspace access of a step is a
// pure function of the step and the run's source, never part of the definition.
// WS-13.
func Test_v1alpha2_source_defaults(t *testing.T) {
	ws := func(a pipeline.Access) *pipeline.Workspace { return &pipeline.Workspace{Access: a} }
	container := func(w *pipeline.Workspace) pipeline.Step {
		return pipeline.Step{ID: "a", Kind: pipeline.KindContainer, Workspace: w}
	}
	for name, tc := range map[string]struct {
		step    pipeline.Step
		backed  bool
		access  pipeline.Access
		refusal string
	}{
		"absent, source-backed": {container(nil), true, pipeline.AccessRead, ""},
		"absent, source-free":   {container(nil), false, pipeline.AccessNone, ""},
		"none, source-backed":   {container(ws(pipeline.AccessNone)), true, "", pipeline.AdmissionWorkspaceNoneSourceBacked},
		"none, source-free":     {container(ws(pipeline.AccessNone)), false, pipeline.AccessNone, ""},
		"read, source-backed":   {container(ws(pipeline.AccessRead)), true, pipeline.AccessRead, ""},
		"read, source-free":     {container(ws(pipeline.AccessRead)), false, "", pipeline.AdmissionWorkspaceAccessSourceFree},
		"write, source-backed":  {container(ws(pipeline.AccessWrite)), true, pipeline.AccessWrite, ""},
		"write, source-free":    {container(ws(pipeline.AccessWrite)), false, "", pipeline.AdmissionWorkspaceAccessSourceFree},
		"agent, source-backed":  {pipeline.Step{Kind: pipeline.KindAgent}, true, pipeline.AccessRead, ""},
		"agent write":           {pipeline.Step{Kind: pipeline.KindAgent, Workspace: ws(pipeline.AccessWrite)}, true, pipeline.AccessWrite, ""},
		"agent, source-free":    {pipeline.Step{Kind: pipeline.KindAgent}, false, "", pipeline.AdmissionAgentSourceFree},
		"gate, source-backed":   {pipeline.Step{Kind: pipeline.KindGate}, true, pipeline.AccessNone, ""},
		"gate, source-free":     {pipeline.Step{Kind: pipeline.KindGate}, false, pipeline.AccessNone, ""},
	} {
		access, refusal := pipeline.EffectiveAccess(tc.step, tc.backed)
		if access != tc.access || refusal != tc.refusal {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", name, access, refusal, tc.access, tc.refusal)
		}
	}

	t.Run("one definition digest serves both run kinds", func(t *testing.T) {
		e := newEnvV2(t)
		v := e.mustValidate(t, string(readFixtureV2(t, "valid/source-free-cleanup.yaml")))
		d, err := v.Document()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range d.Spec.Steps {
			if _, refusal := pipeline.EffectiveAccess(s, false); refusal != "" {
				t.Fatalf("%s refused for a source-free run: %s", s.ID, refusal)
			}
		}
		// The step that declares none is refused for a work run; the one that
		// declares nothing is allowed for both: that is the author's choice, not a
		// digest difference.
		if _, refusal := pipeline.EffectiveAccess(d.Spec.Steps[0], true); refusal != pipeline.AdmissionWorkspaceNoneSourceBacked {
			t.Fatalf("explicit none must be refused for a source-backed run, got %q", refusal)
		}
		if access, refusal := pipeline.EffectiveAccess(d.Spec.Steps[1], true); access != pipeline.AccessRead || refusal != "" {
			t.Fatalf("an absent workspace defaults to read for a source-backed run: %q %q", access, refusal)
		}
		if access, _ := pipeline.EffectiveAccess(d.Spec.Steps[1], false); access != pipeline.AccessNone {
			t.Fatal("an absent workspace defaults to none for a source-free run")
		}
		if strings.Contains(string(v.Canonical()), `"workspace":{"access":"read"}`) {
			t.Fatal("the source default must never be written back into the definition")
		}
	})
}

// Test_v1alpha2_execution_order_ties_and_gates: the order is the
// lexicographically smallest topological order by declaration index; a gate
// holds its position; needs are explicit and preserved. PX-20, WS-14.
func Test_v1alpha2_execution_order_ties_and_gates(t *testing.T) {
	st := func(id string, needs ...string) pipeline.Step {
		return pipeline.Step{ID: id, Kind: pipeline.KindContainer, Needs: needs}
	}
	gateStep := func(id string, needs ...string) pipeline.Step {
		return pipeline.Step{ID: id, Kind: pipeline.KindGate, Needs: needs}
	}
	for name, tc := range map[string]struct {
		steps []pipeline.Step
		want  string
	}{
		"contract row 1":            {[]pipeline.Step{st("a"), st("b", "a"), st("c"), st("d", "c")}, "a b c d"},
		"contract row 2":            {[]pipeline.Step{st("b", "c"), st("c"), st("a")}, "c b a"},
		"contract row 3 with gate":  {[]pipeline.Step{st("test"), gateStep("review", "test"), st("build", "review"), st("lint")}, "test review build lint"},
		"declaration order":         {[]pipeline.Step{st("x"), st("y"), st("z")}, "x y z"},
		"late dependency":           {[]pipeline.Step{st("a", "c"), st("b"), st("c")}, "b c a"},
		"diamond":                   {[]pipeline.Step{st("a"), st("b", "a"), st("c", "a"), st("d", "b", "c")}, "a b c d"},
		"tie unlocked by emission":  {[]pipeline.Step{st("a", "d"), st("b"), st("c"), st("d")}, "b c d a"},
		"gate between independents": {[]pipeline.Step{st("a"), gateStep("g", "a"), st("b"), st("c", "g")}, "a g b c"},
		"duplicate needs tolerated": {[]pipeline.Step{st("a"), st("b", "a", "a")}, "a b"},
	} {
		got, err := pipeline.ExecutionOrder(tc.steps)
		if err != nil || strings.Join(got, " ") != tc.want {
			t.Errorf("%s: got %v (%v), want %s", name, got, err, tc.want)
		}
	}
	// A run of ties is stable across repeated evaluation.
	steps := []pipeline.Step{st("e"), st("d"), st("c"), st("b", "e"), st("a", "b")}
	first, _ := pipeline.ExecutionOrder(steps)
	for i := 0; i < 20; i++ {
		again, _ := pipeline.ExecutionOrder(steps)
		if fmt.Sprint(again) != fmt.Sprint(first) {
			t.Fatal("the order must be deterministic")
		}
	}
	if fmt.Sprint(first) != "[e d c b a]" {
		t.Fatalf("unexpected order %v", first)
	}

	t.Run("invalid graphs are errors, never a partial order", func(t *testing.T) {
		for name, steps := range map[string][]pipeline.Step{
			"cycle":     {st("a", "b"), st("b", "a")},
			"self":      {st("a", "a")},
			"unknown":   {st("a", "ghost")},
			"duplicate": {st("a"), st("a")},
		} {
			if got, err := pipeline.ExecutionOrder(steps); err == nil || got != nil || !errors.Is(err, pipeline.ErrInvalid) {
				t.Errorf("%s: got %v, %v", name, got, err)
			}
		}
	})

	t.Run("validated documents expose the order including gates", func(t *testing.T) {
		e := newEnvV2(t)
		v := e.mustValidate(t, string(readFixtureV2(t, "valid/order-gates.yaml")))
		got, err := v.ExecutionOrder()
		if err != nil || strings.Join(got, " ") != "test review build lint" {
			t.Fatalf("got %v %v", got, err)
		}
		agent := e.mustValidate(t, string(readFixtureV2(t, "valid/agent-gate.yaml")))
		if got, _ := agent.ExecutionOrder(); strings.Join(got, " ") != "implement review ship" {
			t.Fatalf("got %v", got)
		}
	})
}

// Test_v1alpha2_stored_document_and_copies: stored v1alpha2 content decodes
// into a Document that keeps every new field, shares nothing with the stored
// value, and is judged canonical by the stored-content reader. PX-21.
func Test_v1alpha2_stored_document_and_copies(t *testing.T) {
	e := newEnvV2(t)
	v := e.mustValidate(t, string(readFixtureV2(t, "valid/edges-and-endpoints.yaml")))
	doc, err := v.Document()
	if err != nil {
		t.Fatal(err)
	}
	if !doc.IsV1Alpha2() || doc.Spec.TimeoutSeconds != 3600 || doc.Spec.RunWorkspace == nil || doc.Spec.RunWorkspace.SizeMiB != 512 {
		t.Fatalf("spec fields lost: %+v", doc.Spec)
	}
	app := doc.Spec.Services["app"]
	if len(doc.Spec.Services) != 3 || app.Role != pipeline.RoleGeneral || doc.Spec.Services["buildkit"].Role != pipeline.RoleBuilder ||
		app.Readiness.HTTP == nil || app.Readiness.HTTP.Path != "/healthz" || app.Resources.ShmMiB == nil || *app.Resources.ShmMiB != 0 ||
		len(app.Egress) != 1 || app.Ports["admin"].Port != 9090 || len(doc.Spec.Network.Edges) != 2 ||
		len(doc.Spec.Services["db"].Storage) != 2 || len(doc.Spec.Services["db"].Readiness.Exec) != 3 {
		t.Fatalf("service fields lost: %+v", doc.Spec.Services)
	}
	if got := doc.Spec.Steps[0]; got.Workspace == nil || got.Workspace.Access != pipeline.AccessRead || len(got.Egress) != 2 {
		t.Fatalf("step fields lost: %+v", got)
	}

	// Mutating one decoded Document never reaches the stored value or another copy.
	other, _ := v.Document()
	doc.Spec.Services["app"].Env["PLAIN"] = pipeline.EnvValue{}
	doc.Spec.Services["db"].Storage[0].Path = "/mutated"
	doc.Spec.Network.Edges[0].Ports[0] = "mutated"
	doc.Spec.Steps[0].Egress[0] = "mutated"
	*doc.Spec.Services["app"].Resources.ShmMiB = 99
	doc.Spec.Services["app"].Readiness.HTTP.Path = "/mutated"
	again, _ := v.Document()
	for _, d := range []pipeline.Document{other, again} {
		if d.Spec.Services["db"].Storage[0].Path != "/var/lib/db" || d.Spec.Network.Edges[0].Ports[0] != "sql" || d.Spec.Steps[0].Egress[0] != "registry" ||
			*d.Spec.Services["app"].Resources.ShmMiB != 0 || d.Spec.Services["app"].Readiness.HTTP.Path != "/healthz" || d.Spec.Services["app"].Env["PLAIN"].Scalar == nil {
			t.Fatal("decoded documents share state")
		}
	}
	if !bytes.Equal(v.Canonical(), readFixtureV2(t, "canonical/edges-and-endpoints.json")) {
		t.Fatal("the stored value changed")
	}

	t.Run("a non-canonical or mismatched store is refused", func(t *testing.T) {
		canonical := v.Canonical()
		spaced := bytes.Replace(canonical, []byte(`"kind":"Pipeline",`), []byte(`"kind": "Pipeline",`), 1)
		if _, err := pipeline.NewValidatedFromCanonical(spaced, pipeline.DigestOf(spaced)); !errors.Is(err, pipeline.ErrConflict) {
			t.Fatalf("expected a conflict for non-canonical bytes, got %v", err)
		}
		// A v1alpha2 document whose defaults were not filled is not canonical either.
		bare := e.mustValidateJSON(t, mutate(t))
		trimmed := bytes.Replace(bare.Canonical(), []byte(`"role":"general",`), nil, 1)
		if _, err := pipeline.NewValidatedFromCanonical(trimmed, pipeline.DigestOf(trimmed)); !errors.Is(err, pipeline.ErrConflict) {
			t.Fatalf("unfilled defaults must be non-canonical, got %v", err)
		}
		if _, err := pipeline.NewValidatedFromCanonical(canonical, v2Digest1); !errors.Is(err, pipeline.ErrConflict) {
			t.Fatalf("a digest mismatch must be refused, got %v", err)
		}
	})
}

// Test_v1alpha1_compatibility_preserved: the new format never changes what a
// kodama/v1alpha1 document means: a v1alpha2 field in a v1alpha1 document is an
// error, a service reference is still an interpolation error, workspace.from
// still works, and every v1alpha1 golden keeps its bytes and digest. PX-21.
func Test_v1alpha1_compatibility_preserved(t *testing.T) {
	e := newEnvV2(t) // even with the new format enabled
	for name, src := range map[string]string{
		"spec.timeoutSeconds":         strings.Replace(doc(container("a")), "spec:\n", "spec:\n  timeoutSeconds: 600\n", 1),
		"spec.services":               strings.Replace(doc(container("a")), "spec:\n", "spec:\n  services: {}\n", 1),
		"spec.network":                strings.Replace(doc(container("a")), "spec:\n", "spec:\n  network: {edges: []}\n", 1),
		"spec.runWorkspace":           strings.Replace(doc(container("a")), "spec:\n", "spec:\n  runWorkspace: {sizeMiB: 64}\n", 1),
		"step egress":                 doc(container("a", "egress: [registry]")),
		"step workspace without from": doc(container("a", "workspace: {access: read}")),
	} {
		_, err := e.validate(src)
		want := pipeline.CodeUnknownField
		if name == "step workspace without from" {
			want = pipeline.CodeMissingField // v1alpha1 still requires workspace.from
		}
		expectCode(t, err, want)
	}
	_, err := e.validate(doc(container("a", "env: {H: '${{ services.db.host }}'}")))
	expectCode(t, err, pipeline.CodeInterpolation)
	e.mustValidate(t, doc(container("a", "workspace: {access: write, from: run}")))

	t.Run("v1alpha1 goldens are byte-identical", func(t *testing.T) {
		for _, name := range []string{"minimal-test", "build-and-publish", "m2-agent-gate"} {
			v := e.mustValidate(t, string(readFixture(t, "valid/"+name+".yaml")))
			if !bytes.Equal(v.Canonical(), readFixture(t, "canonical/"+name+".json")) {
				t.Errorf("%s: v1alpha1 canonical bytes changed", name)
			}
			if string(v.Digest()) != strings.TrimSpace(string(readFixture(t, "canonical/"+name+".digest"))) {
				t.Errorf("%s: v1alpha1 digest changed", name)
			}
		}
	})

	t.Run("a typed v1alpha1 document carrying v1alpha2 fields is refused, not silently reduced", func(t *testing.T) {
		d := newEnv(t).mustValidate(t, doc(container("a")))
		parsed, err := d.Document()
		if err != nil {
			t.Fatal(err)
		}
		parsed.Spec.Services = map[string]pipeline.RunService{"db": {}}
		_, err = e.v.Validate(t.Context(), scope, actor, parsed)
		expectCode(t, err, pipeline.CodeUnknownField)
		parsed.Spec.Services = nil
		parsed.Spec.Steps[0].Egress = []string{"registry"}
		_, err = e.v.Validate(t.Context(), scope, actor, parsed)
		expectCode(t, err, pipeline.CodeUnknownField)
	})

	t.Run("a typed v1alpha2 document goes through every rule too", func(t *testing.T) {
		v := e.mustValidate(t, string(readFixtureV2(t, "valid/service-test.yaml")))
		parsed, _ := v.Document()
		parsed.Spec.TimeoutSeconds = 10
		parsed.Spec.Services["db"] = func() pipeline.RunService {
			s := parsed.Spec.Services["db"]
			s.Readiness.TCP = "ghost"
			s.Storage[0].Path = "/workspace/x"
			return s
		}()
		_, err := e.v.Validate(t.Context(), scope, actor, parsed)
		for _, code := range []string{pipeline.CodeInvalidTimeout, pipeline.CodeUnknownPort, pipeline.CodeInvalidStorage} {
			if !hasCode(err, code) {
				t.Errorf("typed document skipped %s: %v", code, codes(err))
			}
		}
	})
}

func storageDoc(t testing.TB, paths ...string) []byte {
	t.Helper()
	vols := make([]any, len(paths))
	for i, p := range paths {
		vols[i] = map[string]any{"path": p, "sizeMiB": 1}
	}
	return mutate(t, "spec.services.db.storage", vols)
}

func permutations(in []string) [][]string {
	if len(in) <= 1 {
		return [][]string{append([]string{}, in...)}
	}
	var out [][]string
	for i := range in {
		rest := append(append([]string{}, in[:i]...), in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{in[i]}, p...))
		}
	}
	return out
}

// Test_v1alpha2_storage_overlap_is_pairwise: a volume path that is an ancestor
// of another overlaps it on a component boundary however the paths sort or are
// ordered in the document; a name that merely shares a string prefix does not.
// Every permutation of up to four volumes is judged identically, as JSON and as
// YAML and as a typed document. IT-14.
func Test_v1alpha2_storage_overlap_is_pairwise(t *testing.T) {
	e := newEnvV2(t)
	for name, tc := range map[string]struct {
		paths   []string
		overlap bool
	}{
		"unrelated prefix between ancestor and child": {[]string{"/var/data", "/var/data-other", "/var/data/sub"}, true},
		"two unrelated prefixes between":              {[]string{"/a/b", "/a/b-1", "/a/b.x", "/a/b/c/d"}, true},
		"deep descendant":                             {[]string{"/x", "/x-y", "/x0", "/x/y/z"}, true},
		"identical":                                   {[]string{"/a", "/b", "/a"}, true},
		"string prefix only":                          {[]string{"/var/data", "/var/data-other", "/var/database", "/var/data2"}, false},
		"siblings":                                    {[]string{"/var/a", "/var/b", "/opt/a"}, false},
		"one volume":                                  {[]string{"/var/data"}, false},
	} {
		for _, perm := range permutations(tc.paths) {
			label := name + " " + strings.Join(perm, ",")
			raw := storageDoc(t, perm...)
			_, err := e.validateJSON(raw)
			if tc.overlap {
				expectCode(t, err, pipeline.CodeInvalidStorage)
			} else if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			if len(perm) < 3 {
				continue
			}
			// The same document as YAML.
			var yml strings.Builder
			var tree any
			if jerr := json.Unmarshal(raw, &tree); jerr != nil {
				t.Fatal(jerr)
			}
			yml.Write(raw) // JSON is YAML
			_, yerr := e.v.ValidateYAML(t.Context(), scope, actor, []byte(yml.String()))
			if (yerr != nil) != tc.overlap {
				t.Fatalf("%s: YAML verdict %v", label, yerr)
			}
			// And as a typed document built in code.
			v := newEnvV2(t)
			base := v.mustValidateJSON(t, mutate(t))
			parsed, perr := base.Document()
			if perr != nil {
				t.Fatal(perr)
			}
			svc := parsed.Spec.Services["db"]
			svc.Storage = nil
			for _, p := range perm {
				svc.Storage = append(svc.Storage, pipeline.ServiceStorage{Path: p, SizeMiB: 1})
			}
			parsed.Spec.Services["db"] = svc
			_, terr := v.v.Validate(t.Context(), scope, actor, parsed)
			if (terr != nil) != tc.overlap {
				t.Fatalf("%s: typed verdict %v", label, terr)
			}
		}
	}
}

// Test_v1alpha2_typed_scalar_discriminator: a typed Scalar whose Type is not
// string, boolean or integer is refused wherever a service or step environment
// holds one, before any value check, so a Validated document always canonicalizes
// to what was validated and reads back through the stored-content reader. The
// reported hole was an empty Type skipping the length, secret-literal and
// endpoint-edge checks while the canonical serializer wrote it as a string.
// PX-21.
func Test_v1alpha2_typed_scalar_discriminator(t *testing.T) {
	e := newEnvV2(t)
	base := e.mustValidateJSON(t, mutate(t))
	typed := func(mut func(*pipeline.Document)) (pipeline.Validated, error) {
		d, err := base.Document()
		if err != nil {
			t.Fatal(err)
		}
		mut(&d)
		return e.v.Validate(t.Context(), scope, actor, d)
	}
	setSvc := func(sc pipeline.Scalar) func(*pipeline.Document) {
		return func(d *pipeline.Document) {
			s := d.Spec.Services["cache"]
			s.Env["X"] = pipeline.EnvValue{Scalar: &sc}
			d.Spec.Services["cache"] = s
		}
	}
	setStep := func(sc pipeline.Scalar) func(*pipeline.Document) {
		return func(d *pipeline.Document) { d.Spec.Steps[0].Env["X"] = pipeline.EnvValue{Scalar: &sc} }
	}
	long := strings.Repeat("x", 8193)
	for name, sc := range map[string]pipeline.Scalar{
		"empty type, long string":     {Str: long},
		"empty type, secret literal":  {Str: "secret://project/test-db-password"},
		"empty type, unchecked edge":  {Str: "${{ services.cache.host }}"},
		"empty type, step reference":  {Str: "${{ steps.test.outputs.o }}"},
		"unknown type":                {Type: "float", Str: "x"},
		"artifact type":               {Type: pipeline.TypeArtifact, Str: "x"},
		"ociArtifact type":            {Type: pipeline.TypeOCIArtifact, Str: "x"},
		"case variant of string":      {Type: "String", Str: "x"},
		"empty type, integer payload": {Int: 7},
	} {
		for where, set := range map[string]func(pipeline.Scalar) func(*pipeline.Document){"service": setSvc, "step": setStep} {
			v, err := typed(set(sc))
			if err == nil {
				t.Errorf("%s in a %s env was accepted: %s", name, where, v.Canonical())
				continue
			}
			expectCode(t, err, pipeline.CodeInvalidType)
		}
	}
	// Supported discriminators still validate, and every Validated document
	// reads back through the stored-content reader with the same bytes.
	for name, sc := range map[string]pipeline.Scalar{
		"string": pipeline.StringScalar("x"), "empty string": pipeline.StringScalar(""), "boolean": pipeline.BoolScalar(true), "integer": pipeline.IntScalar(7),
		"endpoint with its edge": pipeline.StringScalar("${{ services.db.host }}"),
	} {
		for where, set := range map[string]func(pipeline.Scalar) func(*pipeline.Document){"service": setSvc, "step": setStep} {
			v, err := typed(set(sc))
			if where == "step" && name == "endpoint with its edge" {
				if err != nil {
					t.Errorf("%s %s: %v", name, where, err)
				}
			} else if err != nil {
				t.Errorf("%s %s: %v", name, where, err)
				continue
			}
			stored, serr := pipeline.NewValidatedFromCanonical(v.Canonical(), v.Digest())
			if serr != nil {
				t.Errorf("%s %s: the validated document is not readable as stored content: %v", name, where, serr)
				continue
			}
			if _, derr := stored.Document(); derr != nil || !bytes.Equal(stored.Canonical(), v.Canonical()) {
				t.Errorf("%s %s: round trip failed: %v", name, where, derr)
			}
		}
	}
	// An unchecked endpoint reference cannot dodge the edge policy this way: the
	// same expression with a real type is refused when no edge exists.
	_, err := typed(setSvc(pipeline.StringScalar("${{ services.db.ports.admin }}")))
	expectCode(t, err, pipeline.CodeEdgeUndeclared)
	_, err = typed(setSvc(pipeline.Scalar{Str: "${{ services.db.ports.admin }}"}))
	expectCode(t, err, pipeline.CodeInvalidType)
	// Input defaults and conditions are typed too: a mismatch is refused.
	_, err = typed(func(d *pipeline.Document) {
		in := d.Spec.Inputs["region"]
		in.Default = &pipeline.Scalar{Str: "eu"}
		d.Spec.Inputs["region"] = in
	})
	expectCode(t, err, pipeline.CodeTypeMismatch)
}

// Test_v1alpha2_diagnostics_never_echo_document_content: every finding of a
// kodama/v1alpha2 document carries constant text, including those of the shared
// graph, reference and duplicate rules, whose kodama/v1alpha1 messages name
// step, input and output identifiers. The v1 messages are asserted unchanged.
// PX-21.
func Test_v1alpha2_diagnostics_never_echo_document_content(t *testing.T) {
	e := newEnvV2(t)
	step := func(id string, extra map[string]any) map[string]any {
		m := map[string]any{"id": id, "kind": "container", "image": "registry.example.invalid/tools@" + v2Digest1, "command": []any{"/bin/true"}, "timeoutSeconds": 60}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	w := map[string]any{"access": "write"}
	for name, tc := range map[string]struct {
		code  string
		steps []any
		extra []any
		leak  []string
	}{
		"duplicate step":   {pipeline.CodeDuplicateStep, []any{step("zqxdup", nil), step("zqxdup", nil)}, nil, []string{"zqxdup"}},
		"cycle":            {pipeline.CodeCycle, []any{step("zqxone", map[string]any{"needs": []any{"zqxtwo"}}), step("zqxtwo", map[string]any{"needs": []any{"zqxone"}})}, nil, []string{"zqxone", "zqxtwo"}},
		"parallel writers": {pipeline.CodeParallelWriters, []any{step("zqxw1", map[string]any{"workspace": w}), step("zqxw2", map[string]any{"workspace": w})}, nil, []string{"zqxw1", "zqxw2"}},
		"unknown input":    {pipeline.CodeUnknownInput, []any{step("zqxs", map[string]any{"env": map[string]any{"A": "${{ inputs.zqxinput }}"}})}, nil, []string{"zqxinput"}},
		"unknown step":     {pipeline.CodeUnknownStep, []any{step("zqxs", map[string]any{"env": map[string]any{"A": "${{ steps.zqxghost.outputs.o }}"}})}, nil, []string{"zqxghost"}},
		"unknown need":     {pipeline.CodeUnknownStep, []any{step("zqxs", map[string]any{"needs": []any{"zqxmissing"}})}, nil, []string{"zqxmissing"}},
		"not an ancestor":  {pipeline.CodeNotAncestor, []any{step("zqxa", map[string]any{"outputs": map[string]any{"zqxout": map[string]any{"type": "string"}}}), step("zqxb", map[string]any{"env": map[string]any{"A": "${{ steps.zqxa.outputs.zqxout }}"}})}, nil, []string{"zqxa", "zqxout"}},
		"unknown output":   {pipeline.CodeUnknownOutput, []any{step("zqxa", nil), step("zqxb", map[string]any{"needs": []any{"zqxa"}, "env": map[string]any{"A": "${{ steps.zqxa.outputs.zqxnone }}"}})}, nil, []string{"zqxa", "zqxnone"}},
		"step kind":        {pipeline.CodeUnknownField, []any{step("zqxs", map[string]any{"agent": map[string]any{"profile": "zqxprofile"}})}, nil, []string{"zqxprofile", "container"}},
	} {
		raw := mutate(t, append([]any{"spec.services", nil, "spec.network", nil, "spec.steps", tc.steps}, tc.extra...)...)
		_, err := e.validateJSON(raw)
		expectCode(t, err, tc.code)
		for _, d := range err.(*pipeline.ValidationError).Diagnostics {
			for _, l := range tc.leak {
				if strings.Contains(d.Message, l) {
					t.Errorf("%s: message %q echoes %q", name, d.Message, l)
				}
			}
		}
	}
	// kodama/v1alpha1 is unchanged and still names identifiers by design.
	_, err := newEnv(t).validate(doc(container("dup"), container("dup")))
	var ve *pipeline.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Diagnostics[0].Message, "the step id dup is already used by step 0") {
		t.Fatalf("the v1alpha1 duplicate-step message changed: %v", err)
	}
}
