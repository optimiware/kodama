package execution

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// This file holds the real kodama/v1alpha2 regressions of the old-executor
// admission guard (checkDefinitionVersion, T-118): the registration capability
// is not an execution fence, and the current v1 step codec and executor cannot
// carry run_shared semantics. A v1alpha2 definition must be refused before
// admission creates any state, whichever route delivered it. Run admission of
// the new model stays closed through this guard until T-112/T-116 deliver the
// complete run-resource capability checks.

const v2Image = "registry.example.invalid/tools@sha256:1111111111111111111111111111111111111111111111111111111111111111"

// v2Documents are real, valid kodama/v1alpha2 definitions of the shapes a run
// could be admitted with: an ordinary work run, and the source-free cleanup run
// shapes (explicit none, and absent workspace that resolves to none), with and
// without services.
var v2Documents = map[string]string{
	"ordinary work run": `
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata: {name: ordinary}
spec:
  timeoutSeconds: 600
  steps:
    - {id: build, kind: container, image: ` + v2Image + `, command: [/bin/true], timeoutSeconds: 60}
`,
	"ordinary with services": `
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata: {name: withservices}
spec:
  timeoutSeconds: 600
  runWorkspace: {sizeMiB: 256}
  services:
    db:
      image: ` + v2Image + `
      ports: {sql: {port: 5432}}
      resources: {cpuMillis: 100, memoryMiB: 64, ephemeralMiB: 64}
      readiness: {tcp: sql, timeoutSeconds: 30}
  steps:
    - id: test
      kind: container
      image: ` + v2Image + `
      command: [/bin/true]
      env: {PGHOST: "${{ services.db.host }}"}
      egress: [registry]
      workspace: {access: write}
      timeoutSeconds: 60
`,
	"source-free cleanup, explicit none": `
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata: {name: cleanexplicit}
spec:
  timeoutSeconds: 600
  steps:
    - {id: teardown, kind: container, image: ` + v2Image + `, command: [/bin/true], timeoutSeconds: 60, workspace: {access: none}}
`,
	"source-free cleanup, absent workspace": `
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata: {name: cleanabsent}
spec:
  timeoutSeconds: 600
  steps:
    - {id: teardown, kind: container, image: ` + v2Image + `, command: [/bin/true], timeoutSeconds: 60}
    - {id: verify, kind: container, needs: [teardown], image: ` + v2Image + `, command: [/bin/true], timeoutSeconds: 60}
`,
}

