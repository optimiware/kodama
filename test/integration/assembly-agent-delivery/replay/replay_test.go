package replay_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

var (
	registerPath = regexp.MustCompile(`^/runtime/v1/attempts/[^/]+/deployments$`)
	outcomePath  = regexp.MustCompile(`^/runtime/v1/attempts/[^/]+/deployments/[^/]+/outcome$`)
	cleanupPath  = regexp.MustCompile(`^/runtime/v1/attempts/[^/]+/deployments/[^/]+/cleanup-result$`)
)

// scenario is one assembled core with a preview environment over its own
// namespace of the scope's kind cluster and a candidate a human committed
// (the agent has its own scenarios: this suite is about delivery).
type scenario struct {
	t    *testing.T
	e    *kit.Env
	p    *kit.ERP
	item client.WorkItem
	cand kit.Lite
}

func newScenario(t *testing.T, label string, ttl time.Duration, o kit.Options) *scenario {
	t.Helper()
	o.Delivery, o.NoProfile = true, false
	e := kit.New(t, o)
	p := e.NewERP(t, kit.ERPSetup{Label: label, TTL: ttl})
	item := e.Item("Delivery "+label, "# delivery "+label)
	return &scenario{t: t, e: e, p: p, item: item, cand: p.HumanCandidate("cand-"+label, item)}
}

// deployments lists the project's deployments.
func (s *scenario) deployments() []client.Deployment { return s.e.Deployments() }

// preview runs the preview run to its end.
func (s *scenario) preview(key string) (client.Run, client.Run) {
	s.t.Helper()
	run := s.p.PreviewLite(key, s.item, s.cand)
	return run, s.e.World.WaitRun(run.Id, 6*time.Minute)
}

// requireOneSettled asserts the end state of one successful preview: one
// deployment record, succeeded and ready, whose objects the cluster holds.
func (s *scenario) requireOneSettled(run client.Run) client.Deployment {
	s.t.Helper()
	deps := s.deployments()
	if len(deps) != 1 {
		s.t.Fatalf("%d deployments for one logical apply", len(deps))
	}
	d := deps[0]
	if d.Status != "succeeded" || d.OutcomeUnknown || d.RunId != run.Id || len(d.Objects) != 2 {
		s.t.Fatalf("deployment %+v", d)
	}
	if kit.UID(s.p.NS.Object(s.t, "deployment", "inventory")) == "" || kit.UID(s.p.NS.Object(s.t, "configmap", "inventory-config")) == "" {
		s.t.Fatal("the recorded objects are not in the namespace")
	}
	return d
}

