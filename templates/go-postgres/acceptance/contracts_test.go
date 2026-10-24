package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

var (
	scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	actor = pipeline.Actor{ID: "u_engineer", Type: pipeline.ActorUser}
)

type knownProfiles map[string]bool

func (k knownProfiles) LookupProfile(_ context.Context, _ domain.Scope, name string) (pipeline.ProfileInfo, error) {
	if _, _, err := agents.ParseReference(name); err != nil {
		return pipeline.ProfileInfo{Immutable: false}, nil // a bare alias moves
	}
	if !k[name] {
		return pipeline.ProfileInfo{}, pipeline.ErrReferenceNotFound
	}
	return pipeline.ProfileInfo{Immutable: true}, nil
}

type allowRefs struct{}

func (allowRefs) CheckSecret(context.Context, domain.Scope, pipeline.Actor, string, pipeline.SecretUse) error {
	return nil
}
func (allowRefs) CheckEnvironment(context.Context, domain.Scope, pipeline.Actor, domain.EnvironmentID) error {
	return nil
}

func validator(t testing.TB) *pipeline.Validator {
	t.Helper()
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		Profiles:     knownProfiles{"coding@1": true, "exploratory-qa@1": true},
		References:   allowRefs{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// renderPipelines runs the generated application's renderer with the sample values.
func renderPipelines(t testing.TB, app string, override map[string]string) map[string]string {
	t.Helper()
	values := readFile(t, filepath.Join(app, "config/pipeline-values.example.env"))
	var lines []string
	for _, l := range strings.Split(values, "\n") {
		if k, _, ok := strings.Cut(l, "="); ok && override != nil {
			if v, has := override[k]; has {
				l = k + "=" + v
			}
		}
		lines = append(lines, l)
	}
	vf := filepath.Join(t.TempDir(), "values.env")
	writeFile(t, vf, strings.Join(lines, "\n"))
	out := filepath.Join(t.TempDir(), "rendered")
	mustRun(t, app, nil, "sh", "ci/render-pipelines.sh", vf, out)
	files, _ := filepath.Glob(filepath.Join(out, "*.yaml"))
	rendered := map[string]string{}
	for _, f := range files {
		rendered[filepath.Base(f)] = readFile(t, f)
	}
	return rendered
}

func codes(err error) []string {
	var ve *pipeline.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out []string
	for _, d := range ve.Diagnostics {
		out = append(out, d.Code)
	}
	return out
}

func has(list []string, want ...string) bool {
	for _, l := range list {
		for _, w := range want {
			if l == w {
				return true
			}
		}
	}
	return false
}

// TestFactoryContracts: generated pipelines pass the merged validation; bare
// profiles, mutable images, invalid output/source references, malformed
// manifests and snapshot serialization drift are rejected. No services.
func TestFactoryContracts(t *testing.T) {
	app := generate(t)
	v := validator(t)
	ctx := context.Background()
	rendered := renderPipelines(t, app, nil)
	if len(rendered) != 3 {
		t.Fatalf("expected three pipeline examples, got %d", len(rendered))
	}

	t.Run("the rendered pipeline examples validate", func(t *testing.T) {
		for name, doc := range rendered {
			validated, err := v.ValidateYAML(ctx, scope, actor, []byte(doc))
			if err != nil {
				t.Errorf("%s: %v %v", name, err, codes(err))
				continue
			}
			if validated.Name() == "" {
				t.Errorf("%s: no pipeline name", name)
			}
		}
	})

	mutate := func(file, from, to string) string {
		doc, ok := rendered[file]
		if !ok || !strings.Contains(doc, from) {
			t.Fatalf("%s does not contain %q", file, from)
		}
		return strings.Replace(doc, from, to, 1)
	}
	reject := func(t *testing.T, doc string, want ...string) {
		t.Helper()
		_, err := v.ValidateYAML(ctx, scope, actor, []byte(doc))
		if err == nil {
			t.Fatalf("the document must be rejected (want one of %v)", want)
		}
		if !has(codes(err), want...) {
			t.Fatalf("rejected for %v, want one of %v", codes(err), want)
		}
	}

	t.Run("bare profile aliases are rejected", func(t *testing.T) {
		reject(t, mutate("inventory-quality-build.yaml", "coding@1", "coding"), pipeline.CodeMutableProfile)
	})
	t.Run("unknown profile versions are rejected", func(t *testing.T) {
		reject(t, mutate("inventory-quality-build.yaml", "coding@1", "coding@9"), pipeline.CodeUnknownProfile)
	})
	t.Run("mutable images are rejected", func(t *testing.T) {
		img := regexp.MustCompile(`registry\.example\.invalid/inventory/tools@sha256:[0-9a-f]{64}`).FindString(rendered["inventory-preview.yaml"])
		reject(t, strings.Replace(rendered["inventory-preview.yaml"], img, "registry.example.invalid/inventory/tools:latest", 1), pipeline.CodeMutableImage)
	})
	t.Run("invalid output references are rejected", func(t *testing.T) {
		reject(t, mutate("inventory-stage.yaml", "${{ steps.stage.outputs.deployment_id }}", "${{ steps.stage.outputs.nope }}"), pipeline.CodeUnknownOutput)
		reject(t, mutate("inventory-quality-build.yaml", "${{ inputs.registry_repository }}", "${{ inputs.missing }}"), pipeline.CodeUnknownInput)
		reject(t, mutate("inventory-quality-build.yaml", "${{ inputs.registry_repository }}", "${{ inputs.registry_repository }}/x"), pipeline.CodeInterpolation)
		reject(t, mutate("inventory-stage.yaml", "${{ steps.stage.outputs.app_url }}", "${{ steps.business-review.outputs.x }}"), pipeline.CodeNotAncestor, pipeline.CodeUnknownOutput)
		// The preview and stage pipelines receive the image as a run input: an OCI output cannot flow into a scalar site.
		withOCI := mutate("inventory-quality-build.yaml", "TOOLS_IMAGE: ", "IMAGE_FROM_BUILD: ${{ steps.build.outputs.image }}\n        TOOLS_IMAGE: ")
		if _, err := v.ValidateYAML(ctx, scope, actor, []byte(withOCI)); err == nil {
			t.Log("the merged validator now accepts an OCI output in a scalar site; the split into quality-build and preview/stage runs may be revisited")
		}
	})
	t.Run("invalid source references are rejected", func(t *testing.T) {
		reject(t, mutate("inventory-quality-build.yaml", "from: implement", "from: ghost"), pipeline.CodeInvalidSource, pipeline.CodeUnknownStep)
	})
	t.Run("an unrendered placeholder never reaches registration", func(t *testing.T) {
		vf := filepath.Join(t.TempDir(), "values.env")
		writeFile(t, vf, "TOOLS_IMAGE=registry.example.invalid/tools@sha256:"+strings.Repeat("0", 64)+"\n")
		if out, err := run(t, app, nil, "sh", "ci/render-pipelines.sh", vf, filepath.Join(t.TempDir(), "o")); err == nil {
			t.Fatalf("a partial values file must fail:\n%s", out)
		}
		vf2 := filepath.Join(t.TempDir(), "values2.env")
		writeFile(t, vf2, "TOOLS_IMAGE=a b\n")
		if _, err := run(t, app, nil, "sh", "ci/render-pipelines.sh", vf2, filepath.Join(t.TempDir(), "o")); err == nil {
			t.Fatal("an unsafe value must be refused")
		}
	})

	t.Run("script result manifests follow the entrypoint contract", func(t *testing.T) {
		decl := map[string]pipeline.Output{"deployment_id": {Type: pipeline.TypeString}, "app_url": {Type: pipeline.TypeString}, "evidence": {Type: pipeline.TypeArtifact, Path: "/kodama/outputs/deployment.json"}}
		good := `{"outcome":"completed","outputs":{"app_url":"http://x.example.invalid","deployment_id":"dep_0123456789abcdef0123456789abcdef"}}`
		if err := validateManifest([]byte(good), decl); err != nil {
			t.Fatalf("a valid manifest: %v", err)
		}
		if err := validateManifest([]byte(`{"outcome":"failed","reason":"rollout_timeout"}`), decl); err != nil {
			t.Fatalf("a valid failure: %v", err)
		}
		oci := map[string]pipeline.Output{"image": {Type: pipeline.TypeOCIArtifact}}
		if err := validateManifest([]byte(`{"outcome":"completed","outputs":{"image":{"digest":"sha256:`+strings.Repeat("a", 64)+`","registryRef":"registry.example.invalid/acme/inventory"}}}`), oci); err != nil {
			t.Fatalf("a valid OCI output: %v", err)
		}
		bad := map[string]string{
			"duplicate keys":                       `{"outcome":"completed","outcome":"failed"}`,
			"float":                                `{"outcome":"completed","outputs":{"n":1.5}}`,
			"unknown top-level field":              `{"outcome":"completed","extra":1}`,
			"unknown outcome":                      `{"outcome":"done"}`,
			"failure without a reason code format": `{"outcome":"failed","reason":"Not A Code"}`,
			"failure carrying outputs":             `{"outcome":"failed","reason":"x_y","outputs":{"app_url":"x"}}`,
			"undeclared output":                    `{"outcome":"completed","outputs":{"surprise":"x","app_url":"x","deployment_id":"d"}}`,
			"file artifact in manifest":            `{"outcome":"completed","outputs":{"app_url":"x","deployment_id":"d","evidence":"x"}}`,
			"missing declared output":              `{"outcome":"completed","outputs":{"app_url":"x"}}`,
			"wrong type":                           `{"outcome":"completed","outputs":{"app_url":1,"deployment_id":"d"}}`,
			"not JSON":                             `outcome=completed`,
			"trailing data":                        good + `{}`,
		}
		for name, raw := range bad {
			if err := validateManifest([]byte(raw), decl); err == nil {
				t.Errorf("%s: must be rejected", name)
			}
		}
		for name, raw := range map[string]string{
			"oci digest":        `{"outcome":"completed","outputs":{"image":{"digest":"sha256:xyz","registryRef":"r"}}}`,
			"oci extra field":   `{"outcome":"completed","outputs":{"image":{"digest":"sha256:` + strings.Repeat("a", 64) + `","registryRef":"r","tag":"latest"}}}`,
			"oci missing field": `{"outcome":"completed","outputs":{"image":{"digest":"sha256:` + strings.Repeat("a", 64) + `"}}}`,
		} {
			if err := validateManifest([]byte(raw), oci); err == nil {
				t.Errorf("%s: must be rejected", name)
			}
		}
	})

	t.Run("environment snapshot digests equal the factory's serialization", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "delivery")
		mustRun(t, app, nil, "go", "build", "-o", bin, "./cmd/delivery")
		type binding = runtimev1.SecretBinding
		cases := map[string]delivery.ConfigurationSnapshot{
			"preview": {Scope: scope, Environment: "env_preview", Kind: delivery.KindPreview, Target: "kind", Namespace: "acme-preview",
				Config: map[string]string{"REPLICAS": "1", "APP_URL": "http://x.example.invalid/?a=1&b=<2>", "NOTE": "ünïcode \"quoted\" \\ slash"}, PreviewTTL: 24 * time.Hour, Kubeconfig: "KUBECONFIG",
				Secrets: []binding{{Ref: "secret://project/kubeconfig", Version: 3, Name: "KUBECONFIG", Delivery: runtimev1.DeliveryFile}, {Ref: "secret://project/erp", Version: 12, Name: "ERP_TOKEN", Delivery: runtimev1.DeliveryEnv}}},
			"stage unsorted bindings": {Scope: scope, Environment: "env_stage", Kind: delivery.KindStage, Target: "prod-cluster", Namespace: "acme-stage",
				Config: map[string]string{}, Kubeconfig: "KUBECONFIG",
				Secrets: []binding{{Ref: "secret://project/z", Version: 1, Name: "ZED", Delivery: runtimev1.DeliveryFile}, {Ref: "secret://project/kubeconfig", Version: 1, Name: "KUBECONFIG", Delivery: runtimev1.DeliveryFile}, {Ref: "secret://project/a", Version: 2, Name: "ALPHA", Delivery: runtimev1.DeliveryEnv}}},
			"nil config": {Scope: scope, Environment: "env_stage", Kind: delivery.KindStage, Target: "kind", Namespace: "ns", Kubeconfig: "K",
				Secrets: []binding{{Ref: "secret://project/k", Version: 1, Name: "K", Delivery: runtimev1.DeliveryFile}}},
		}
		for name, f := range cases {
			want, err := f.ComputeDigest()
			if err != nil {
				t.Fatal(err)
			}
			// The application's wire form of the same snapshot.
			doc := map[string]any{"format": "kodama/environment-snapshot/v1", "tenant": string(f.Scope.TenantID), "project": string(f.Scope.ProjectID), "environment": string(f.Environment),
				"kind": string(f.Kind), "target": f.Target, "namespace": f.Namespace, "config": f.Config, "previewTtlSeconds": int64(f.PreviewTTL / time.Second), "kubeconfig": f.Kubeconfig, "secrets": f.Secrets}
			if f.Config == nil {
				doc["config"] = map[string]string{}
			}
			raw, _ := json.Marshal(doc)
			cmd := exec.Command(bin, "snapshot-digest")
			cmd.Stdin = bytes.NewReader(raw)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s: %v %s", name, err, errb.String())
			}
			if got := strings.TrimSpace(out.String()); got != string(want) {
				t.Errorf("%s: serialization drift: application %s, factory %s", name, got, want)
			}
		}
		// A snapshot with an unknown field or a different format is not accepted.
		cmd := exec.Command(bin, "snapshot-digest")
		cmd.Stdin = strings.NewReader(`{"format":"kodama/environment-snapshot/v1","extra":1}`)
		if err := cmd.Run(); err == nil {
			t.Error("unknown snapshot fields must be refused")
		}
	})
}