// v2Definition registers and validates with the new format opted in, as an installation that
// enabled registration (and nothing else) would.
func v2Definition(t *testing.T, yaml string) pipeline.Definition {
	t.Helper()
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities:   pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		References:     pipelinetest.References{Secrets: map[string]bool{"token": true}},
		EnableV1Alpha2: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := pipeline.Actor{ID: "u1", Type: pipeline.ActorUser}
	validated, err := v.ValidateYAML(context.Background(), scope, actor, []byte(yaml))
	if err != nil {
		t.Fatalf("the fixture must be a valid v1alpha2 document: %v", err)
	}
	def, err := pipeline.NewDefinition(pipeline.Candidate{Scope: scope, ID: domain.DefinitionID("pd_" + validated.Name()), Content: validated, CreatedBy: actor, CreatedAt: t0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := def.Content.Document(); err != nil || doc.APIVersion != pipelinev1alpha2.APIVersion {
		t.Fatalf("not a v1alpha2 definition: %v", err)
	}
	return def
}

// restoredDefinition rebuilds a definition the way a persistence adapter does: from
// the stored canonical bytes and digest, without any registration capability.
func restoredDefinition(t *testing.T, def pipeline.Definition) pipeline.Definition {
	t.Helper()
	content, err := pipeline.NewValidatedFromCanonical(def.Content.Canonical(), def.Digest)
	if err != nil {
		t.Fatalf("stored v1alpha2 content must stay readable: %v", err)
	}
	out := def
	out.Content = content
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
	return out
}

// effectCounters count every read or effect that admission would perform after
// the version guard.
type effectCounters struct {
	work, sources, config, cleanup, environments atomic.Int32
}

type countedWork struct {
	n *effectCounters
}

func (c countedWork) Snapshot(context.Context, domain.Scope, domain.WorkItemID) (WorkSnapshot, error) {
	c.n.work.Add(1)
	return WorkSnapshot{ID: "wi_1", Actionable: true, ContentRevision: 1, Spec: "s"}, nil
}

type countedSources struct {
	fakeSources
	n *effectCounters
}

func (c *countedSources) Verify(ctx context.Context, s domain.Scope, i domain.WorkItemID, commit string) (SourceInfo, error) {
	c.n.sources.Add(1)
	return c.fakeSources.Verify(ctx, s, i, commit)
}

type countedConfig struct{ n *effectCounters }

func (c countedConfig) Resolve(context.Context, ConfigRequest) (ConfigResult, error) {
	c.n.config.Add(1)
	return ConfigResult{}, nil
}

func (c *effectCounters) total() int32 {
	return c.work.Load() + c.sources.Load() + c.config.Load() + c.environments.Load()
}

func v2Service(t *testing.T, defs Definitions, n *effectCounters, cleanup DeploymentCleanup) *Service {
	t.Helper()
	return service(t, Dependencies{Definitions: defs, Work: countedWork{n}, Sources: &countedSources{n: n}, Configuration: countedConfig{n}, Cleanup: cleanup})
}

func isGuardRefusal(err error) bool {
	return errors.Is(err, ErrAdmissionRefused) && IsBusinessError(err)
}

// Test_unit_pipeline_v2_ordinary_admission_refused_before_effects: a stored
// kodama/v1alpha2 definition is refused by ordinary run admission before the
// work item, the source, the configuration or the store is touched, from
// definitions delivered by the resolver port and by the stored-content reader
// alike. The new document could otherwise run under per-attempt semantics. PX-21.
func Test_unit_pipeline_v2_ordinary_admission_refused_before_effects(t *testing.T) {
	ctx := context.Background()
	for name, yaml := range v2Documents {
		def := v2Definition(t, yaml)
		for route, stored := range map[string]pipeline.Definition{"resolver": def, "stored canonical content": restoredDefinition(t, def)} {
			t.Run(name+"/"+route, func(t *testing.T) {
				var n effectCounters
				svc := v2Service(t, fakeDefs{defs: map[domain.DefinitionID]pipeline.Definition{stored.ID: stored}}, &n, nil)
				cmd := AdmitCommand{Scope: scope, WorkItemID: "wi_1", DefinitionID: stored.ID, ContentRevision: 1, SourceCommit: commitA}
				_, err := svc.Prepare(ctx, Caller{PrincipalID: "u1"}, cmd)
				if !isGuardRefusal(err) {
					t.Fatalf("Prepare must be a classified refusal, got %v", err)
				}
				// Admit reaches the nil store only after Prepare succeeded: it must
				// fail the same way and, with the nil store, would panic otherwise.
				if _, err := svc.Admit(ctx, Caller{PrincipalID: "u1"}, cmd); !isGuardRefusal(err) {
					t.Fatalf("Admit must be a classified refusal, got %v", err)
				}
				if n.total() != 0 {
					t.Fatalf("admission performed %d reads before refusing: %+v", n.total(), &n)
				}
				for _, leak := range []string{"kodama/v1alpha2", "registry.example", "services", "timeoutSeconds"} {
					if strings.Contains(err.Error(), leak) {
						t.Fatalf("the refusal echoes document content %q: %v", leak, err)
					}
				}
			})
		}
	}
}

// Test_unit_pipeline_v2_cleanup_admission_refused_before_effects: the internal
// source-free cleanup path refuses the same definitions, including the shapes a
// cleanup run would accept under v1 (explicit and absent workspace), before
// resolving configuration, environments or creating a run. PX-21, WS-13.
func Test_unit_pipeline_v2_cleanup_admission_refused_before_effects(t *testing.T) {
	ctx := context.Background()
	for name, yaml := range v2Documents {
		def := v2Definition(t, yaml)
		for route, stored := range map[string]pipeline.Definition{"resolver": def, "stored canonical content": restoredDefinition(t, def)} {
			t.Run(name+"/"+route, func(t *testing.T) {
				var n effectCounters
				svc := v2Service(t, fakeDefs{defs: map[domain.DefinitionID]pipeline.Definition{stored.ID: stored}}, &n, allowCleanup{})
				cmd := CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: stored.ID}
				if _, err := svc.PrepareCleanup(ctx, cmd); !isGuardRefusal(err) {
					t.Fatalf("PrepareCleanup must be a classified refusal, got %v", err)
				}
				if _, err := svc.AdmitCleanup(ctx, cmd); !isGuardRefusal(err) {
					t.Fatalf("AdmitCleanup must be a classified refusal, got %v", err)
				}
				if n.total() != 0 {
					t.Fatalf("cleanup admission performed %d reads before refusing", n.total())
				}
			})
		}
	}

	t.Run("an unauthorized cleanup is still denied first and a v1 cleanup is unaffected", func(t *testing.T) {
		var n effectCounters
		v1 := newDefinition(t, scope, pipelinetest.DocNamed("clean", "{}", "{id: teardown, kind: container, image: "+pipelinetest.Image+", command: [/bin/true], timeoutSeconds: 60, workspace: {access: none}}"))
		v2 := v2Definition(t, v2Documents["source-free cleanup, explicit none"])
		defs := fakeDefs{defs: map[domain.DefinitionID]pipeline.Definition{v1.ID: v1, v2.ID: v2}}
		deny := v2Service(t, defs, &n, allowCleanup{err: ErrDenied})
		if _, err := deny.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: v2.ID}); !errors.Is(err, ErrDenied) {
			t.Fatalf("authorization precedes the guard: %v", err)
		}
		ok := v2Service(t, defs, &n, allowCleanup{})
		plan, err := ok.PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: v1.ID})
		if err != nil || plan.Aggregate.Run.Purpose != PurposeCleanup {
			t.Fatalf("the v1alpha1 cleanup path must be unchanged: %v", err)
		}
	})
}

