package pipeline_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

const v3Document = `apiVersion: kodama/v1alpha3
kind: Pipeline
metadata: {name: plugin-flow}
spec:
  timeoutSeconds: 1200
  steps:
    - id: build
      kind: container
      image: registry.example.invalid/builder@sha256:1111111111111111111111111111111111111111111111111111111111111111
      command: [/bin/sh, -ec]
      args: ["true"]
      timeoutSeconds: 600
      outputs:
        bundle: {type: artifact, path: /kodama/outputs/worker.tar.gz}
    - id: deploy
      kind: container
      needs: [build]
      image: registry.example.invalid/deploy@sha256:2222222222222222222222222222222222222222222222222222222222222222
      command: [/usr/local/bin/deploy]
      args: ["apply"]
      timeoutSeconds: 300
      state:
        release:
          namespace: deploy
          prefix: website/stage/
          access: read-write
      artifactFiles:
        bundle: "${{ steps.build.outputs.bundle }}"
`

func newEnvV3(t testing.TB) *env {
	t.Helper()
	e := newEnv(t)
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: e.caps, Profiles: e.profiles, References: e.refs, EnableV1Alpha3: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.v = v
	return e
}

func Test_v1alpha3_state_and_artifact_bindings(t *testing.T) {
	e := newEnvV3(t)
	v := e.mustValidate(t, v3Document)
	doc, err := v.Document()
	if err != nil {
		t.Fatal(err)
	}
	if !doc.IsV1Alpha3() || !doc.IsShared() {
		t.Fatalf("format: %s", doc.APIVersion)
	}
	if got := doc.Spec.Steps[1].State["release"].Access; got != pipeline.StateReadWrite {
		t.Fatalf("state access = %q", got)
	}
	if got := doc.Spec.Steps[1].ArtifactFiles["bundle"]; got != "${{ steps.build.outputs.bundle }}" {
		t.Fatalf("artifact file = %q", got)
	}
	if e.v.FormatEnabled("kodama/v1alpha3") != true || e.v.FormatEnabled("kodama/v1alpha2") {
		t.Fatal("format capability is not independent")
	}
}

func Test_v1alpha3_rejects_environment_and_bad_bindings(t *testing.T) {
	e := newEnvV3(t)
	cases := []string{
		v3Document + "      environment: preview\n",
		v3Document + "      state:\n        release: {namespace: Deploy, prefix: ../x, access: read}\n",
	}
	for _, src := range cases {
		if _, err := e.validate(src); err == nil {
			t.Fatalf("accepted invalid document:\n%s", src)
		}
	}
}
