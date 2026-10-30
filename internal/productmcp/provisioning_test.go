package productmcp

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

func Test_unit_productmcp_provisioning_delegation_bounded(t *testing.T) {
	ctx := context.Background()
	eng := personCaller(tenantA, "erin")
	setup := func(t *testing.T, limits ...Limits) *fixture {
		f := newFixture(t, limits...)
		f.w.grant(eng, projP, "engineer")
		return f
	}
	in := func(key string) ProvisioningStartIn {
		return ProvisioningStartIn{ProjectID: string(projP), IdempotencyKey: key, DefinitionID: "def_bootstrap", Repository: "backend", Inputs: map[string]string{"stack": "go"}}
	}

	t.Run("a run starts within the caller's project and the server's budget", func(t *testing.T) {
		f := setup(t)
		r, err := f.svc.StartProvisioningRun(ctx, eng, in("k1"))
		if err != nil || r.Status != "queued" || r.DefinitionID != "def_bootstrap" || r.Replayed {
			t.Fatalf("%v %+v", err, r)
		}
		req := f.w.startReq[0]
		if req.MaxActive != DefaultLimits().MaxActiveProvisioning || req.Repository != "backend" || req.Inputs["stack"] != "go" {
			t.Fatalf("the admission got %+v", req)
		}
		if !strings.HasPrefix(string(req.RunID), "run_prov_") || string(req.RunID) != r.ID {
			t.Fatalf("the run identity is derived from the idempotency identity: %s %s", req.RunID, r.ID)
		}
		// A retry replays the same run, rereading it under current authority.
		again, err := f.svc.StartProvisioningRun(ctx, eng, in("k1"))
		if err != nil || again.ID != r.ID || !again.Replayed || len(f.w.runs) != 1 {
			t.Fatalf("%v %+v", err, again)
		}
		changed := in("k1")
		changed.DefinitionID = "def_other"
		_, err = f.svc.StartProvisioningRun(ctx, eng, changed)
		mustCode(t, err, "idempotency_conflict")
		// The run is another principal's identity when they reuse the key.
		other := personCaller(tenantA, "eli")
		f.w.grant(other, projP, "engineer")
		o, err := f.svc.StartProvisioningRun(ctx, other, in("k1"))
		if err != nil || o.ID == r.ID {
			t.Fatalf("%v %+v", err, o)
		}
	})

	t.Run("the active-run budget is bounded and enforced by the admission", func(t *testing.T) {
		lim := DefaultLimits()
		lim.MaxActiveProvisioning = 2
		f := setup(t, lim)
		for i := 0; i < 2; i++ {
			if _, err := f.svc.StartProvisioningRun(ctx, eng, in(fmt.Sprintf("k%d", i))); err != nil {
				t.Fatal(err)
			}
		}
		_, err := f.svc.StartProvisioningRun(ctx, eng, in("k-over"))
		mustCode(t, err, "budget_exhausted")
		if len(f.w.runs) != 2 {
			t.Fatalf("%d runs", len(f.w.runs))
		}
		// A replay of an admitted run is not new spend.
		if r, err := f.svc.StartProvisioningRun(ctx, eng, in("k0")); err != nil || !r.Replayed {
			t.Fatalf("%v %+v", err, r)
		}
		// The budget is per project.
		f.w.grant(eng, projQ, "engineer")
		q := in("k-q")
		q.ProjectID = string(projQ)
		if _, err := f.svc.StartProvisioningRun(ctx, eng, q); err != nil {
			t.Fatalf("another project has its own budget: %v", err)
		}
	})

	t.Run("only a person with run authority in that project may start one", func(t *testing.T) {
		f := setup(t)
		viewer, stranger := personCaller(tenantA, "vic"), personCaller(tenantA, "sam")
		f.w.grant(viewer, projP, "viewer")
		_, err := f.svc.StartProvisioningRun(ctx, viewer, in("k"))
		mustCode(t, err, "forbidden")
		_, err = f.svc.StartProvisioningRun(ctx, stranger, in("k"))
		mustCode(t, err, "not_found")
		// The engineer's grant on one project is no scope in another or a foreign tenant's.
		q := in("k")
		q.ProjectID = string(projQ)
		_, err = f.svc.StartProvisioningRun(ctx, eng, q)
		mustCode(t, err, "not_found")
		q.ProjectID = "p_foreign"
		_, err = f.svc.StartProvisioningRun(ctx, eng, q)
		mustCode(t, err, "not_found")
		// A provisioning attempt cannot start runs, so a run cannot fan out.
		a := attemptCaller(tenantA, projP, "att1")
		before := len(f.w.sawCalls)
		_, err = f.svc.StartProvisioningRun(ctx, a, in("k"))
		mustCode(t, err, "forbidden")
		if len(f.w.runs) != 0 || len(f.w.sawCalls) != before {
			t.Fatalf("a refused call admitted a run: %v", f.w.runs)
		}
		for _, sc := range f.w.sawScope {
			if sc.TenantID != tenantA {
				t.Fatalf("scope %v", sc)
			}
		}
	})

	t.Run("inputs are bounded before any admission", func(t *testing.T) {
		lim := DefaultLimits()
		lim.MaxProvisionInputs, lim.MaxInputValueBytes, lim.MaxInputBytes = 3, 16, 40
		f := setup(t, lim)
		bad := map[string]ProvisioningStartIn{}
		mk := func(name string, mut func(*ProvisioningStartIn)) {
			v := in("k-" + name)
			mut(&v)
			bad[name] = v
		}
		mk("count", func(v *ProvisioningStartIn) { v.Inputs = map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"} })
		mk("value", func(v *ProvisioningStartIn) { v.Inputs = map[string]string{"a": strings.Repeat("x", 17)} })
		mk("total", func(v *ProvisioningStartIn) {
			v.Inputs = map[string]string{"a": strings.Repeat("x", 16), "b": strings.Repeat("x", 16), "c": strings.Repeat("x", 16)}
		})
		mk("name", func(v *ProvisioningStartIn) { v.Inputs = map[string]string{"bad name": "x"} })
		mk("repository", func(v *ProvisioningStartIn) { v.Repository = "Not A Repo" })
		mk("definition", func(v *ProvisioningStartIn) { v.DefinitionID = "" })
		mk("project", func(v *ProvisioningStartIn) { v.ProjectID = "" })
		mk("key", func(v *ProvisioningStartIn) { v.IdempotencyKey = "no good" })
		for name, v := range bad {
			_, err := f.svc.StartProvisioningRun(ctx, eng, v)
			if code, _ := classify(err); code != "invalid_request" {
				t.Errorf("%s: %v", name, err)
			}
		}
		if len(f.w.sawCalls) != 0 {
			t.Fatalf("an out-of-bounds request reached the admission: %v", f.w.sawCalls)
		}
		if _, err := f.svc.StartProvisioningRun(ctx, eng, in("ok")); err != nil {
			t.Fatalf("in-bounds request refused: %v", err)
		}
	})

	t.Run("no credential or seed grant crosses the delegation", func(t *testing.T) {
		f := setup(t)
		for _, typ := range []reflect.Type{reflect.TypeOf(StartProvisioning{}), reflect.TypeOf(ProvisioningRun{}), reflect.TypeOf(ProvisioningStartIn{}), reflect.TypeOf(RunOut{})} {
			for i := 0; i < typ.NumField(); i++ {
				n := strings.ToLower(typ.Field(i).Name)
				for _, banned := range []string{"seed", "credential", "password", "token", "secret", "grant", "origin"} {
					if strings.Contains(n, banned) {
						t.Errorf("%s.%s: the tool delegates no authority and chooses no origin", typ.Name(), typ.Field(i).Name)
					}
				}
			}
		}
		if _, err := f.svc.StartProvisioningRun(ctx, eng, in("k")); err != nil {
			t.Fatal(err)
		}
		if f.w.seeds != 0 {
			t.Fatal("starting a run minted a seed grant")
		}
	})

	t.Run("a port that admits another run than the derived one is not trusted", func(t *testing.T) {
		f := setup(t)
		svc, _ := NewService(Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: fakeItems{f.w}, Provisioning: wrongRun{fakeProv{f.w}}, Results: f.res})
		_, err := svc.StartProvisioningRun(ctx, eng, in("k"))
		mustCode(t, err, "unavailable")
		if len(f.res.m) != 0 {
			t.Fatal("recorded a run that does not belong to the request")
		}
	})
}

type wrongRun struct{ fakeProv }

func (w wrongRun) Start(ctx context.Context, c Caller, s domain.Scope, req StartProvisioning) (ProvisioningRun, error) {
	r, err := w.fakeProv.Start(ctx, c, s, req)
	r.ID = "run_other"
	return r, err
}