// Test_unit_pipeline_v2_guard_unchanged pins that the guard still admits exactly
// the current format: registering or reading v1alpha2 never widens it, so the
// new model reaches execution only through a later, explicit change that brings
// the complete run-resource capability checks. PX-21.
func Test_unit_pipeline_v2_guard_unchanged(t *testing.T) {
	if err := checkDefinitionVersion(pipelinev1.APIVersion); err != nil {
		t.Fatalf("the current version must be admitted: %v", err)
	}
	err := checkDefinitionVersion(pipelinev1alpha2.APIVersion)
	if !isGuardRefusal(err) {
		t.Fatalf("v1alpha2 must be refused by the old executor, got %v", err)
	}
	if strings.Contains(err.Error(), "v1alpha2") {
		t.Fatalf("the refusal must not echo the version: %v", err)
	}
	// The same refusal holds for every stored definition of the new format, whatever its content.
	for name, yaml := range v2Documents {
		def := v2Definition(t, yaml)
		doc, err := def.Content.Document()
		if err != nil {
			t.Fatal(err)
		}
		if err := checkDefinitionVersion(doc.APIVersion); !isGuardRefusal(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Test_unit_pipeline_v2_step_codec_round_trip replaces T-110's deliberate
// "step codec is v1 only" regression in the same change that teaches the codec
// the kodama/v1alpha2 step fields (T-112): the frozen step of a run_shared run
// round-trips exactly, egress profiles included, and a kodama/v1alpha1 step is
// stored byte for byte as before (no egress key), so historical rows and
// hashes are unchanged. New-format admission stays closed unless the
// installation composes the run_shared ports (see the shared admission tests).
func Test_unit_pipeline_v2_step_codec_round_trip(t *testing.T) {
	step := pipeline.Step{ID: "t", Kind: pipeline.KindContainer, Image: v2Image, Command: []string{"/bin/true"}, TimeoutSeconds: 60,
		Workspace: &pipeline.Workspace{Access: pipeline.AccessRead}, Egress: []string{"registry", "preview"}}
	raw, err := EncodeStep(step)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"egress":["registry","preview"]`) {
		t.Fatalf("the step codec dropped the v1alpha2 egress profiles: %s", raw)
	}
	back, err := DecodeStep(raw)
	if err != nil || !reflect.DeepEqual(back, step) {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	// Every v1alpha2 step of the documents above survives the stored form.
	for name, yaml := range v2Documents {
		doc, err := v2Definition(t, yaml).Content.Document()
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range doc.Spec.Steps {
			b, err := EncodeStep(st)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeStep(b)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := EncodeStep(got)
			if string(again) != string(b) {
				t.Errorf("%s/%s: the stored step is not stable: %s != %s", name, st.ID, again, b)
			}
		}
	}
	// A v1alpha1 step is stored exactly as before.
	v1 := pipeline.Step{ID: "b", Kind: pipeline.KindContainer, Image: v2Image, Command: []string{"/bin/true"}, TimeoutSeconds: 60}
	v1raw, _ := EncodeStep(v1)
	if strings.Contains(string(v1raw), "egress") {
		t.Fatalf("a v1alpha1 step must not gain an egress key: %s", v1raw)
	}
}
