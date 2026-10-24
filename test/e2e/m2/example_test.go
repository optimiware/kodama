package m2_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// TestM2ExampleEnvArtifactReference documents, as an executable fact, why the
// ERP preview is a second run: the example (docs/examples/workflows/
// erp-gap-to-preview.md) passes the build's `ociArtifact` output into the
// preview step's `env`, and the pipeline validator refuses that registration
// (type_mismatch), so an image identity reaches a deployment step only as a run
// input (the promotion form the delivery qualification uses).
func TestM2ExampleEnvArtifactReference(t *testing.T) {
	e := kit.New(t, kit.Options{NoProfile: true})
	build := kit.ContainerStep("build", `true`, map[string]any{"image": map[string]any{"type": "ociArtifact"}})
	use := kit.ContainerStep("preview", `true`, nil, "build")
	use["env"] = map[string]string{"DEPLOY_IMAGE_ARTIFACT_ID": "${{ steps.build.outputs.image }}"}
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "example-form"},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{build, use}},
	}
	code, body := e.Do("engineer", "POST", e.ProjectPath()+"/pipeline-definitions", doc, nil)
	if code != http.StatusUnprocessableEntity || kit.ErrorCode(body) != "invalid_definition" || !strings.Contains(string(body), "type_mismatch") || !strings.Contains(string(body), "$.spec.steps[1].env.DEPLOY_IMAGE_ARTIFACT_ID") {
		t.Fatalf("the example's env reference of an ociArtifact output: %d %s", code, body)
	}
	// The promotion form registers: the identity is a declared string input.
	use["env"] = map[string]string{"DEPLOY_IMAGE_ARTIFACT_ID": "${{ inputs.image }}"}
	doc["spec"] = map[string]any{"inputs": map[string]any{"image": map[string]any{"type": "string", "required": true}}, "steps": []any{use}}
	delete(use, "needs")
	if code, body := e.Do("engineer", "POST", e.ProjectPath()+"/pipeline-definitions", doc, nil); code != http.StatusCreated {
		t.Fatalf("the promotion form: %d %s", code, body)
	}
}

// TestM2ShippedToolsImageOnPodman records a Podman runtime gap the M2 fixture
// works around (docs/acceptance/m2.md, finding 8): the Podman runtime mounts
// the attempt's bootstrap material as host-owned 0600 files and runs the
// image's own user, so the shipped deployment-tools image (uid 65532) cannot
// read its admission credential or CA and its step fails before any command
// runs. The control is the same image with `USER 0:0` and nothing else changed,
// which runs the same step. When the runtime stops needing a root image this
// test fails by design: switch the fixture to the shipped image and delete it.
func TestM2ShippedToolsImageOnPodman(t *testing.T) {
	e := kit.New(t, kit.Options{Delivery: true, NoProfile: true})
	run := func(name string, img kit.Image) client.Run {
		step := kit.ContainerStep("work", `printf '{"outcome":"completed","outputs":{"ran":"yes"}}' > "$KODAMA_RESULTS_FILE"`, map[string]any{"ran": map[string]any{"type": "string"}})
		step["image"] = img.Ref()
		step["timeoutSeconds"] = 60
		def := e.Register(name, map[string]any{}, step)
		r := e.StartRun(name, e.Item(name, "# "+name), def.Id, nil)
		return e.World.WaitRun(r.Id, 3*time.Minute)
	}
	if f := run("control-root", e.Tools); f.Status != client.RunStatusSucceeded {
		t.Fatalf("the control (the shipped image run as root) is %s (%s)", f.Status, f.Reason)
	}
	f := run("shipped-user", kit.ShippedToolsImage(t))
	if f.Status != client.RunStatusFailed {
		t.Fatalf("the shipped non-root tools image is %s: the Podman runtime now supports it; use it in the fixture and drop this test", f.Status)
	}
	if st := e.StepOf(f.Id, "work"); st.Status != client.StepStatusFailed || string(st.Reason) != "workload_failed" || kit.Output(st, "ran") != "" {
		t.Fatalf("step %+v", st)
	}
}
