package workspacetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// StoreEnv is the workspace persistence under contract. The fixture scopes
// ScopeA, ScopeA2 (another project of tenant A) and ScopeB exist in every
// implementation.
type StoreEnv interface {
	Store() workspace.Store
	// EventIDs lists the tenant's outbox event IDs in order.
	EventIDs(t testing.TB, tenant domain.TenantID) []string
	// AuditActions lists the workspace's audit actions, sorted.
	AuditActions(t testing.TB, scope domain.Scope, id workspace.ID) []string
	// FailEvents makes every event append fail with err after the event was
	// written in the mutation's transaction; nil restores it.
	FailEvents(err error)
}

// OpenStore builds an empty StoreEnv.
type OpenStore func(testing.TB) StoreEnv

var (
	storeNow = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	commitA  = workspace.Commit(strings.Repeat("a", 40))
	commitB  = workspace.Commit(strings.Repeat("b", 40))
	commitC  = workspace.Commit(strings.Repeat("c", 40))
	owner    = workspace.Actor{Type: workspace.ActorUser, ID: "usr_t088"}
)

func auditOf(action string) workspace.Audit {
	return workspace.Audit{Action: action, Actor: owner, Metadata: map[string]any{"by": "contract"}}
}

func stateEvent(from, to, reason string) workspace.Event {
	return workspace.Event{Kind: workspace.EventStateChanged, Actor: owner, From: from, To: to, Reason: reason}
}

