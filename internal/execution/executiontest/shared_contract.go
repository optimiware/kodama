package executiontest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// rig drives one run_shared run through the lifecycle the way the control plane
// does: admit, claim (which starts resource holding), reconcile passes, the
// preparation receipt, begin and results.
type rig struct {
	t   *testing.T
	s   SharedSubject
	run *execution.Run
}

var sharedCaller = execution.Caller{PrincipalID: Engineer, RequestID: "req_shared"}

func (r *rig) ctx() context.Context { return context.Background() }

func (r *rig) ref() execution.RunRef { return execution.RunRef{Scope: SharedScope, ID: r.run.ID} }

func (r *rig) admit(yaml string) *execution.Run {
	r.t.Helper()
	def := r.s.DefineShared(yaml)
	run, err := r.s.Svc.Admit(r.ctx(), sharedCaller, execution.AdmitCommand{
		Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID, ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA,
	})
	if err != nil {
		r.t.Fatalf("admit: %v", err)
	}
	r.run = run
	return run
}

func (r *rig) get() *execution.Aggregate {
	r.t.Helper()
	agg, err := r.s.Store.Get(r.ctx(), SharedScope, r.run.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return agg
}

// pass runs one reconciliation pass of the run on svc (the default Service).
func (r *rig) pass() {
	r.t.Helper()
	r.passOn(r.s.Svc)
}

func (r *rig) passOn(svc *execution.Service) {
	r.t.Helper()
	if _, err := svc.ReconcileRun(r.ctx(), r.ref()); err != nil && !errors.Is(err, execution.ErrBusy) {
		r.t.Fatalf("reconcile: %v", err)
	}
}

func (r *rig) passes(n int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		r.pass()
	}
}

// start performs the claim that starts resource holding (no attempt exists yet).
func (r *rig) start() {
	r.t.Helper()
	if in, err := r.s.Svc.ClaimRunnable(r.ctx(), SharedScope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
		r.t.Fatalf("the first claim only starts resource holding, got %v %v", in, err)
	}
}

func (r *rig) res(key string) *execution.RunResource {
	r.t.Helper()
	res := r.get().Resource(key)
	if res == nil {
		r.t.Fatalf("no resource %s", key)
	}
	return res
}

func (r *rig) receipt() (execution.ReceiptAck, error) {
	return r.s.Svc.RecordPreparationReceipt(r.ctx(), SharedScope, r.run.ID, runtimev2.PreparationReceiptRequest{
		Commit: CommitA, TreeID: CommitC, HooksDisabled: true, Submodules: runtimev2.SubmodulesSkipped, CredentialsRemoved: true, Attempts: 1,
	})
}

// up drives a freshly admitted run to an open barrier with AutoReady objects.
func (r *rig) up() {
	r.t.Helper()
	r.s.Resources.AutoReady = true
	r.start()
	r.pass()
	if _, err := r.receipt(); err != nil {
		r.t.Fatalf("receipt: %v", err)
	}
	r.passes(2)
}

func (r *rig) claim() (*execution.AttemptSpec, error) {
	return r.s.Svc.ClaimRunnable(r.ctx(), SharedScope.TenantID)
}

func (r *rig) mustClaim() *execution.AttemptSpec {
	r.t.Helper()
	in, err := r.claim()
	if err != nil {
		r.t.Fatalf("claim: %v", err)
	}
	return in
}

func (r *rig) started(in *execution.AttemptSpec) {
	r.t.Helper()
	r.s.Drive.Start(in)
}

func (r *rig) tick(d time.Duration) { r.s.Advance(d) }

func (r *rig) settledSucceed(in *execution.AttemptSpec, commit string) {
	r.t.Helper()
	if _, err := r.s.Drive.Succeed(in, nil, commit); err != nil {
		r.t.Fatalf("result: %v", err)
	}
}

func hasKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func callIndex(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

// SharedContract is the shared memory/PostgreSQL contract of the run_shared
// resource lifecycle and persistence: admission pins and refusals, intent-first
// start with atomic capacity, ordered provisioning with the receipt-bound
// workspace barrier, holder admission separate from the logical writer, the
// command-begin journal and retry safety, loss, unknown, deadline, cancellation
// and cleanup with restart, cancel/create races, collisions, duplicate
// wakeups and orphan cleanup. Every store adapter passes it: the in-memory
// store on every unit run and PostgreSQL in the explicit `test` environment.
func SharedContract(t *testing.T, open func(testing.TB) SharedSubject) {
	t.Helper()
	svcOnly := SharedSpecDoc(false, SharedStep("job"))
	withDB := SharedSpecDoc(true, SharedStep("job", "env: {PGHOST: '${{ services.db.host }}', PGPORT: '${{ services.db.ports.sql }}'}"))
	twoStep := SharedSpecDoc(false, SharedStep("a"), SharedStep("b", "needs: [a]"))
	newRig := func(t *testing.T) *rig {
		t.Helper()
		return &rig{t: t, s: open(t)}
	}

	t.Run("admission is closed unless the ports are composed explicitly", func(t *testing.T) {
		r := newRig(t)
		def := r.s.DefineShared(svcOnly)
		_, err := r.s.Closed().Admit(context.Background(), sharedCaller, execution.AdmitCommand{
			Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID, ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA,
		})
		if !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("an unconfigured installation must refuse the new format, got %v", err)
		}
		if runs, _ := r.s.Store.List(context.Background(), SharedScope, execution.ListQuery{}); len(runs) != 0 {
			t.Fatalf("a refused admission created %d runs", len(runs))
		}
	})

	t.Run("admission refuses a backend missing any required capability, service-free runs included", func(t *testing.T) {
		cases := []struct {
			name string
			doc  string
			drop execution.ResourceCapability
			mut  func(r *rig)
		}{
			{name: "run_network_isolation (service-free)", doc: svcOnly, drop: execution.CapRunNetworkIsolation},
			{name: "egress_default_deny (service-free)", doc: svcOnly, drop: execution.CapEgressDefaultDeny},
			{name: "workspace_fence", doc: svcOnly, drop: execution.CapWorkspaceFence},
			{name: "resource_recovery", doc: svcOnly, drop: execution.CapResourceRecovery},
			{name: "run_workspace", doc: svcOnly, drop: execution.CapRunWorkspace},
			{name: "workspace_preparation (source-backed)", doc: svcOnly, drop: execution.CapWorkspacePreparation},
			{name: "run_services", doc: withDB, drop: execution.CapRunServices},
			{name: "service_isolation", doc: withDB, drop: execution.CapServiceIsolation},
			{name: "the run_resources marker", doc: svcOnly, mut: func(r *rig) {
				r.s.Manual.SetCapabilities(execution.Capabilities{Backend: SharedBackend, Guarantees: execution.AllGuarantees})
			}},
			{name: "the same backend identity", doc: svcOnly, mut: func(r *rig) {
				r.s.Manual.SetCapabilities(execution.Capabilities{Backend: "another", Guarantees: execution.AllGuarantees.WithRunResources()})
			}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t)
				if c.mut != nil {
					c.mut(r)
				} else {
					r.s.Resources.SetCapabilities(FullResourceSet &^ execution.ResourceCapabilitySet(c.drop))
				}
				def := r.s.DefineShared(c.doc)
				_, err := r.s.Svc.Admit(context.Background(), sharedCaller, execution.AdmitCommand{
					Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID, ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA,
				})
				if !errors.Is(err, execution.ErrAdmissionRefused) {
					t.Fatalf("a backend lacking %s must be refused, got %v", c.name, err)
				}
				if runs, _ := r.s.Store.List(context.Background(), SharedScope, execution.ListQuery{}); len(runs) != 0 {
					t.Fatal("a refusal created a run")
				}
			})
		}
	})

	t.Run("admission refuses a closed or unavailable policy and any ceiling violation", func(t *testing.T) {
		for name, mut := range map[string]func(r *rig){
			"policy closed": func(r *rig) {
				p := TestPolicy()
				p.Admission.RunShared = execution.PolicyClosed
				r.s.Policies.Set(p)
			},
			"policy unavailable": func(r *rig) { r.s.Policies.Fail(errors.New("down")) },
			"too many services": func(r *rig) {
				p := TestPolicy()
				p.Limits.MaxServices = 1
				r.s.Policies.Set(p)
			},
			"service cpu above the ceiling": func(r *rig) {
				p := TestPolicy()
				p.Limits.MaxServiceCPUMillis = 50
				r.s.Policies.Set(p)
			},
			"service storage above the ceiling": func(r *rig) {
				p := TestPolicy()
				p.Limits.MaxServiceStorageMiB = 64
				r.s.Policies.Set(p)
			},
			"run deadline above the ceiling": func(r *rig) {
				p := TestPolicy()
				p.Limits.MaxRunSeconds = 120
				r.s.Policies.Set(p)
			},
		} {
			t.Run(name, func(t *testing.T) {
				r := newRig(t)
				mut(r)
				if name == "too many services" {
					r.admitTwoServices()
					return
				}
				def := r.s.DefineShared(withDB)
				_, err := r.s.Svc.Admit(context.Background(), sharedCaller, execution.AdmitCommand{
					Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID, ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA,
				})
				if !errors.Is(err, execution.ErrAdmissionRefused) {
					t.Fatalf("got %v", err)
				}
			})
		}
	})

	t.Run("admission pins the model, policy body, endpoints, access and order and copies them", func(t *testing.T) {
		r := newRig(t)
		run := r.admit(withDB)
		agg := r.get()
		sh := agg.Run.Shared
		if sh == nil || run.Status != execution.RunQueued || len(agg.Resources) != 0 || r.s.RunReservations(SharedScope.TenantID) != 0 {
			t.Fatalf("admission is pins only: %+v resources=%d", sh, len(agg.Resources))
		}
		pol := TestPolicy()
		want, _ := pol.Pin()
		host := execution.EndpointHost(SharedScope, run.ID, "db")
		if sh.RunModel != "run_shared" || sh.ContextVersion != 2 || sh.Policy != want || sh.Backend != SharedBackend ||
			sh.Requires&execution.RunResourcesMarker == 0 || sh.Endpoints["db"].Host != host || sh.Endpoints["db"].Ports["sql"] != 5432 ||
			sh.Access["job"] != execution.AccessRead || len(sh.Order) != 1 || sh.Order[0] != "job" || sh.RunSeconds != 3600 ||
			sh.WorkspaceMiB != pol.Limits.DefaultRunWorkspaceMiB || sh.Services["db"].Env == nil {
			t.Fatalf("pins: %+v", sh)
		}
		if got := agg.Run.Snapshot.Runtime; got == nil || got.ResourceRequires != sh.ResourceRequires || !got.ResourceRequires.Has(execution.CapRunServices) ||
			!got.ResourceRequires.Has(execution.CapRunNetworkIsolation) || !got.ResourceRequires.Has(execution.CapEgressDefaultDeny) {
			t.Fatalf("the binding does not pin the resource capabilities: %+v", got)
		}
		if got := agg.Steps[0].Spec.Env["PGHOST"]; got.Scalar == nil || !strings.Contains(got.Scalar.Str, "services.db.host") {
			t.Fatalf("the frozen step lost its endpoint expression: %+v", got)
		}
		// Reads share nothing with the store, and a later policy never moves a pin.
		agg.Run.Shared.Order[0] = "tampered"
		agg.Run.Shared.Endpoints["db"] = execution.EndpointPin{}
		p2 := TestPolicy()
		p2.Limits.StopGraceSeconds = 31
		r.s.Policies.Set(p2)
		again := r.get().Run.Shared
		if again.Order[0] != "job" || again.Endpoints["db"].Host != host || again.Policy != want {
			t.Fatal("a stored pin was changed through a copy or by a policy change")
		}
		if err := again.Verify(); err != nil {
			t.Fatalf("the retained body no longer verifies: %v", err)
		}
	})

	t.Run("start reserves capacity atomically and records intent before any runtime call", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.start()
		agg := r.get()
		if agg.Run.Status != execution.RunRunning || agg.Run.StartedAt == nil || agg.Run.DeadlineAt == nil ||
			!agg.Run.DeadlineAt.Equal(agg.Run.StartedAt.Add(3600*time.Second)) {
			t.Fatalf("start: %+v", agg.Run)
		}
		var keys []string
		for _, res := range agg.Resources {
			keys = append(keys, res.Key)
			fp, _ := res.Spec.Fingerprint()
			if res.Fingerprint == "" || res.Fingerprint != fp || res.UID != "" || res.EnsureRequestedAt != nil || res.Name != execution.ResourceName(SharedScope, agg.Run.ID, res.Kind, res.Key) {
				t.Fatalf("intent %+v", res)
			}
		}
		if strings.Join(keys, ",") != "network,workspace,preparation,service.db" {
			t.Fatalf("provisioning order %v", keys)
		}
		if calls := r.s.Resources.Calls(); len(calls) != 0 {
			t.Fatalf("the start made runtime calls: %v", calls)
		}
		if r.s.RunReservations(SharedScope.TenantID) != 1 {
			t.Fatal("the run reservation was not recorded with the intents")
		}
		r.start()
		if r.s.RunReservations(SharedScope.TenantID) != 1 || len(r.get().Resources) != 4 {
			t.Fatal("a repeated start duplicated state")
		}
	})

	t.Run("insufficient capacity leaves the run queued with no partial reservation", func(t *testing.T) {
		r := newRig(t)
		lim := SharedLimit()
		lim.SharedProject.StorageMiB = 2048 + 128 // the first run's scratch (2048) and service storage fit; a second does not
		r.s.Limits.Set(lim)
		first := r.admit(withDB)
		r.start()
		second := r.admit(withDB)
		for i := 0; i < 2; i++ {
			if _, err := r.s.Svc.ClaimRunnable(context.Background(), SharedScope.TenantID); err != nil && !errors.Is(err, execution.ErrNothingRunnable) && !errors.Is(err, execution.ErrQuotaExceeded) {
				t.Fatalf("claim: %v", err)
			}
		}
		agg, err := r.s.Store.Get(context.Background(), SharedScope, second.ID)
		if err != nil || agg.Run.Status != execution.RunQueued || len(agg.Resources) != 0 || agg.Run.DeadlineAt != nil {
			t.Fatalf("the second run must stay queued untouched: %+v %v", agg.Run, err)
		}
		if r.s.RunReservations(SharedScope.TenantID) != 1 {
			t.Fatalf("held reservations %d, want exactly the first run's", r.s.RunReservations(SharedScope.TenantID))
		}
		// Cancelling the first and finishing its cleanup releases capacity: the second starts.
		r.run = first
		if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, first.ID); err != nil {
			t.Fatal(err)
		}
		r.passes(4)
		if st := r.get().Run.Status; st != execution.RunCancelled {
			t.Fatalf("the first run is %s", st)
		}
		if _, err := r.s.Svc.ClaimRunnable(context.Background(), SharedScope.TenantID); err != nil && !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatal(err)
		}
		agg, _ = r.s.Store.Get(context.Background(), SharedScope, second.ID)
		if agg.Run.Status != execution.RunRunning || r.s.RunReservations(SharedScope.TenantID) != 1 {
			t.Fatalf("the second run did not start after release: %s held=%d", agg.Run.Status, r.s.RunReservations(SharedScope.TenantID))
		}
	})

	t.Run("provisioning is ordered and the barrier needs the receipt, the stopped preparation and ready services", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.start()
		r.pass()
		if calls := r.s.Resources.Calls(); strings.Join(calls, ",") != "ensure:network" {
			t.Fatalf("only the network may be created first, got %v", calls)
		}
		r.pass() // network still pending: nothing else is created
		if len(r.s.Resources.Calls()) != 2 || r.s.Resources.Exists("workspace") {
			t.Fatalf("calls %v", r.s.Resources.Calls())
		}
		r.s.Resources.Ready("network")
		r.pass()
		if !r.s.Resources.Exists("workspace") || r.s.Resources.Exists("preparation") || r.s.Resources.Exists("service.db") {
			t.Fatalf("the workspace comes before preparation and services: %v", r.s.Resources.LiveKeys())
		}
		r.s.Resources.Ready("workspace")
		r.pass()
		if !r.s.Resources.Exists("preparation") || !r.s.Resources.Exists("service.db") {
			t.Fatalf("preparation and services start together: %v", r.s.Resources.LiveKeys())
		}
		r.s.Resources.ReadyAll()
		r.passes(2)
		if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("no claim without the receipt: %v", err)
		}
		// The receipt is bound to the workspace UID; the workspace is ready only after
		// the preparation is verified stopped.
		ack, err := r.receipt()
		if err != nil {
			t.Fatal(err)
		}
		obj := execution.PreparationReceiptObject{
			Request: runtimev2.PreparationReceiptRequest{Commit: CommitA, TreeID: CommitC, HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 1},
			RunID:   r.run.ID, ResourceKey: execution.KeyPreparation, WorkspaceUID: r.s.Resources.UID("workspace"),
		}
		if want, _ := obj.Digest(); ack.Digest != want || ack.Replayed {
			t.Fatalf("receipt digest %s, want %s", ack.Digest, want)
		}
		if agg := r.get(); agg.WorkspaceReady() {
			t.Fatal("the workspace is ready before the preparation was verified stopped")
		}
		in := (*execution.AttemptSpec)(nil)
		if got, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claimed before the preparation stopped: %v %v", got, err)
		}
		r.pass() // stops the preparation (credentials gone)
		if !r.res("preparation").Stopped() {
			t.Fatal("the preparation was not stopped after its receipt")
		}
		r.pass()
		in = r.mustClaim()
		ctx2, err := in.DecodeContextV2()
		if err != nil {
			t.Fatal(err)
		}
		if ctx2.ContextVersion != 2 || ctx2.RunModel != "run_shared" || ctx2.Workspace.Preparation == nil || string(ctx2.Workspace.Preparation.ReceiptDigest) != ack.Digest ||
			ctx2.Workspace.Source == nil || string(ctx2.Workspace.Source.InputCommit) != CommitA || ctx2.Workspace.Source.Branch != nil ||
			len(ctx2.Workspace.Lineage) != 0 || ctx2.Endpoints["db"].Host != execution.EndpointHost(SharedScope, r.run.ID, "db") ||
			ctx2.Command.Env["PGHOST"] != execution.EndpointHost(SharedScope, r.run.ID, "db") || ctx2.Command.Env["PGPORT"] != "5432" ||
			ctx2.Policy.ID != "run-test" || ctx2.Workspace.Access != runtimev1.AccessRead || ctx2.Run.DeadlineAt == "" {
			t.Fatalf("context %+v", ctx2)
		}
		if err := ctx2.Validate(); err != nil {
			t.Fatalf("the context violates the runtime v2 contract: %v", err)
		}
	})

	t.Run("the preparation receipt is bound, replayable and conflicting receipts are refused", func(t *testing.T) {
		r := newRig(t)
		r.admit(svcOnly)
		r.start()
		if _, err := r.receipt(); !errors.Is(err, execution.ErrResourceUnbound) {
			t.Fatalf("before the UIDs are bound: %v", err)
		}
		r.s.Resources.AutoReady = true
		r.pass()
		bad := runtimev2.PreparationReceiptRequest{Commit: CommitB, TreeID: CommitC, HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 1}
		if _, err := r.s.Svc.RecordPreparationReceipt(context.Background(), SharedScope, r.run.ID, bad); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("a commit other than the pinned input must be refused: %v", err)
		}
		bad = runtimev2.PreparationReceiptRequest{Commit: CommitA, TreeID: CommitC, HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 3}
		if _, err := r.s.Svc.RecordPreparationReceipt(context.Background(), SharedScope, r.run.ID, bad); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("more tries than the policy allows must be refused: %v", err)
		}
		first, err := r.receipt()
		if err != nil {
			t.Fatal(err)
		}
		again, err := r.receipt()
		if err != nil || !again.Replayed || again.Digest != first.Digest {
			t.Fatalf("an identical repeat returns the original: %+v %v", again, err)
		}
		other := runtimev2.PreparationReceiptRequest{Commit: CommitA, TreeID: CommitB, HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 1}
		if _, err := r.s.Svc.RecordPreparationReceipt(context.Background(), SharedScope, r.run.ID, other); !errors.Is(err, execution.ErrReceiptConflict) {
			t.Fatalf("a different receipt: %v", err)
		}
		// A tampered binding: the stored digest no longer matches another volume UID.
		agg := r.get()
		if !agg.ReceiptBound() {
			t.Fatal("the recorded receipt must verify against the bound workspace UID")
		}
		// After the stop intent the preparation has no authority to record a receipt.
		r2 := newRig(t)
		r2.admit(svcOnly)
		r2.s.Resources.AutoReady = true
		r2.start()
		r2.pass()
		if _, err := r2.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r2.run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r2.receipt(); !errors.Is(err, execution.ErrResourceRevoked) {
			t.Fatalf("a stopping preparation: %v", err)
		}
	})

	t.Run("one holder at a time, in order, handed off only after the verified stop and the released volume", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		r.up()
		a := r.mustClaim()
		if a.StepID != "a" {
			t.Fatalf("the order starts with a, got %s", a.StepID)
		}
		agg := r.get()
		if h := agg.ActiveHolder(); h == nil || h.AttemptID != a.AttemptID {
			t.Fatalf("holder %+v", h)
		}
		if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("b must wait for the holder and for a: %v", err)
		}
		r.started(a)
		r.settledSucceed(a, "")
		// a settled, but its holder is released only after the verified stop and a
		// release observation of the workspace.
		r.s.Resources.Hold(true)
		r.passes(2)
		if agg := r.get(); agg.ActiveHolder() == nil {
			t.Fatal("the holder was released while the volume still had a user")
		}
		if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("handoff before the volume was released: %v", err)
		}
		r.s.Resources.Hold(false)
		r.tick(2 * time.Second)
		r.passes(2)
		if agg := r.get(); agg.ActiveHolder() != nil {
			t.Fatal("the holder stayed after the volume was observed released")
		}
		b := r.mustClaim()
		if b.StepID != "b" {
			t.Fatalf("got %s", b.StepID)
		}
		bctx, err := b.DecodeContextV2()
		if err != nil || len(bctx.Workspace.Lineage) != 0 {
			t.Fatalf("a never began a command, so the scratch is still the prepared tree: %+v %v", bctx.Workspace.Lineage, err)
		}
		holders := r.get().Holders
		if len(holders) != 2 || holders[0].ReleasedAt == nil || holders[1].ReleasedAt != nil {
			t.Fatalf("holders %+v", holders)
		}
	})

	t.Run("a launch carries only the pinned step role, the bound workspace and the network model", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		if _, err := execution.SharedLaunchOf(r.get(), "a"); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a launch before the workspace is bound: %v", err)
		}
		r.up()
		a := r.mustClaim()
		agg := r.get()
		launch, err := execution.SharedLaunchOf(agg, a.StepID)
		if err != nil {
			t.Fatal(err)
		}
		if err := launch.Validate(*a); err != nil {
			t.Fatal(err)
		}
		ws := agg.Resource(execution.KeyWorkspace)
		pol := agg.Run.Shared.PolicyBody
		digest, _ := pol.RoleDigest(execution.RoleStep)
		if launch.Workspace != ws.Handle() || launch.Workspace.UID == "" || launch.RoleDigest != digest ||
			strings.Join(launch.Role.CapabilitiesAdd, ",") != "SETUID,SETGID" || launch.Network != pol.Network || launch.Policy != agg.Run.Shared.Policy {
			t.Fatalf("launch %+v", launch)
		}
		other := *a
		other.RunID = "run_other"
		if launch.Validate(other) == nil {
			t.Fatal("a launch was accepted for another run's attempt")
		}
		// A later policy never changes the launch of a pinned run.
		p := TenantPolicy()
		p.Roles[execution.RoleStep] = execution.RoleShape{User: "nonroot", UserNamespace: "pod", CapabilitiesAdd: []string{}, Seccomp: "RuntimeDefault", Devices: []string{}}
		r.s.Policies.Set(p)
		again, err := execution.SharedLaunchOf(r.get(), a.StepID)
		if err != nil || again.RoleDigest != digest || again.Network != pol.Network {
			t.Fatalf("the launch followed the current policy: %+v %v", again, err)
		}
	})

	t.Run("dispatch and redrive hand the runtime the pinned launch", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		r.up()
		lastStart := func() execution.AttemptSpec {
			starts := r.s.Starts()
			if len(starts) == 0 {
				t.Fatal("the runtime received no start")
			}
			return starts[len(starts)-1]
		}
		check := func(spec execution.AttemptSpec, what string) {
			t.Helper()
			ws := r.get().Resource(execution.KeyWorkspace)
			if spec.Shared == nil || spec.Shared.Workspace != ws.Handle() || spec.Shared.Validate(spec) != nil {
				t.Fatalf("%s: the runtime did not receive the pinned launch: %+v", what, spec.Shared)
			}
		}
		// First dispatch.
		first := newRig(t)
		first.admit(twoStep)
		first.up()
		if ok, err := first.s.Svc.DispatchNext(first.ctx(), SharedScope.TenantID); !ok || err != nil {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		starts := first.s.Starts()
		if len(starts) != 1 || starts[0].Redrive || starts[0].StepID != "a" || starts[0].Shared == nil ||
			starts[0].Shared.Workspace != first.get().Resource(execution.KeyWorkspace).Handle() || starts[0].Shared.Validate(starts[0]) != nil {
			t.Fatalf("first dispatch: %+v", starts)
		}
		// A claim whose start never happened is redriven after its lease lapses.
		claimed := r.mustClaim()
		before := len(r.s.Starts())
		r.tick(3 * time.Second)
		r.passes(2)
		if len(r.s.Starts()) == before {
			t.Fatal("the lapsed claim was not redriven")
		}
		redriven := lastStart()
		if redriven.AttemptID != claimed.AttemptID || !redriven.Redrive {
			t.Fatalf("redrive %+v", redriven)
		}
		check(redriven, "redrive")
		if claimed.Shared != nil {
			t.Fatal("a claim is not a launch: Shared is set only immediately before Start")
		}
	})

	t.Run("the command-begin journal is idempotent, conflicts on a different body and refuses a cancelled attempt", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		r.up()
		a := r.mustClaim()
		r.started(a)
		body := runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: CommitA, TrackedClean: true, UntrackedPresent: false, IgnoredPresent: true}}
		ack, err := r.s.Svc.RecordBegin(context.Background(), SharedScope, a.AttemptID, body)
		if err != nil || ack.Replayed || ack.WorkspaceState != runtimev2.StateClean || !ack.IgnoredPresent {
			t.Fatalf("begin: %+v %v", ack, err)
		}
		again, err := r.s.Svc.RecordBegin(context.Background(), SharedScope, a.AttemptID, body)
		if err != nil || !again.Replayed || !again.BeganAt.Equal(ack.BeganAt) {
			t.Fatalf("replay: %+v %v", again, err)
		}
		other := runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: CommitA, TrackedClean: false, UntrackedPresent: false, IgnoredPresent: true}}
		if _, err := r.s.Svc.RecordBegin(context.Background(), SharedScope, a.AttemptID, other); !errors.Is(err, execution.ErrBeginConflict) {
			t.Fatalf("a different body: %v", err)
		}
		if _, err := r.s.Svc.RecordBegin(context.Background(), SharedScope, a.AttemptID, runtimev2.BeginRequest{}); !errors.Is(err, execution.ErrInvalid) && !errors.Is(err, execution.ErrBeginConflict) {
			t.Fatalf("a null workspace for a source-backed attempt: %v", err)
		}
		// A cancelled run refuses begin of a later attempt: command authorization
		// linearizes against cancellation.
		r.settledSucceed(a, "")
		r.tick(2 * time.Second)
		r.passes(2)
		b := r.mustClaim()
		if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Svc.RecordBegin(context.Background(), SharedScope, b.AttemptID, body); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("begin after cancellation: %v", err)
		}
		// The recorded begin of a shows in the lineage of what would follow.
		agg := r.get()
		if !agg.Began(a.AttemptID) || agg.Began(b.AttemptID) {
			t.Fatal("begin records are wrong")
		}
	})

	t.Run("automatic retry is allowed only before any begin and never after one", func(t *testing.T) {
		retry := SharedSpecDoc(false, SharedStep("job", "retry: {maxAttempts: 3, backoffSeconds: 1, on: [infrastructure]}"))
		t.Run("a provably pre-command failure retries on the same scratch and services", func(t *testing.T) {
			r := newRig(t)
			r.admit(retry)
			r.up()
			ensures := r.s.Resources.Count("ensure:workspace")
			a := r.mustClaim()
			if err := r.s.Drive.Observe(a, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}); err != nil {
				t.Fatal(err)
			}
			if err := r.s.Drive.Terminate(a, execution.LaunchNotStarted); err != nil {
				t.Fatal(err)
			}
			r.tick(2 * time.Second)
			r.passes(3)
			agg := r.get()
			if agg.Run.FailureIntentAt != nil || agg.Steps[0].Status != execution.StepPending {
				t.Fatalf("a pre-command infrastructure failure must be retried: run=%+v step=%s", agg.Run.FailureIntentAt, agg.Steps[0].Status)
			}
			b := r.mustClaim()
			if b.AttemptID == a.AttemptID || r.s.Resources.Count("ensure:workspace") != ensures {
				t.Fatal("the retry must be a new attempt on the same resources")
			}
		})
		t.Run("a recorded begin makes the same failure final", func(t *testing.T) {
			r := newRig(t)
			r.admit(retry)
			r.up()
			a := r.mustClaim()
			r.started(a)
			if _, err := r.s.Svc.RecordBegin(context.Background(), SharedScope, a.AttemptID, runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: CommitA, TrackedClean: true}}); err != nil {
				t.Fatal(err)
			}
			if err := r.s.Drive.Observe(a, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}); err != nil {
				t.Fatal(err)
			}
			if err := r.s.Drive.Terminate(a, execution.LaunchNotStarted); err != nil {
				t.Fatal(err)
			}
			r.tick(2 * time.Second)
			r.passes(2)
			agg := r.get()
			if agg.Run.FailureIntentAt == nil || agg.Steps[0].Status != execution.StepFailed {
				t.Fatalf("an attempt that began must never be retried automatically: %+v", agg.Steps[0])
			}
		})
	})

	t.Run("service loss fails the run, stops the resources and releases capacity only after verified removal", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		a := r.mustClaim()
		r.started(a)
		r.s.Resources.Vanish("service.db")
		r.pass()
		agg := r.get()
		if agg.Run.FailureIntentAt == nil || agg.Run.FailureReason != execution.Reason(execution.ReasonServiceLost) {
			t.Fatalf("service loss must record the failure intent: %+v", agg.Run)
		}
		if agg.Run.Status.Terminal() {
			t.Fatal("terminal before every resource was stopped and removed")
		}
		if r.s.RunReservations(SharedScope.TenantID) != 1 {
			t.Fatal("capacity released before verified removal")
		}
		r.passes(6)
		agg = r.get()
		if agg.Run.Status != execution.RunFailed || !agg.ResourcesRemoved() || len(r.s.Resources.LiveKeys()) != 0 {
			t.Fatalf("after cleanup: %s removed=%v live=%v", agg.Run.Status, agg.ResourcesRemoved(), r.s.Resources.LiveKeys())
		}
		if r.s.RunReservations(SharedScope.TenantID) != 0 {
			t.Fatal("the reservation was not released with the terminal run")
		}
		if strings.Count(strings.Join(r.s.Resources.Calls(), ","), "ensure:service.db") != 1 {
			t.Fatal("a lost service must not be replaced under the same run")
		}
	})

	t.Run("an unknown observation blocks dispatch without counting as loss until the deadline", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		r.s.Resources.FailObserve("service.db", errors.New("api outage"))
		r.tick(2 * time.Second)
		r.passes(2)
		agg := r.get()
		if agg.Run.FailureIntentAt != nil || agg.Resource("service.db").UnknownSince == nil {
			t.Fatalf("an outage must not fail the run: %+v", agg.Run)
		}
		if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("dispatch must stay blocked: %v", err)
		}
		r.tick(2 * time.Hour)
		r.pass()
		agg = r.get()
		if agg.Run.FailureReason != execution.Reason(execution.ReasonRunDeadlineExceeded) {
			t.Fatalf("the deadline must end the blocked run: %+v", agg.Run)
		}
	})

	t.Run("the total deadline covers waiting and fails the run with run_deadline_exceeded", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		r.up()
		r.tick(2 * time.Hour)
		r.pass()
		agg := r.get()
		if agg.Run.FailureReason != execution.Reason(execution.ReasonRunDeadlineExceeded) || agg.Steps[0].Status != execution.StepCancelled {
			t.Fatalf("deadline: reason=%s step=%s", agg.Run.FailureReason, agg.Steps[0].Status)
		}
		if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("no dispatch past the deadline: %v", err)
		}
		r.passes(6)
		if agg := r.get(); agg.Run.Status != execution.RunFailed || !agg.ResourcesRemoved() {
			t.Fatalf("cleanup after the deadline: %s", agg.Run.Status)
		}
	})

	t.Run("cancellation tears down in order, stuck cleanup stays visible, retried with backoff and survives a restart", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		r.s.Resources.HoldRemove("workspace", 3)
		if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Fatal(err)
		}
		// Every resource has its stop intent at once: its admission is revoked.
		for _, res := range r.get().Resources {
			if !res.Stopping() {
				t.Fatalf("%s has no stop intent after cancellation", res.Key)
			}
		}
		r.pass()
		agg := r.get()
		if agg.Run.Status.Terminal() || agg.CleanupReason() != execution.ReasonCleanupPending {
			t.Fatalf("stuck cleanup must stay visible and nonterminal: %s %q", agg.Run.Status, agg.CleanupReason())
		}
		view := agg.SharedView()
		if view == nil || view.CleanupReason != execution.ReasonCleanupPending || view.RunModel != "run_shared" || view.DeadlineAt == nil || len(view.Resources) != 4 {
			t.Fatalf("the observation must show the stuck cleanup: %+v", view)
		}
		for _, rv := range view.Resources {
			if rv.Key == "workspace" && (rv.Reason != execution.ReasonCleanupPending || rv.RemovedAt != nil || rv.StoppedAt == nil) {
				t.Fatalf("workspace view %+v", rv)
			}
		}
		calls := r.s.Resources.Calls()
		if i, j := callIndex(calls, "remove:service.db"), callIndex(calls, "stop:workspace"); i < 0 || j < 0 {
			t.Fatalf("the service is removed and the workspace stopped: %v", calls)
		}
		if callIndex(calls, "stop:network") >= 0 {
			t.Fatalf("the network is removed last, after the workspace: %v", calls)
		}
		before := r.s.Resources.Count("remove:workspace")
		r.pass() // within the backoff window: no new remove call
		if r.s.Resources.Count("remove:workspace") != before {
			t.Fatal("the remove was retried inside its backoff window")
		}
		if r.s.RunReservations(SharedScope.TenantID) != 1 {
			t.Fatal("the reservation was released with cleanup pending")
		}
		restarted := r.s.Restart()
		for i := 0; i < 8; i++ {
			r.tick(10 * time.Second)
			r.passOn(restarted)
		}
		agg = r.get()
		if agg.Run.Status != execution.RunCancelled || !agg.ResourcesRemoved() || len(r.s.Resources.LiveKeys()) != 0 {
			t.Fatalf("after a restart: %s removed=%v live=%v", agg.Run.Status, agg.ResourcesRemoved(), r.s.Resources.LiveKeys())
		}
		final := r.s.Resources.Calls()
		if callIndex(final, "stop:network") < callIndex(final, "remove:workspace") {
			t.Fatalf("the network must be removed last: %v", final)
		}
		if r.s.RunReservations(SharedScope.TenantID) != 0 {
			t.Fatal("the reservation survived the terminal run")
		}
	})

	t.Run("an unconfirmed stop keeps the run nonterminal and visible as resource_stop_unconfirmed", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		r.s.Resources.HoldStop("service.db", 2)
		if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Fatal(err)
		}
		r.pass()
		agg := r.get()
		if agg.CleanupReason() != execution.ReasonStopUnconfirmed || agg.Run.Status.Terminal() {
			t.Fatalf("reason %q status %s", agg.CleanupReason(), agg.Run.Status)
		}
		if agg.Resource("service.db").Removed() || callIndex(r.s.Resources.Calls(), "remove:service.db") >= 0 {
			t.Fatal("removal must not be requested before a verified stop")
		}
		for i := 0; i < 8; i++ {
			r.tick(10 * time.Second)
			r.pass()
		}
		if agg := r.get(); agg.Run.Status != execution.RunCancelled {
			t.Fatalf("status %s", agg.Run.Status)
		}
	})

	t.Run("a create racing cancellation is reaped and a late create can no longer arrive", func(t *testing.T) {
		t.Run("the response was lost after the object was created", func(t *testing.T) {
			r := newRig(t)
			r.admit(withDB)
			r.s.Resources.AutoReady = true
			r.s.Resources.LoseEnsure("service.db", errors.New("connection reset"))
			r.start()
			r.pass()
			res := r.res("service.db")
			if res.Bound() || res.EnsureRequestedAt == nil || !r.s.Resources.Exists("service.db") {
				t.Fatalf("expected an unbound intent whose object exists: %+v", res)
			}
			if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r.run.ID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				r.tick(10 * time.Second)
				r.pass()
			}
			if agg := r.get(); agg.Run.Status != execution.RunCancelled || len(r.s.Resources.LiveKeys()) != 0 {
				t.Fatalf("orphan: %s live=%v", agg.Run.Status, r.s.Resources.LiveKeys())
			}
		})
		t.Run("the create had not arrived yet", func(t *testing.T) {
			r := newRig(t)
			r.admit(withDB)
			r.s.Resources.AutoReady = true
			r.s.Resources.LateEnsure("service.db", errors.New("timeout"))
			r.start()
			r.pass()
			if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r.run.ID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				r.tick(10 * time.Second)
				r.pass()
			}
			if r.s.Resources.Deliver("service.db") {
				t.Fatal("a create arrived after the verified stop")
			}
			if agg := r.get(); agg.Run.Status != execution.RunCancelled || len(r.s.Resources.LiveKeys()) != 0 {
				t.Fatalf("late create: %s live=%v", agg.Run.Status, r.s.Resources.LiveKeys())
			}
		})
	})

	t.Run("a same-name object of another fingerprint is a collision: reported, never adopted, stopped or removed", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.start()
		specs, err := execution.BuildResourceSpecs(r.get().Run, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, sp := range specs {
			if sp.Key == "network" {
				foreign := sp
				foreign.Backend = "someone-else" // a different fingerprint at our name
				r.s.Resources.Plant(foreign)
			}
		}
		r.pass()
		agg := r.get()
		if agg.Run.FailureReason != execution.Reason(execution.ReasonResourceCollision) {
			t.Fatalf("collision must fail the run: %+v", agg.Run)
		}
		uid := r.s.Resources.UID("network")
		for i := 0; i < 6; i++ {
			r.tick(10 * time.Second)
			r.pass()
		}
		if !r.s.Resources.Exists("network") || r.s.Resources.UID("network") != uid {
			t.Fatal("the foreign object was removed")
		}
		if agg := r.get(); !agg.Resource("network").Removed() {
			t.Fatalf("our record of a name we never owned completes locally: %+v", agg.Resource("network"))
		}
	})

	t.Run("duplicate and reordered wakeups from several controllers are harmless", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		r.s.Resources.AutoReady = true
		r.start()
		other := r.s.Restart()
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			for _, svc := range []*execution.Service{r.s.Svc, other} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = svc.ReconcileRun(context.Background(), r.ref())
				}()
			}
		}
		wg.Wait()
		r.pass()
		if _, err := r.receipt(); err != nil {
			t.Fatal(err)
		}
		r.passes(2)
		agg := r.get()
		uids := map[string]bool{}
		for _, res := range agg.Resources {
			if res.UID == "" || uids[res.UID] {
				t.Fatalf("every resource has exactly one bound UID: %+v", res)
			}
			uids[res.UID] = true
		}
		// Two controllers racing to dispatch yield one attempt and one holder.
		var claimed int
		var mu sync.Mutex
		var cw sync.WaitGroup
		for _, svc := range []*execution.Service{r.s.Svc, other} {
			cw.Add(1)
			go func() {
				defer cw.Done()
				if in, err := svc.ClaimRunnable(context.Background(), SharedScope.TenantID); err == nil && in != nil {
					mu.Lock()
					claimed++
					mu.Unlock()
				}
			}()
		}
		cw.Wait()
		agg = r.get()
		if claimed != 1 || len(agg.Attempts) != 1 || len(agg.Holders) != 1 {
			t.Fatalf("claimed=%d attempts=%d holders=%d", claimed, len(agg.Attempts), len(agg.Holders))
		}
	})

	t.Run("a terminal run still showing unfinished resources is cleaned without changing its outcome", func(t *testing.T) {
		r := newRig(t)
		r.admit(withDB)
		r.up()
		r.s.ForceTerminal(SharedScope, r.run.ID)
		agg := r.get()
		if !agg.Run.Status.Terminal() || agg.ResourcesRemoved() {
			t.Fatalf("fixture: %s", agg.Run.Status)
		}
		for i := 0; i < 6; i++ {
			r.tick(10 * time.Second)
			if _, err := r.s.Svc.ReconcileTenant(context.Background(), SharedScope.TenantID); err != nil {
				t.Fatal(err)
			}
		}
		agg = r.get()
		if agg.Run.Status != execution.RunFailed || !agg.ResourcesRemoved() || len(r.s.Resources.LiveKeys()) != 0 {
			t.Fatalf("orphan cleanup: %s removed=%v live=%v", agg.Run.Status, agg.ResourcesRemoved(), r.s.Resources.LiveKeys())
		}
		if r.s.RunReservations(SharedScope.TenantID) != 0 {
			t.Fatal("the reservation of the terminal run was not released")
		}
	})

	t.Run("a late result of a cancelled attempt is evidence only and cannot progress the run", func(t *testing.T) {
		r := newRig(t)
		r.admit(twoStep)
		r.up()
		a := r.mustClaim()
		r.started(a)
		if _, err := r.s.Svc.Cancel(context.Background(), sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Drive.Succeed(a, nil, ""); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("a late result must be refused: %v", err)
		}
		agg := r.get()
		if agg.Step("a").Status == execution.StepSucceeded || agg.Run.Status == execution.RunSucceeded {
			t.Fatal("a late result progressed the run")
		}
	})

	t.Run("a run keeps its pinned policy when the current policy changes", func(t *testing.T) {
		r := newRig(t)
		r.admit(svcOnly)
		old := r.get().Run.Shared.Policy
		p := TestPolicy()
		p.Limits.StopGraceSeconds = 45
		r.s.Policies.Set(p)
		r.up()
		in := r.mustClaim()
		c, err := in.DecodeContextV2()
		if err != nil || string(c.Policy.Digest) != old.Digest {
			t.Fatalf("the attempt must carry the run's pinned policy: %v %s", err, c.Policy.Digest)
		}
		r.settledSucceed(in, "")
		next := r.admit(svcOnly)
		if np := r.s.Store; np == nil {
			t.Fatal("no store")
		}
		agg, _ := r.s.Store.Get(context.Background(), SharedScope, next.ID)
		if agg.Run.Shared.Policy.Digest == old.Digest {
			t.Fatal("a later admission must pin the new policy")
		}
	})

	sharedRegressions(t, open)

	t.Run("legacy runs are unaffected by the shared composition", func(t *testing.T) {
		r := newRig(t)
		def := r.s.Seed.Define(SharedScope, "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: legacy}\nspec:\n  steps:\n    - {id: job, kind: container, image: "+
			SharedImage+", command: [/bin/true], timeoutSeconds: 60, workspace: {access: read, from: run}}\n")
		run, err := r.s.Svc.Admit(context.Background(), sharedCaller, execution.AdmitCommand{
			Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID, ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA,
		})
		if err != nil {
			t.Fatal(err)
		}
		agg, err := r.s.Store.Get(context.Background(), SharedScope, run.ID)
		if err != nil || agg.Run.Shared != nil || agg.Run.DeadlineAt != nil || len(agg.Resources) != 0 || agg.Run.Snapshot.Runtime.ResourceRequires != 0 {
			t.Fatalf("legacy run: %+v %v", agg.Run, err)
		}
		in := r.mustClaimRun()
		if in == nil || in.StepID != "job" {
			t.Fatal("the legacy run must dispatch as before")
		}
		if c, err := in.DecodeContext(); err != nil || c.Workspace.Access != runtimev1.AccessRead {
			t.Fatalf("legacy context: %v", err)
		}
		r.run = run
		r.tick(3 * time.Second)
		r.passes(2)
		seen := 0
		for _, s := range r.s.Starts() {
			if s.AttemptID != in.AttemptID {
				continue
			}
			seen++
			if s.Shared != nil {
				t.Fatalf("a legacy attempt received a shared launch: %+v", s.Shared)
			}
		}
		if seen == 0 {
			t.Fatal("the legacy attempt was never started, so nothing was checked")
		}
	})
}