// ---- strict results-manifest validation (the entrypoint's documented contract) ----

var (
	reasonRE   = regexp.MustCompile(runtimev1.Patterns["ReasonCode"])
	digestRE   = regexp.MustCompile(runtimev1.Patterns["Digest"])
	registryRE = regexp.MustCompile(runtimev1.Patterns["RegistryRef"])
)

// validateManifest applies the rules of internal/entrypoint/README.md to a
// command results manifest: strict JSON without duplicate keys or floats, only
// known fields, scalar and OCI outputs matching their declarations, file
// artifacts never listed, and every declared non-file output present on success.
func validateManifest(raw []byte, declared map[string]pipeline.Output) error {
	if len(raw) > 1<<20 {
		return errors.New("manifest too large")
	}
	if err := noDuplicateKeys(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var doc struct {
		Outcome  string                     `json:"outcome"`
		Reason   string                     `json:"reason"`
		Question string                     `json:"question"`
		Outputs  map[string]json.RawMessage `json:"outputs"`
	}
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	switch doc.Outcome {
	case "failed":
		if doc.Reason != "" && !reasonRE.MatchString(doc.Reason) {
			return errors.New("invalid reason code")
		}
		if len(doc.Outputs) > 0 || doc.Question != "" {
			return errors.New("a failure carries no outputs or question")
		}
		return nil
	case "completed":
	default:
		return errors.New("unsupported outcome " + doc.Outcome)
	}
	if doc.Reason != "" || doc.Question != "" {
		return errors.New("a completed result carries no reason or question")
	}
	for name, val := range doc.Outputs {
		d, ok := declared[name]
		if !ok {
			return errors.New("undeclared output " + name)
		}
		switch d.Type {
		case pipeline.TypeString:
			var s string
			if json.Unmarshal(val, &s) != nil {
				return errors.New(name + " must be a string")
			}
		case pipeline.TypeBoolean:
			var b bool
			if json.Unmarshal(val, &b) != nil {
				return errors.New(name + " must be a boolean")
			}
		case pipeline.TypeInteger:
			var n json.Number
			if json.Unmarshal(val, &n) != nil || strings.ContainsAny(n.String(), ".eE+") {
				return errors.New(name + " must be an integer")
			}
		case pipeline.TypeOCIArtifact:
			var o struct{ Digest, RegistryRef string }
			d2 := json.NewDecoder(bytes.NewReader(val))
			d2.DisallowUnknownFields()
			var wire struct {
				Digest      string `json:"digest"`
				RegistryRef string `json:"registryRef"`
			}
			if err := d2.Decode(&wire); err != nil || !digestRE.MatchString(wire.Digest) || !registryRE.MatchString(wire.RegistryRef) {
				return errors.New(name + " must be {digest, registryRef}")
			}
			_ = o
		default:
			return errors.New(name + " is a file artifact and must not be listed in the manifest")
		}
	}
	for name, d := range declared {
		if d.Type == pipeline.TypeArtifact {
			continue
		}
		if _, ok := doc.Outputs[name]; !ok {
			return errors.New("declared output " + name + " is missing")
		}
	}
	return nil
}

// noDuplicateKeys rejects any JSON object with a repeated key.
func noDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				if seen[k.(string)] {
					return errors.New("duplicate key " + k.(string))
				}
				seen[k.(string)] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token()
		return err
	}
	return walk()
}