// StoreContract is the behavior every workspace.Store adapter shares: one
// workspace per work item, project- and tenant-scoped reads, a mutation that
// runs on a private copy and commits the workspace, its receipt, retained
// references, audit and events together or not at all, receipts and pins that
// upsert only their settlement fields, and the reconciler's and the project's
// listings. It asserts outcomes, not lock order or isolation levels.
func StoreContract(t *testing.T, open OpenStore) {
	t.Helper()
	ctx := workspace.WithRequestID(context.Background(), "req_t088")

	newWS := func(t *testing.T, scope domain.Scope, id workspace.ID, item domain.WorkItemID) *workspace.Workspace {
		t.Helper()
		w, err := workspace.New(workspace.NewParams{ID: id, Scope: scope, WorkItemID: item, RepositoryID: "repo_t088",
			RepositoryURL: "https://git.example.test/acme/app.git", CheckoutSecretRef: "secret://t088/checkout", PublisherSecretRef: "secret://t088/publisher"}, storeNow)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	insert := func(t *testing.T, env StoreEnv, w *workspace.Workspace) (*workspace.Workspace, bool) {
		t.Helper()
		got, created, err := env.Store().Insert(ctx, w, workspace.Effects{
			Audit: []workspace.Audit{auditOf("workspace.provision_requested")}, Events: []workspace.Event{stateEvent("none", "provisioning", "provision_requested")}})
		if err != nil {
			t.Fatalf("insert %s: %v", w.ID, err)
		}
		return got, created
	}
	// provision makes the workspace available at commitA, as the service does.
	provision := func(t *testing.T, env StoreEnv, w *workspace.Workspace) *workspace.Aggregate {
		t.Helper()
		agg, err := env.Store().Update(ctx, w.Scope, w.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			if _, err := a.Workspace.RecordInitialIntent(commitA, storeNow.Add(time.Minute)); err != nil {
				return workspace.Effects{}, err
			}
			if err := a.Workspace.CompleteProvisioning(commitA, storeNow.Add(2*time.Minute)); err != nil {
				return workspace.Effects{}, err
			}
			return workspace.Effects{Audit: []workspace.Audit{auditOf("workspace.provisioned")}, Events: []workspace.Event{stateEvent("provisioning", "available", "branch_ready")}}, nil
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		return agg
	}

	t.Run("one workspace per work item, written with its audit and event", func(t *testing.T) {
		env := open(t)
		w := newWS(t, ScopeA, "ws_t088_1", "wi_1")
		got, created := insert(t, env, w)
		if !created || got.ID != w.ID || got.State != workspace.StateProvisioning || got.Version != 1 || got.WorkBranch != workspace.WorkBranch("wi_1") || got.Fence.Epoch != 1 {
			t.Fatalf("insert: created %v, %+v", created, got)
		}
		if ids := env.EventIDs(t, ScopeA.TenantID); len(ids) != 1 {
			t.Fatalf("events after insert: %v", ids)
		}
		if actions := env.AuditActions(t, ScopeA, w.ID); !slices.Equal(actions, []string{"workspace.provision_requested"}) {
			t.Fatalf("audit: %v", actions)
		}
		// Another identity for the same work item returns the stored workspace and
		// writes nothing.
		again, created := insert(t, env, newWS(t, ScopeA, "ws_t088_2", "wi_1"))
		if created || again.ID != w.ID {
			t.Fatalf("a second workspace for the work item: created %v, %+v", created, again)
		}
		if ids := env.EventIDs(t, ScopeA.TenantID); len(ids) != 1 {
			t.Fatalf("a repeated insert wrote an event: %v", ids)
		}
		if _, err := env.Store().Get(ctx, ScopeA, "ws_t088_2"); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("the refused identity was stored: %v", err)
		}
		// The same work item in another project or tenant is another workspace.
		for i, scope := range []domain.Scope{ScopeA2, ScopeB} {
			other, created := insert(t, env, newWS(t, scope, workspace.ID([]string{"ws_t088_3", "ws_t088_4"}[i]), "wi_1"))
			if !created || other.Scope != scope {
				t.Fatalf("work item wi_1 in %v: created %v %+v", scope, created, other)
			}
		}
	})

	t.Run("an identity or work branch collision never replaces a workspace", func(t *testing.T) {
		env := open(t)
		w, _ := insert(t, env, newWS(t, ScopeA, "ws_t088_c1", "wi_c1"))
		events0, audit0 := len(env.EventIDs(t, ScopeA.TenantID)), len(env.AuditActions(t, ScopeA, w.ID))
		refused := func(name string, other *workspace.Workspace) {
			t.Helper()
			got, created, err := env.Store().Insert(ctx, other, workspace.Effects{
				Audit: []workspace.Audit{auditOf("workspace.provision_requested")}, Events: []workspace.Event{stateEvent("none", "provisioning", "provision_requested")}})
			if err == nil && created {
				t.Fatalf("%s: a colliding workspace was created: %+v", name, got)
			}
			if err == nil && got.ID != w.ID {
				t.Fatalf("%s: the insert reported success for another identity: %+v", name, got)
			}
			stored, gerr := env.Store().Get(ctx, ScopeA, w.ID)
			if gerr != nil || stored.WorkItemID != "wi_c1" || stored.Version != 1 || stored.State != workspace.StateProvisioning {
				t.Fatalf("%s: the stored workspace changed: %+v %v", name, stored, gerr)
			}
			if n := len(env.EventIDs(t, ScopeA.TenantID)); n != events0 {
				t.Fatalf("%s: events %d, want %d", name, n, events0)
			}
			if n := len(env.AuditActions(t, ScopeA, w.ID)); n != audit0 {
				t.Fatalf("%s: audit entries %d, want %d", name, n, audit0)
			}
			if byItem, gerr := env.Store().GetByWorkItem(ctx, ScopeA, other.WorkItemID); other.WorkItemID != "wi_c1" && !errors.Is(gerr, workspace.ErrNotFound) {
				t.Fatalf("%s: the work item %s resolves to %+v %v", name, other.WorkItemID, byItem, gerr)
			}
		}
		refused("the same identity for another work item", newWS(t, ScopeA, "ws_t088_c1", "wi_c2"))
		sameBranch := newWS(t, ScopeA, "ws_t088_c3", "wi_c3")
		sameBranch.WorkBranch = w.WorkBranch
		refused("the same repository and work branch", sameBranch)
	})

	t.Run("reads are scoped to the project", func(t *testing.T) {
		env := open(t)
		w, _ := insert(t, env, newWS(t, ScopeA, "ws_t088_5", "wi_5"))
		st := env.Store()
		if got, err := st.Get(ctx, ScopeA, w.ID); err != nil || got.WorkItemID != "wi_5" {
			t.Fatalf("get: %+v %v", got, err)
		}
		if got, err := st.GetByWorkItem(ctx, ScopeA, "wi_5"); err != nil || got.ID != w.ID {
			t.Fatalf("get by work item: %+v %v", got, err)
		}
		if got, err := st.View(ctx, ScopeA, w.ID, workspace.Load{Pins: true}); err != nil || got.Workspace.ID != w.ID || len(got.Pins) != 0 || got.Receipt != nil {
			t.Fatalf("view: %+v %v", got, err)
		}
		for name, err := range map[string]error{
			"get from the sibling project":        second(st.Get(ctx, ScopeA2, w.ID)),
			"get from another tenant":             second(st.Get(ctx, ScopeB, w.ID)),
			"get an unknown workspace":            second(st.Get(ctx, ScopeA, "ws_unknown")),
			"by work item in the sibling project": second(st.GetByWorkItem(ctx, ScopeA2, "wi_5")),
			"by work item in another tenant":      second(st.GetByWorkItem(ctx, ScopeB, "wi_5")),
			"by an unknown work item":             second(st.GetByWorkItem(ctx, ScopeA, "wi_unknown")),
			"view from the sibling project":       second(st.View(ctx, ScopeA2, w.ID, workspace.Load{})),
			"view from another tenant":            second(st.View(ctx, ScopeB, w.ID, workspace.Load{})),
			"update from the sibling project": second(st.Update(ctx, ScopeA2, w.ID, workspace.Load{}, func(*workspace.Aggregate) (workspace.Effects, error) {
				t.Error("a mutation ran for a workspace of another project")
				return workspace.Effects{}, nil
			})),
			"update from another tenant": second(st.Update(ctx, ScopeB, w.ID, workspace.Load{}, func(*workspace.Aggregate) (workspace.Effects, error) {
				t.Error("a mutation ran for a workspace of another tenant")
				return workspace.Effects{}, nil
			})),
		} {
			if !errors.Is(err, workspace.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A returned workspace is a copy: changing it changes nothing stored.
		got, _ := st.Get(ctx, ScopeA, w.ID)
		got.State, got.LastError = workspace.StateArchived, "tampered"
		if again, _ := st.Get(ctx, ScopeA, w.ID); again.State != workspace.StateProvisioning || again.LastError != "" {
			t.Fatalf("a caller changed the stored workspace: %+v", again)
		}
	})

	t.Run("a mutation commits its effects together or none of them", func(t *testing.T) {
		env := open(t)
		w, _ := insert(t, env, newWS(t, ScopeA, "ws_t088_6", "wi_6"))
		events0 := len(env.EventIDs(t, ScopeA.TenantID))
		audit0 := len(env.AuditActions(t, ScopeA, w.ID))

		boom := errors.New("the mutation failed after changing the aggregate")
		_, err := env.Store().Update(ctx, ScopeA, w.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			if err := a.Workspace.CompleteProvisioning(commitA, storeNow); err != nil {
				return workspace.Effects{}, err
			}
			return workspace.Effects{}, boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("update: %v", err)
		}
		if got, _ := env.Store().Get(ctx, ScopeA, w.ID); got.State != workspace.StateProvisioning || got.Version != 1 || got.AcceptedCommit != "" {
			t.Fatalf("a failed mutation changed the workspace: %+v", got)
		}

		// A failing event append undoes the aggregate, the receipt and the audit.
		env.FailEvents(errors.New("event bus unavailable"))
		_, err = env.Store().Update(ctx, ScopeA, w.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			if err := a.Workspace.CompleteProvisioning(commitA, storeNow); err != nil {
				return workspace.Effects{}, err
			}
			return workspace.Effects{Audit: []workspace.Audit{auditOf("workspace.provisioned")}, Events: []workspace.Event{stateEvent("provisioning", "available", "branch_ready")}}, nil
		})
		if err == nil {
			t.Fatal("an update reported success although its event could not be appended")
		}
		env.FailEvents(nil)
		if got, _ := env.Store().Get(ctx, ScopeA, w.ID); got.State != workspace.StateProvisioning || got.Version != 1 {
			t.Fatalf("a failed event append left the workspace changed: %+v", got)
		}
		if n := len(env.EventIDs(t, ScopeA.TenantID)); n != events0 {
			t.Fatalf("events: %d, want %d", n, events0)
		}
		if n := len(env.AuditActions(t, ScopeA, w.ID)); n != audit0 {
			t.Fatalf("audit entries: %d, want %d", n, audit0)
		}
		// Nothing was replayed: the same mutation now applies exactly once.
		agg := provision(t, env, w)
		if agg.Workspace.State != workspace.StateAvailable || agg.Workspace.Version != 3 || agg.Workspace.AcceptedCommit != commitA {
			t.Fatalf("provisioned: %+v", agg.Workspace)
		}
		got, _ := env.Store().Get(ctx, ScopeA, w.ID)
		if got.State != workspace.StateAvailable || got.Version != 3 || got.InitialCommit != commitA || got.AcceptedCommit != commitA || !got.UpdatedAt.After(got.CreatedAt) {
			t.Fatalf("stored: %+v", got)
		}
		if n := len(env.EventIDs(t, ScopeA.TenantID)); n != events0+1 {
			t.Fatalf("events after provisioning: %d", n)
		}
		if actions := env.AuditActions(t, ScopeA, w.ID); !slices.Equal(actions, []string{"workspace.provision_requested", "workspace.provisioned"}) {
			t.Fatalf("audit: %v", actions)
		}
		// A mutation that changes nothing writes no version.
		noop, err := env.Store().Update(ctx, ScopeA, w.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) { return workspace.Effects{}, nil })
		if err != nil || noop.Workspace.Version != 3 {
			t.Fatalf("no-op update: %+v %v", noop, err)
		}
		if got, _ := env.Store().Get(ctx, ScopeA, w.ID); got.Version != 3 {
			t.Fatalf("a no-op update advanced the version: %d", got.Version)
		}
	})

	t.Run("writer, publication receipt and pins persist with the workspace", func(t *testing.T) {
		env := open(t)
		w, _ := insert(t, env, newWS(t, ScopeA, "ws_t088_7", "wi_7"))
		provision(t, env, w)
		st := env.Store()
		now := storeNow.Add(time.Hour)

		// Reserve the writer slot.
		agg, err := st.Update(ctx, ScopeA, w.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			return workspace.Effects{}, a.Workspace.ReserveWriter("att_1", commitA, commitA, now)
		})
		if err != nil || agg.Workspace.Writer == nil || agg.Workspace.Writer.AttemptID != "att_1" {
			t.Fatalf("reserve: %+v %v", agg, err)
		}
		got, _ := st.Get(ctx, ScopeA, w.ID)
		if got.Writer == nil || got.Writer.AttemptID != "att_1" || got.Writer.InputCommit != commitA || got.Writer.ExpectedTip != commitA || !got.Writer.ReservedAt.Equal(now) {
			t.Fatalf("stored writer: %+v", got.Writer)
		}
		if _, err := st.Update(ctx, ScopeA, w.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			return workspace.Effects{}, a.Workspace.ReserveWriter("att_2", commitA, commitA, now)
		}); !errors.Is(err, workspace.ErrWriterBusy) {
			t.Fatalf("a second writer: %v", err)
		}

		// Begin a publication: pending intent on the workspace, receipt beside it.
		proposal := workspace.Proposal{AttemptID: "att_1", Commit: commitB, ExpectedTip: commitA, ArtifactID: "art_1", BundleDigest: "sha256:" + strings.Repeat("d", 64)}
		var receipt *workspace.Publication
		if _, err := st.Update(ctx, ScopeA, w.ID, workspace.Load{Receipt: "att_1"}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			if a.Receipt != nil {
				t.Error("a receipt exists before any proposal")
			}
			r, disp, err := a.Workspace.BeginPublication(proposal, a.Receipt, now)
			if err != nil || disp != workspace.DispositionNew {
				return workspace.Effects{}, errors.Join(errors.New("begin publication"), err)
			}
			receipt = r
			return workspace.Effects{Receipt: r}, nil
		}); err != nil {
			t.Fatal(err)
		}
		view, err := st.View(ctx, ScopeA, w.ID, workspace.Load{Receipt: "att_1"})
		if err != nil || view.Receipt == nil || view.Receipt.Status != workspace.PublicationPending || view.Receipt.Commit != commitB ||
			view.Receipt.ExpectedCommit != commitA || view.Receipt.Outcome != workspace.OutcomeUnsettled || view.Workspace.Pending == nil || view.Workspace.Pending.Commit != commitB {
			t.Fatalf("view: %+v %v", view, err)
		}
		if other, err := st.View(ctx, ScopeA, w.ID, workspace.Load{Receipt: "att_other"}); err != nil || other.Receipt != nil {
			t.Fatalf("another attempt's receipt: %+v %v", other, err)
		}
		// An identical proposal observes the stored receipt; a changed one is refused.
		if _, err := st.Update(ctx, ScopeA, w.ID, workspace.Load{Receipt: "att_1"}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			r, disp, err := a.Workspace.BeginPublication(proposal, a.Receipt, now)
			if err != nil || disp != workspace.DispositionReplay || r.Commit != commitB {
				return workspace.Effects{}, errors.Join(errors.New("replay"), err)
			}
			changed := proposal
			changed.Commit = commitC
			if _, _, err := a.Workspace.BeginPublication(changed, a.Receipt, now); !errors.Is(err, workspace.ErrProposalChanged) {
				return workspace.Effects{}, errors.Join(errors.New("a changed proposal"), err)
			}
			return workspace.Effects{}, nil
		}); err != nil {
			t.Fatal(err)
		}

		// Publish and settle: the receipt's settlement fields move, its identity does not.
		settled := now.Add(time.Minute)
		if _, err := st.Update(ctx, ScopeA, w.ID, workspace.Load{Receipt: "att_1"}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			if err := a.Workspace.ClaimPublisher("drv_1", settled, time.Minute); err != nil {
				return workspace.Effects{}, err
			}
			if _, err := a.Workspace.RecordPublished("drv_1", a.Receipt, settled); err != nil {
				return workspace.Effects{}, err
			}
			if err := a.Workspace.SettleWriter("att_1", workspace.OutcomeSucceeded, a.Receipt, settled); err != nil {
				return workspace.Effects{}, err
			}
			return workspace.Effects{Receipt: a.Receipt, Events: []workspace.Event{{Kind: workspace.EventCheckpointCreated, Actor: owner,
				PreviousCommit: commitA, NewCommit: commitB, PublishingAttempt: "att_1"}}}, nil
		}); err != nil {
			t.Fatal(err)
		}
		view, err = st.View(ctx, ScopeA, w.ID, workspace.Load{Receipt: "att_1"})
		if err != nil || view.Receipt.Status != workspace.PublicationPublished || view.Receipt.Outcome != workspace.OutcomeSucceeded || view.Receipt.SettledAt == nil ||
			view.Receipt.Commit != receipt.Commit || view.Receipt.ArtifactID != "art_1" || view.Workspace.AcceptedCommit != commitB || view.Workspace.Pending != nil || view.Workspace.Writer != nil {
			t.Fatalf("settled view: %+v %+v %v", view.Receipt, view.Workspace, err)
		}
		if ids := env.EventIDs(t, ScopeA.TenantID); len(ids) != 3 { // requested, provisioned and the checkpoint
			t.Fatalf("events: %v", ids)
		}

		// Retained references: stored with their settlement fields, listed in order.
		pin1, err := workspace.NewPin("pin_1", view.Workspace, commitB, workspace.PinReader, "run_1", "", workspace.DefaultRetainedRefPrefix, now)
		if err != nil {
			t.Fatal(err)
		}
		pin2, err := workspace.NewPin("pin_2", view.Workspace, commitB, workspace.PinReader, "run_2", "", workspace.DefaultRetainedRefPrefix, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Update(ctx, ScopeA, w.ID, workspace.Load{Pins: true}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			return workspace.Effects{Pins: []*workspace.RetainedRef{pin2, pin1}}, nil
		}); err != nil {
			t.Fatal(err)
		}
		pins, err := st.View(ctx, ScopeA, w.ID, workspace.Load{Pins: true})
		if err != nil || len(pins.Pins) != 2 || pins.Pins[0].ID != "pin_1" || pins.Pins[1].ID != "pin_2" || pins.Pins[0].Status != workspace.PinIntent || pins.Pins[0].Commit != commitB {
			t.Fatalf("pins: %+v %v", pins, err)
		}
		confirmed := *pins.Pins[0]
		at := now.Add(time.Minute)
		confirmed.Status, confirmed.ConfirmedAt = workspace.PinConfirmed, &at
		confirmed.Commit = commitC // an immutable field: a repeated write must not move it
		if _, err := st.Update(ctx, ScopeA, w.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			return workspace.Effects{Pins: []*workspace.RetainedRef{&confirmed}}, nil
		}); err != nil {
			t.Fatal(err)
		}
		pins, _ = st.View(ctx, ScopeA, w.ID, workspace.Load{Pins: true})
		if pins.Pins[0].Status != workspace.PinConfirmed || pins.Pins[0].ConfirmedAt == nil || !pins.Pins[0].ConfirmedAt.Equal(at) || pins.Pins[0].Commit != commitB || pins.Pins[1].Status != workspace.PinIntent {
			t.Fatalf("pins after confirmation: %+v %+v", pins.Pins[0], pins.Pins[1])
		}
		// Another tenant's workspace has no pins, whatever the identifiers.
		other, _ := insert(t, env, newWS(t, ScopeB, "ws_t088_8", "wi_7"))
		if v, err := st.View(ctx, ScopeB, other.ID, workspace.Load{Pins: true}); err != nil || len(v.Pins) != 0 {
			t.Fatalf("pins of another tenant's workspace: %+v %v", v, err)
		}
	})

	t.Run("the reconciler finds unfinished durable work only", func(t *testing.T) {
		env := open(t)
		st := env.Store()
		idle, _ := insert(t, env, newWS(t, ScopeA, "ws_t088_a1", "wi_a1"))
		provision(t, env, idle)                                            // available, nothing pending
		prov, _ := insert(t, env, newWS(t, ScopeA, "ws_t088_a2", "wi_a2")) // still provisioning
		archiving, _ := insert(t, env, newWS(t, ScopeA, "ws_t088_a3", "wi_a3"))
		provision(t, env, archiving)
		if _, err := st.Update(ctx, ScopeA, archiving.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			_, err := a.Workspace.RequestArchive(storeNow.Add(time.Hour))
			return workspace.Effects{}, err
		}); err != nil {
			t.Fatal(err)
		}
		pinning, _ := insert(t, env, newWS(t, ScopeA2, "ws_t088_a4", "wi_a4"))
		pinned := provision(t, env, pinning)
		pin, err := workspace.NewPin("pin_a4", pinned.Workspace, commitA, workspace.PinReader, "run_1", "", workspace.DefaultRetainedRefPrefix, storeNow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Update(ctx, ScopeA2, pinning.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			return workspace.Effects{Pins: []*workspace.RetainedRef{pin}}, nil
		}); err != nil {
			t.Fatal(err)
		}
		foreign, _ := insert(t, env, newWS(t, ScopeB, "ws_t088_a5", "wi_a5")) // provisioning, but tenant B

		var ids []workspace.ID
		after := workspace.ID("")
		for {
			page, err := st.Reconcilable(ctx, ScopeA.TenantID, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range page {
				ids = append(ids, c.ID)
				if c.Scope.TenantID != ScopeA.TenantID {
					t.Fatalf("a candidate of another tenant: %+v", c)
				}
			}
			if len(page) < 2 {
				break
			}
			after = page[len(page)-1].ID
		}
		want := []workspace.ID{prov.ID, archiving.ID, pinning.ID}
		if !slices.Equal(ids, want) {
			t.Fatalf("reconcilable %v, want %v (idle %s must not be listed)", ids, want, idle.ID)
		}
		// Once the pin is confirmed (no intent or removal outstanding) the
		// workspace is idle again.
		confirmed := *pin
		at := storeNow.Add(time.Minute)
		confirmed.Status, confirmed.ConfirmedAt = workspace.PinConfirmed, &at
		if _, err := st.Update(ctx, ScopeA2, pinning.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			return workspace.Effects{Pins: []*workspace.RetainedRef{&confirmed}}, nil
		}); err != nil {
			t.Fatal(err)
		}
		page, err := st.Reconcilable(ctx, ScopeA.TenantID, "", 10)
		if err != nil || len(page) != 2 {
			t.Fatalf("after confirmation: %+v %v", page, err)
		}
		if only, err := st.Reconcilable(ctx, ScopeB.TenantID, "", 10); err != nil || len(only) != 1 || only[0].ID != foreign.ID {
			t.Fatalf("tenant B: %+v %v", only, err)
		}
	})

	t.Run("a project lists its workspace IDs in order", func(t *testing.T) {
		env := open(t)
		for i, id := range []workspace.ID{"ws_t088_l3", "ws_t088_l1", "ws_t088_l2"} {
			insert(t, env, newWS(t, ScopeA, id, domain.WorkItemID([]string{"wi_l3", "wi_l1", "wi_l2"}[i])))
		}
		insert(t, env, newWS(t, ScopeA2, "ws_t088_l4", "wi_l4"))
		insert(t, env, newWS(t, ScopeB, "ws_t088_l5", "wi_l5"))
		st := env.Store()
		all, err := st.ListProject(ctx, ScopeA, "", 10)
		if err != nil || !slices.Equal(all, []workspace.ID{"ws_t088_l1", "ws_t088_l2", "ws_t088_l3"}) {
			t.Fatalf("list: %v %v", all, err)
		}
		page, err := st.ListProject(ctx, ScopeA, "ws_t088_l1", 1)
		if err != nil || !slices.Equal(page, []workspace.ID{"ws_t088_l2"}) {
			t.Fatalf("page: %v %v", page, err)
		}
		if other, err := st.ListProject(ctx, ScopeA2, "", 10); err != nil || !slices.Equal(other, []workspace.ID{"ws_t088_l4"}) {
			t.Fatalf("sibling project: %v %v", other, err)
		}
	})
}

func second[T any](_ T, err error) error { return err }