func (r *rig) mustClaimRun() *execution.AttemptSpec {
	r.t.Helper()
	in, err := r.claim()
	if err != nil {
		r.t.Fatalf("claim: %v", err)
	}
	return in
}

// admitTwoServices checks that a policy allowing one service refuses a document
// with two.
func (r *rig) admitTwoServices() {
	r.t.Helper()
	yaml := strings.Replace(SharedSpecDoc(true, SharedStep("job")), "  steps:", "    cache:\n      image: "+SharedImage+
		"\n      ports: {redis: {port: 6379}}\n      resources: {cpuMillis: 100, memoryMiB: 64, ephemeralMiB: 64}\n      readiness: {tcp: redis, timeoutSeconds: 30}\n  steps:", 1)
	def := r.s.DefineShared(yaml)
	_, err := r.s.Svc.Admit(context.Background(), sharedCaller, execution.AdmitCommand{
		Scope: SharedScope, WorkItemID: r.s.Project.Item, DefinitionID: def.ID, ContentRevision: r.s.Seed.ContentRevision(r.s.Project), SourceCommit: CommitA,
	})
	if !errors.Is(err, execution.ErrAdmissionRefused) {
		r.t.Fatalf("too many services: %v", err)
	}
}

var _ = domain.Scope{}

// SharedRun is one run_shared run driven by a test through the lifecycle, for
// the adapter-specific suites that need the same fixture the contract uses.
type SharedRun struct{ r *rig }

// NewSharedRun admits a run of the document.
func NewSharedRun(t *testing.T, s SharedSubject, yaml string) *SharedRun {
	t.Helper()
	r := &rig{t: t, s: s}
	r.admit(yaml)
	return &SharedRun{r: r}
}

// Run is the admitted run.
func (s *SharedRun) Run() *execution.Run { return s.r.run }

// Get loads the aggregate.
func (s *SharedRun) Get() *execution.Aggregate { return s.r.get() }

// Start performs the claim that starts resource holding.
func (s *SharedRun) Start() { s.r.start() }

// Pass runs one reconciliation pass.
func (s *SharedRun) Pass() { s.r.pass() }

// Up drives the run to an open barrier.
func (s *SharedRun) Up() { s.r.up() }

// Claim claims one step of the tenant.
func (s *SharedRun) Claim() (*execution.AttemptSpec, error) { return s.r.claim() }

// Tick advances the clock.
func (s *SharedRun) Tick(d time.Duration) { s.r.tick(d) }