// declaredOutputs returns a step's declared outputs from a rendered pipeline.
func declaredOutputs(t testing.TB, doc, step string) map[string]pipeline.Output {
	t.Helper()
	parsed, err := pipeline.ParseYAML([]byte(doc), pipeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range parsed.Spec.Steps {
		if s.ID == step {
			return s.Outputs
		}
	}
	t.Fatalf("no step %s", step)
	return nil
}

var _ = os.Getenv

// TestConformanceEvidenceDeclaration: the coding example declares an ordinary
// candidate-conformance artifact, the review gate carries it as evidence, and the
// generated agent instructions describe the report the merged contract validates.
// This checks the template's declaration only; it does not claim that any
// profile or wrapper produces the artifact.
func TestConformanceEvidenceDeclaration(t *testing.T) {
	app := generate(t)
	v := validator(t)
	ctx := context.Background()
	rendered := renderPipelines(t, app, nil)
	doc := rendered["inventory-quality-build.yaml"]

	decl := declaredOutputs(t, doc, "implement")
	c, ok := decl["conformance"]
	if !ok || c.Type != pipeline.TypeArtifact || c.Path != "/kodama/outputs/conformance.json" {
		t.Fatalf("the agent step must declare the conformance artifact: %+v", decl)
	}
	parsed, err := pipeline.ParseYAML([]byte(doc), pipeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var evidence []string
	for _, s := range parsed.Spec.Steps {
		if s.ID == "review" && s.Gate != nil {
			evidence = s.Gate.Evidence
		}
	}
	want := "${{ steps.implement.outputs.conformance }}"
	if !contains(evidence, want) || !contains(evidence, "${{ steps.quality.outputs.report }}") {
		t.Fatalf("the review gate evidence must include the conformance artifact and the quality report: %v", evidence)
	}
	if _, err := v.ValidateYAML(ctx, scope, actor, []byte(doc)); err != nil {
		t.Fatalf("the rendered example validates: %v %v", err, codes(err))
	}

	t.Run("a reference to an undeclared conformance output is rejected", func(t *testing.T) {
		broken := strings.Replace(doc, "conformance: {type: artifact, path: /kodama/outputs/conformance.json}", "other: {type: artifact, path: /kodama/outputs/other.json}", 1)
		_, err := v.ValidateYAML(ctx, scope, actor, []byte(broken))
		if err == nil || !has(codes(err), pipeline.CodeUnknownOutput) {
			t.Fatalf("want %s, got %v", pipeline.CodeUnknownOutput, codes(err))
		}
	})
	t.Run("the artifact path cannot escape the output root", func(t *testing.T) {
		broken := strings.Replace(doc, "/kodama/outputs/conformance.json", "/workspace/conformance.json", 1)
		if _, err := v.ValidateYAML(ctx, scope, actor, []byte(broken)); err == nil {
			t.Fatal("a conformance artifact outside /kodama/outputs must be rejected")
		}
	})

	t.Run("the agent instructions name the candidate-bound report and its fail-closed rules", func(t *testing.T) {
		agents := readFile(t, filepath.Join(app, "AGENTS.md"))
		for _, phrase := range []string{"immutable", "/kodama/outputs/conformance.json", "unresolved deviation", "candidate commit", "`not_run`", "`needs_input`", "not approval"} {
			if !strings.Contains(agents, phrase) {
				t.Errorf("AGENTS.md does not state %q", phrase)
			}
		}
		for _, skill := range []string{"inventory-domain", "inventory-migrations", "inventory-delivery"} {
			s := readFile(t, filepath.Join(app, ".agents/skills", skill, "SKILL.md"))
			if !strings.Contains(s, "conformance report") {
				t.Errorf("skill %s must require reporting deviations in the conformance report", skill)
			}
		}
	})

	t.Run("a report with the fields the instructions list is accepted or refused by the merged contract", func(t *testing.T) {
		rev := runtimev1.GitCommit(strings.Repeat("a", 40))
		base, err := runtimev1.NewDesignBaseline([]string{"DQ-07", "INV-01"}, nil,
			[]runtimev1.SourceRef{{Repository: "https://git.example.invalid/acme/inventory.git", Revision: rev, Path: "docs/specification.md"}}, []string{"tenant-authority"})
		if err != nil {
			t.Fatal(err)
		}
		candidate := runtimev1.GitCommit(strings.Repeat("c", 40))
		report := runtimev1.ConformanceReport{BaselineDigest: base.Digest, CandidateCommit: candidate, Requirements: []string{"DQ-07", "INV-01"},
			Checks:               []runtimev1.ConformanceCheck{{ID: "tenant-authority", Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: []domain.ArtifactID{"art_report_1"}}},
			UnresolvedDeviations: []runtimev1.Deviation{}, Disposition: runtimev1.Conforms}
		if err := runtimev1.CheckConformance(report, base, candidate); err != nil {
			t.Fatalf("the report shape the instructions describe: %v", err)
		}
		if err := runtimev1.CheckConformance(report, base, runtimev1.GitCommit(strings.Repeat("d", 40))); err == nil {
			t.Error("a report for another candidate must be refused")
		}
		blocked := report
		blocked.UnresolvedDeviations = []runtimev1.Deviation{{Requirement: "INV-01", Reason: "needs a business decision"}}
		blocked.Disposition = runtimev1.DispBlocked
		if err := runtimev1.CheckConformance(blocked, base, candidate); err == nil {
			t.Error("an unresolved deviation blocks progression")
		}
		notRun := report
		notRun.Checks = []runtimev1.ConformanceCheck{{ID: "tenant-authority", Outcome: runtimev1.CheckNotRun, EvidenceArtifactIDs: []domain.ArtifactID{}}}
		notRun.Disposition = runtimev1.DispBlocked
		if err := runtimev1.CheckConformance(notRun, base, candidate); err == nil {
			t.Error("a check that did not run cannot conform")
		}
	})
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

// TestSnapshotBindingContract: the application's pre-effect verification agrees
// with the factory's CheckEnvironment on every binding relationship: each snapshot
// binding must be present identically in the pinned context; additional context
// bindings are permitted. Real Git for generation only.
func TestSnapshotBindingContract(t *testing.T) {
	app := generate(t)
	bin := filepath.Join(t.TempDir(), "delivery")
	mustRun(t, app, nil, "go", "build", "-o", bin, "./cmd/delivery")
	type binding = runtimev1.SecretBinding
	kube := binding{Ref: "secret://project/kubeconfig", Version: 3, Name: "KUBECONFIG", Delivery: runtimev1.DeliveryFile}
	erp := binding{Ref: "secret://project/erp", Version: 12, Name: "ERP_TOKEN", Delivery: runtimev1.DeliveryEnv}
	cursor := binding{Ref: "secret://project/cursor", Version: 1, Name: "CURSOR_KEY", Delivery: runtimev1.DeliveryFile}
	extra := binding{Ref: "secret://project/unrelated", Version: 1, Name: "UNRELATED", Delivery: runtimev1.DeliveryFile}
	snap := delivery.ConfigurationSnapshot{Scope: scope, Environment: "env_preview", Kind: delivery.KindPreview, Target: "kind", Namespace: "acme-preview",
		Config: map[string]string{"REPLICAS": "1"}, PreviewTTL: 24 * time.Hour, Kubeconfig: "KUBECONFIG", Secrets: []binding{cursor, erp, kube}, CreatedAt: time.Unix(1, 0).UTC()}
	d, err := snap.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	snap.Digest = d

	ctxWith := func(secrets ...binding) runtimev1.Context {
		pin := snap.Pin()
		return runtimev1.Context{TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_1", StepID: "deploy", AttemptID: "att_1", ActionKey: "run_1:deploy", Environment: &pin, Secrets: secrets}
	}
	changed := func(b binding, f func(*binding)) binding { f(&b); return b }
	cases := []struct {
		name    string
		secrets []binding
		accept  bool
	}{
		{"identical bindings", []binding{cursor, erp, kube}, true},
		{"additional unrelated binding", []binding{cursor, erp, kube, extra}, true},
		{"missing snapshot binding", []binding{erp, kube}, false},
		{"missing deployment credential", []binding{cursor, erp}, false},
		{"different version", []binding{cursor, changed(erp, func(b *binding) { b.Version++ }), kube}, false},
		{"different reference", []binding{cursor, changed(erp, func(b *binding) { b.Ref = "secret://project/other" }), kube}, false},
		{"different delivery form", []binding{cursor, changed(erp, func(b *binding) { b.Delivery = runtimev1.DeliveryFile }), kube}, false},
		{"renamed binding", []binding{cursor, changed(erp, func(b *binding) { b.Name = "ERP_TOKEN2" }), kube}, false},
	}
	wire, _ := json.Marshal(map[string]any{"format": "kodama/environment-snapshot/v1", "tenant": string(scope.TenantID), "project": string(scope.ProjectID),
		"environment": string(snap.Environment), "kind": string(snap.Kind), "target": snap.Target, "namespace": snap.Namespace, "config": snap.Config,
		"previewTtlSeconds": int64(snap.PreviewTTL / time.Second), "kubeconfig": snap.Kubeconfig, "secrets": snap.Secrets})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cx := ctxWith(c.secrets...)
			factoryOK := delivery.CheckEnvironment(cx, snap) == nil
			input, _ := json.Marshal(map[string]any{"context": cx, "snapshot": json.RawMessage(wire)})
			cmd := exec.Command(bin, "snapshot-verify", "--kind", "preview")
			cmd.Stdin = bytes.NewReader(input)
			var errb bytes.Buffer
			cmd.Stderr = &errb
			appOK := cmd.Run() == nil
			if factoryOK != c.accept || appOK != c.accept {
				t.Fatalf("factory accepts=%v application accepts=%v, want %v (%s)", factoryOK, appOK, c.accept, errb.String())
			}
		})
	}
}