// TestDeliveryAssemblyReplay (delivery_assembly_replay) breaks the delivery
// path of the assembled core where its acknowledgments, authority and clock
// meet the cluster, with real Podman steps running the shipped apply/delete
// scripts against a kind namespace: the response of the deployment record's
// registration, of its outcome report and of the cleanup completion is lost
// AFTER the server committed (the front closes the stream), or the request is
// lost BEFORE it; the environment's credential is revoked and the environment
// edited between run admission and dispatch; a preview expires and two replicas
// sweep it. Every case ends with one record, the objects the record names (and
// no others), a result the run accepted once, and nothing replayed twice.
// Covers DQ-08, DQ-09, DQ-10, DQ-15, DQ-A04, DQ-A05, DQ-A06, DQ-A11, PX-06,
// PX-07 and SD-11.
func TestDeliveryAssemblyReplay(t *testing.T) {
	t.Run("a lost registration acknowledgment converges on one record", func(t *testing.T) {
		s := newScenario(t, "ack-register", time.Hour, kit.Options{})
		var dropped, seen atomic.Int64
		s.e.Stack.Front.Faults(func(method, path string) kit.Fault {
			if method == http.MethodPost && registerPath.MatchString(path) {
				seen.Add(1)
				if dropped.Add(1) == 1 {
					return kit.FaultDropResponse
				}
			}
			return kit.FaultNone
		})
		run, f := s.preview("replay-register")
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("preview %s (%s)", f.Status, f.Reason)
		}
		if seen.Load() < 2 {
			t.Fatalf("the registration was posted %d times: the script did not retry after the lost acknowledgment", seen.Load())
		}
		d := s.requireOneSettled(run)
		if attempts := s.e.StepOf(run.Id, "preview").Attempts; len(attempts) != 1 {
			t.Fatalf("%d attempts: the retry was the script's own, not a second attempt", len(attempts))
		}
		if got := kit.Output(s.e.StepOf(run.Id, "preview"), "deployment_id"); got != d.Id {
			t.Fatalf("the accepted result names %q, the record is %s", got, d.Id)
		}
	})

	t.Run("a lost registration request is simply retried", func(t *testing.T) {
		s := newScenario(t, "drop-register", time.Hour, kit.Options{})
		var n atomic.Int64
		s.e.Stack.Front.Faults(func(method, path string) kit.Fault {
			if method == http.MethodPost && registerPath.MatchString(path) && n.Add(1) == 1 {
				return kit.FaultDropRequest
			}
			return kit.FaultNone
		})
		run, f := s.preview("replay-drop")
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("preview %s (%s)", f.Status, f.Reason)
		}
		s.requireOneSettled(run)
	})

	t.Run("a lost outcome acknowledgment is replayed, not repeated", func(t *testing.T) {
		s := newScenario(t, "ack-outcome", time.Hour, kit.Options{})
		var n atomic.Int64
		s.e.Stack.Front.Faults(func(method, path string) kit.Fault {
			if method == http.MethodPost && outcomePath.MatchString(path) && n.Add(1) == 1 {
				return kit.FaultDropResponse
			}
			return kit.FaultNone
		})
		run, f := s.preview("replay-outcome")
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("preview %s (%s)", f.Status, f.Reason)
		}
		if n.Load() < 2 {
			t.Fatalf("the outcome was posted %d times", n.Load())
		}
		d := s.requireOneSettled(run)
		// The objects were applied once: the object's generation never advanced past its creation.
		if gen := s.p.NS.Admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.metadata.generation}"); gen != "1" {
			t.Fatalf("the deployment was applied again (generation %s)", gen)
		}
		if got := s.e.Dump(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'step' AND step_id = 'preview' AND to_status = 'succeeded'`, run.Id); len(got) != 1 || got[0] != "1" {
			t.Fatalf("the step succeeded %v times", got)
		}
		if d.Cleanup.State != "none" {
			t.Fatalf("cleanup state %s", d.Cleanup.State)
		}
	})

	t.Run("editing the environment after admission changes nothing admitted", func(t *testing.T) {
		s := newScenario(t, "pinned", time.Hour, kit.Options{})
		// A first preview of the environment as it is now: the admitted configuration to compare with.
		firstRun, f := s.preview("replay-pin-1")
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("preview %s (%s)", f.Status, f.Reason)
		}
		first := s.requireOneSettled(firstRun)
		s.p.NS.Admin(t, "delete", "deployment/inventory", "configmap/inventory-config", "--wait=true")

		// Admit the second run while dispatch is held: its environment and credential are pinned at admission.
		s.e.Retune(func(tu *app.Tuning) { tu.DispatchInterval = time.Hour })
		s.e.Restart()
		run := s.p.PreviewLite("replay-pin-2", s.item, s.cand)
		other := s.p.NS.C.NewNamespace(t, t.TempDir(), "other")
		s.e.EditEnvironment(s.p.EnvID, map[string]any{"namespace": other.Name, "config": map[string]string{"REPLICAS": "9"}})
		s.e.Retune(nil)
		s.e.Restart()
		f2 := s.e.World.WaitRun(run.Id, 6*time.Minute)
		if f2.Status != client.RunStatusSucceeded {
			t.Fatalf("the admitted run %s (%s)", f2.Status, f2.Reason)
		}
		var second client.Deployment
		for _, d := range s.deployments() {
			if d.RunId == run.Id {
				second = d
			}
		}
		if second.Id == "" || second.Namespace != s.p.NS.Name || second.ConfigurationDigest != first.ConfigurationDigest {
			t.Fatalf("the edit after admission reached the deployment: namespace %s (admitted %s), digest %s (admitted %s)", second.Namespace, s.p.NS.Name, second.ConfigurationDigest, first.ConfigurationDigest)
		}
		if out, _ := other.AdminTry("get", "deployment", "inventory", "--ignore-not-found", "-o", "name"); out != "" {
			t.Fatalf("an object appeared in the namespace the environment was edited to: %s", out)
		}

	})

	t.Run("revoking the pinned credential stops the step before the cluster", func(t *testing.T) {
		s := newScenario(t, "revoked", time.Hour, kit.Options{})
		// The control: the same environment and credential deploy before the revocation.
		firstRun, f := s.preview("replay-revoke-1")
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("preview %s (%s)", f.Status, f.Reason)
		}
		s.requireOneSettled(firstRun)
		s.p.NS.Admin(t, "delete", "deployment/inventory", "configmap/inventory-config", "--wait=true")
		resolved := func() int {
			return s.e.TenantCount(kit.TenantA, `SELECT count(*) FROM audit WHERE action = 'secret.resolve'`)
		}
		before := resolved()
		if before == 0 {
			t.Fatal("the first deployment resolved no secret: the control proves nothing")
		}

		// Admit the second run with dispatch held; its credential is pinned; revoke that version; let it dispatch.
		s.e.Retune(func(tu *app.Tuning) { tu.DispatchInterval = time.Hour })
		s.e.Restart()
		second := s.p.PreviewLite("replay-revoke-2", s.item, s.cand)
		var secretName string
		for _, n := range s.e.Dump(`SELECT name FROM secrets WHERE kind = 'kubeconfig'`) {
			secretName = n
		}
		s.e.RevokeSecretVersion(secretName, 1)
		s.e.Retune(nil)
		s.e.Restart()
		f2 := s.e.World.WaitRun(second.Id, 6*time.Minute)
		if f2.Status != client.RunStatusFailed {
			t.Fatalf("a run whose pinned credential was revoked is %s", f2.Status)
		}
		st := s.e.StepOf(second.Id, "preview")
		if st.Status != client.StepStatusFailed || len(st.Attempts) == 0 {
			t.Fatalf("preview step %+v", st)
		}
		if got := resolved(); got != before {
			t.Fatalf("a revoked credential was resolved (%d secret.resolve entries, was %d)", got, before)
		}
		for _, d := range s.deployments() {
			if d.RunId == second.Id {
				t.Fatalf("a deployment record exists for the run whose credential was revoked: %+v", d)
			}
		}
		if out, _ := s.p.NS.AdminTry("get", "deployment,configmap", "-o", "name"); strings.Contains(out, "inventory") {
			t.Fatalf("objects reached the cluster after the revocation: %s", out)
		}
	})

	t.Run("an expired preview is cleaned once by two replicas, through a lost completion, keeping its history", func(t *testing.T) {
		s := newScenario(t, "expiry", time.Minute, kit.Options{})
		s.e.StartReplica() // a second process sweeps the same installation
		var lost atomic.Int64
		s.e.Stack.Front.Faults(func(method, path string) kit.Fault {
			if method == http.MethodPost && cleanupPath.MatchString(path) && lost.Add(1) == 1 {
				return kit.FaultDropResponse
			}
			return kit.FaultNone
		})
		run, f := s.preview("replay-expiry")
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("preview %s (%s)", f.Status, f.Reason)
		}
		d := s.requireOneSettled(run)
		if d.ExpiresAt == nil {
			t.Fatal("the preview has no expiry")
		}
		s.p.NS.Admin(t, "create", "configmap", "bystander", "--from-literal=owner=somebody-else")
		created := time.Now()

		kit.Eventually(t, 4*time.Minute, "the expired preview to be cleaned", func() (bool, string) {
			for _, x := range s.deployments() {
				if x.Id == d.Id {
					return x.DeletedAt != nil, fmt.Sprintf("cleanup %s/%v", x.Cleanup.State, x.Cleanup.Reason)
				}
			}
			return false, "the record vanished"
		})
		if time.Since(created) < 0 {
			t.Fatal("unreachable")
		}
		if lost.Load() < 2 {
			t.Fatalf("the cleanup completion was posted %d times: the lost acknowledgment was not retried", lost.Load())
		}
		// One cleanup run for the deployment, however many replicas swept and however the acknowledgment went.
		var n int
		s.e.MustScan(&n, `SELECT count(*) FROM runs WHERE purpose = 'cleanup' AND deployment_id = $1`, d.Id)
		if n != 1 {
			t.Fatalf("%d cleanup runs for one deployment", n)
		}
		var cleanup string
		s.e.MustScan(&cleanup, `SELECT id FROM runs WHERE purpose = 'cleanup' AND deployment_id = $1`, d.Id)
		cf := s.e.World.WaitRun(cleanup, 3*time.Minute)
		if cf.Status != client.RunStatusSucceeded {
			t.Fatalf("cleanup run %s (%s)", cf.Status, cf.Reason)
		}
		cs := s.e.StepOf(cleanup, "cleanup")
		if kit.Output(cs, "deployment_id") != d.Id || kit.Output(cs, "cleanup_state") != "done" || len(cs.Attempts) != 1 {
			t.Fatalf("cleanup step %+v outputs deployment_id=%q state=%q", cs, kit.Output(cs, "deployment_id"), kit.Output(cs, "cleanup_state"))
		}
		// Exactly the recorded objects are gone; what the deployment never owned survives.
		if o := s.p.NS.Object(t, "deployment", "inventory"); o != nil {
			t.Fatal("the recorded deployment survived the cleanup")
		}
		if o := s.p.NS.Object(t, "configmap", "inventory-config"); o != nil {
			t.Fatal("the recorded configmap survived the cleanup")
		}
		if o := s.p.NS.Object(t, "configmap", "bystander"); o == nil {
			t.Fatal("the cleanup deleted an object the deployment never owned")
		}
		// The delivery history is retained and still says what was deployed and inspected.
		got := s.deployments()
		if len(got) != 1 || got[0].Id != d.Id || got[0].Status != "succeeded" || len(got[0].Objects) != 2 || got[0].Image.ArtifactId != d.Image.ArtifactId ||
			got[0].Source.Commit != s.cand.Commit || got[0].ConfigurationDigest != d.ConfigurationDigest || got[0].DeletedAt == nil {
			t.Fatalf("the retained record %+v", got)
		}
		if kit.Output(s.e.StepOf(run.Id, "preview"), "deployment_id") != d.Id {
			t.Fatal("the preview run's accepted outputs changed")
		}
		// Repeatable: more sweeps, and an explicit request, admit nothing new and change nothing.
		time.Sleep(5 * time.Second)
		s.e.MustScan(&n, `SELECT count(*) FROM runs WHERE purpose = 'cleanup' AND deployment_id = $1`, d.Id)
		if n != 1 {
			t.Fatalf("%d cleanup runs after further sweeps", n)
		}
		code, body := s.e.Do("engineer", "POST", s.e.ProjectPath()+"/deployments/"+d.Id+"/cleanup", nil, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, got[0].Version)})
		t.Logf("an explicit cleanup request of a cleaned deployment: %d %s", code, kit.ErrorCode(body))
		s.e.MustScan(&n, `SELECT count(*) FROM runs WHERE purpose = 'cleanup' AND deployment_id = $1`, d.Id)
		if n != 1 {
			t.Fatalf("%d cleanup runs after an explicit request", n)
		}
	})
}
