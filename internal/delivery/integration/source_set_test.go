package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
)

// recordForm is a set source as the record keeps it: the set and the primary
// source's repository URL and commit (repo_api).
func recordForm(set delivery.SourceSet) delivery.Source {
	return delivery.Source{RepositoryURL: "https://git.example.invalid/git/t/p/api.git", Commit: strings.Repeat("1", 40), SourceSet: set}
}

// seedRepositories records the repositories of the fixture set in the scope: a
// source set may only name repositories of the deployment's own project.
func (s *stack) seedRepositories(t *testing.T) {
	t.Helper()
	for _, id := range []string{"repo_api", "repo_web"} {
		s.X.OwnerExec(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
			VALUES ($1::text,$2::text,$3::text,replace($3::text,'_','-'),'main','internal','ready','user','u',now(),now())`, string(scopeA.TenantID), string(scopeA.ProjectID), id)
	}
}

func mustSetOf(t *testing.T, repos ...string) delivery.SourceSet {
	t.Helper()
	var commits []delivery.SourceCommit
	for i, r := range repos {
		commits = append(commits, delivery.SourceCommit{RepositoryID: r, Commit: strings.Repeat(string(rune('1'+i)), 40)})
	}
	set, err := delivery.NewSourceSet("sha256:"+strings.Repeat("d", 64), commits)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func twoRepositories(t *testing.T) delivery.SourceSet {
	t.Helper()
	set, err := delivery.NewSourceSet("sha256:"+strings.Repeat("d", 64), []delivery.SourceCommit{
		{RepositoryID: "repo_web", Commit: strings.Repeat("2", 40)}, {RepositoryID: "repo_api", Commit: strings.Repeat("1", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Test_source_set_deployment_record (DQ-17, DQ-A13) over the real store and
// migration 013: a deployment of a run with a source set is recorded, read back,
// listed, advanced and replayed with its digest and per-repository commits and
// no single source; a changed set under the same action key conflicts; the
// database refuses a mixed, empty, malformed or rewritten source; and a store
// that does not have the migration refuses a set instead of recording another
// source.
func Test_source_set_deployment_record(t *testing.T) {
	s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
	s.seedRepositories(t)
	set := twoRepositories(t)
	reg := s.registration()
	reg.Source = recordForm(set)
	ctx := context.Background()

	res, err := s.storeRegister(t, reg)
	if err != nil || !res.Created {
		t.Fatalf("register a set deployment: %+v %v", res, err)
	}
	id := res.Deployment.ID()
	for name, load := range map[string]func() (delivery.Deployment, error){
		"by id":     func() (delivery.Deployment, error) { return s.Store.GetDeployment(ctx, scopeA, id) },
		"by action": func() (delivery.Deployment, error) { return s.Store.FindByAction(ctx, scopeA, reg.ActionKey) },
	} {
		d, err := load()
		if err != nil || d.Record().Source != recordForm(set) {
			t.Fatalf("%s: %+v %v", name, d.Record().Source, err)
		}
	}
	list, err := s.Store.ListDeployments(ctx, scopeA, delivery.DeploymentFilter{}, "", 10)
	if err != nil || len(list) != 1 || list[0].Record().Source.SourceSet != set {
		t.Fatalf("list: %d %v", len(list), err)
	}
	if n := s.count(`SELECT count(*) FROM deployments WHERE tenant_id = $1 AND source_commit = $3 AND source_set_digest = $2`,
		string(scopeA.TenantID), set.Digest, strings.Repeat("1", 40)); n != 1 {
		t.Fatalf("the row stores the set and its primary source: %d", n)
	}

	t.Run("a replay observes the original; another set under the same action key conflicts", func(t *testing.T) {
		again, err := s.storeRegister(t, reg)
		if err != nil || again.Created || again.Deployment.ID() != id {
			t.Fatalf("replay: %+v %v", again, err)
		}
		moved := reg
		moved.Source = recordForm(func() delivery.SourceSet {
			c := set.Commits()
			c[1].Commit = strings.Repeat("3", 40)
			x, _ := delivery.NewSourceSet(set.Digest, c)
			return x
		}())
		if _, err := s.storeRegister(t, moved); err == nil || !isConflict(err) {
			t.Fatalf("another set for the same logical action: %v", err)
		}
		single := reg
		single.Source = s.D.Source
		if _, err := s.storeRegister(t, single); err == nil || !isConflict(err) {
			t.Fatalf("a single source for the same logical action: %v", err)
		}
	})

	t.Run("an outcome keeps the source set", func(t *testing.T) {
		m := delivery.Mutation{Actor: delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Engineer}, Now: time.Now().UTC().Truncate(time.Microsecond)}
		d, _, err := s.Store.Update(ctx, scopeA, id, m, func(d *delivery.Deployment) (delivery.Transition, error) {
			c, err := d.BeginApply(objects(deliverytest.TargetNamespace, "api"), m.Now)
			return delivery.Transition{Kind: "apply_started", Change: c, Attempt: reg.Attempt}, err
		})
		if err != nil || d.Record().Source.SourceSet != set || d.Status() != delivery.StatusRunning {
			t.Fatalf("update: %+v %v", d.Record(), err)
		}
	})

	t.Run("cleanup deletes the owned objects and keeps the source set", func(t *testing.T) {
		clean := reg
		clean.ActionKey = "act:setcleanup0123456"
		res, err := s.storeRegister(t, clean)
		if err != nil || !res.Created {
			t.Fatalf("register: %+v %v", res, err)
		}
		cid := res.Deployment.ID()
		m := delivery.Mutation{Actor: delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Engineer}, Now: time.Now().UTC().Truncate(time.Microsecond)}
		owned := objects(deliverytest.TargetNamespace, "web")
		step := func(fn func(d *delivery.Deployment) (delivery.Transition, error)) delivery.Deployment {
			t.Helper()
			d, _, err := s.Store.Update(ctx, scopeA, cid, m, fn)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
		step(func(d *delivery.Deployment) (delivery.Transition, error) {
			c, err := d.BeginApply(owned, m.Now)
			return delivery.Transition{Kind: "apply_started", Change: c, Attempt: clean.Attempt}, err
		})
		step(func(d *delivery.Deployment) (delivery.Transition, error) {
			c, err := d.Settle(delivery.Settlement{Reason: "apply_failed"}, m.Now)
			return delivery.Transition{Kind: "failed", Change: c, Attempt: clean.Attempt}, err
		})
		step(func(d *delivery.Deployment) (delivery.Transition, error) {
			_, err := d.RequestCleanup(m.Now)
			return delivery.Transition{Kind: "cleanup_requested", Attempt: clean.Attempt}, err
		})
		done := step(func(d *delivery.Deployment) (delivery.Transition, error) {
			_, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: owned}, m.Now)
			return delivery.Transition{Kind: "cleanup_done", Attempt: clean.Attempt}, err
		})
		if !done.Deleted() || done.Record().Source.SourceSet != set {
			t.Fatalf("a deleted deployment keeps the set its image was built from: %+v", done.Record().Source)
		}
		var kept int
		s.X.Scalar(&kept, `SELECT count(*) FROM deployments WHERE id = $1 AND deleted_at IS NOT NULL AND source_set_digest = $2 AND jsonb_array_length(source_commits) = 2`, string(cid), set.Digest)
		if kept != 1 {
			t.Fatalf("the stored record keeps its source set after cleanup: %d", kept)
		}
	})

	t.Run("the database refuses a malformed or rewritten source", func(t *testing.T) {
		fail := func(name, sql string, args ...any) {
			t.Helper()
			if err := s.X.OwnerExecErr(sql, args...); err == nil {
				t.Errorf("%s: statement succeeded", name)
			}
		}
		const q = `UPDATE deployments SET `
		fail("rewrite the digest", q+`source_set_digest = 'sha256:`+strings.Repeat("e", 64)+`' WHERE id = $1`, string(id))
		fail("rewrite the commits", q+`source_commits = '[{"repositoryId":"repo_api","commit":"`+strings.Repeat("9", 40)+`"}]'::jsonb WHERE id = $1`, string(id))
		fail("turn the set into a single source", q+`source_set_digest = NULL, source_commits = NULL, source_repository_url = 'https://git.example.invalid/r.git', source_commit = '`+strings.Repeat("1", 40)+`' WHERE id = $1`, string(id))
		fail("drop the set", q+`source_set_digest = NULL, source_commits = NULL WHERE id = $1`, string(id))
		// A single-source deployment of another logical action stays a single source.
		other := s.registration()
		other.ActionKey = "act:single0123456789"
		if _, err := s.storeRegister(t, other); err != nil {
			t.Fatalf("a single-source deployment beside the set: %v", err)
		}
		fail("give a single source a set afterwards", q+`source_set_digest = 'sha256:`+strings.Repeat("d", 64)+`', source_commits = '[{"repositoryId":"repo_api","commit":"`+strings.Repeat("1", 40)+`"}]'::jsonb, source_commits = '[{"repositoryId":"repo_api","commit":"`+strings.Repeat("1", 40)+`"}]'::jsonb WHERE action_key = $1`, other.ActionKey)
		fail("a deployment with no source at all", q+`source_repository_url = NULL, source_commit = NULL WHERE action_key = $1`, other.ActionKey)
		fail("a single source and a set together", q+`source_set_digest = 'sha256:`+strings.Repeat("d", 64)+`', source_commits = '[{"repositoryId":"repo_api","commit":"`+strings.Repeat("1", 40)+`"}]'::jsonb WHERE action_key = $1`, other.ActionKey)
		fail("clear the digest alone", q+`source_set_digest = NULL WHERE id = $1`, string(id))
	})

	t.Run("a set names only repositories of the deployment's own project, and the record's commit is the set's", func(t *testing.T) {
		foreign := reg
		foreign.ActionKey = "act:foreignrepo012345"
		foreign.Source = recordForm(mustSetOf(t, "repo_api", "repo_other"))
		if _, err := s.storeRegister(t, foreign); err == nil {
			t.Fatal("a set naming a repository of no project of the deployment was recorded")
		}
		if n := s.count(`SELECT count(*) FROM deployments WHERE action_key = $1`, foreign.ActionKey); n != 0 {
			t.Fatalf("a refused registration left %d rows", n)
		}
		// The database also refuses a primary commit that is not one of the set's.
		if err := s.X.OwnerExecErr(`UPDATE deployments SET source_commit = $2 WHERE id = $1`, string(id), strings.Repeat("5", 40)); err == nil {
			t.Fatal("the primary commit was rewritten")
		}
	})
}

func isConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "conflict")
}

// Test_source_set_needs_the_migration: a store configured without the source
// provenance columns refuses a source set, writes nothing and leaves the earlier
// single-source behavior untouched.
func Test_source_set_needs_the_migration(t *testing.T) {
	s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
	plain, err := deliverypg.NewStore(deliverypg.Config{DB: s.X.Runtime, Events: deliverypg.EventsFrom(s.X.Events), Guard: s.Guard, Clock: s.X.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	reg := s.registration()
	reg.Source = recordForm(twoRepositories(t))
	d, err := delivery.NewDeployment(reg, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = plain.Register(context.Background(), delivery.RegisterCommand{
		Mutation: delivery.Mutation{Actor: delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Engineer}, Now: time.Now().UTC().Truncate(time.Microsecond)}, Deployment: d, Attempt: reg.Attempt})
	is(t, err, delivery.ErrUnavailable)
	if s.deployments() != 0 {
		t.Fatalf("a refused source set left %d records", s.deployments())
	}
	// The single source of the same store still registers and reads back.
	single := s.registration()
	sd, err := delivery.NewDeployment(single, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	got, err := plain.Register(context.Background(), delivery.RegisterCommand{
		Mutation: delivery.Mutation{Actor: delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Engineer}, Now: time.Now().UTC().Truncate(time.Microsecond)}, Deployment: sd, Attempt: single.Attempt})
	if err != nil || !got.Created || got.Deployment.Record().Source != single.Source {
		t.Fatalf("single source on a store without the columns: %+v %v", got, err)
	}
}
