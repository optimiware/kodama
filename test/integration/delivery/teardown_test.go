package delivery_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

func containerExists(name string) bool {
	return exec.Command("podman", "container", "exists", name).Run() == nil
}

// Test_teardown_removes_only_the_containers_it_started: an unrelated container
// running the very same tools image (another scope, a concurrent invocation, or a
// cached image under another tag) is not this test's resource and survives the
// teardown of the workloads this stack's runtime started.
func Test_teardown_removes_only_the_containers_it_started(t *testing.T) {
	t.Parallel()
	w := newWorld(t, "teardown", worldOptions{})
	ref := w.img.Repository + "@" + string(w.img.Digest)
	foreign := "t067-foreign-" + randHex(4)
	if out, err := exec.Command("podman", "run", "-d", "--name", foreign, "--entrypoint", "sleep", ref, "600").CombinedOutput(); err != nil {
		t.Fatalf("starting the unrelated container: %v: %s", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", "--volumes", foreign).Run() })

	// A workload of this stack that is still running when the teardown runs.
	f := w.flow(stack.FlowOptions{Manifests: w.appManifests(0, "inventory")})
	n := w.S.Dispatch(f)
	agg, err := w.S.ExecStore.Get(context.Background(), f.Scope, f.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	mine := agg.Attempts[n-1].WorkloadName
	if !containerExists(mine) {
		t.Fatalf("the workload %s was not started", mine)
	}
	w.S.RemoveWorkloads()
	if containerExists(mine) {
		t.Fatalf("the workload %s of this stack survived its teardown", mine)
	}
	if !containerExists(foreign) {
		t.Fatalf("the teardown removed %s, an unrelated container of the same image", foreign)
	}
	if out, _ := exec.Command("podman", "inspect", "--format", "{{.State.Running}}", foreign).Output(); strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("the unrelated container is no longer running: %s", out)
	}
}
