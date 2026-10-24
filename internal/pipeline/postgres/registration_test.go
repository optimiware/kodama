package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func jsonOf(t testing.TB, v pipeline.Validated) string { return string(v.Canonical()) }

func Test_duplicate_definition(t *testing.T) {
	e := newEnv(t)
	src := pipelinetest.Doc(pipelinetest.Container("a", "args: [go, test]"))

	first := e.mustRegister(scopeA, engineer, src)
	if !first.Created || first.Definition.Version != 1 {
		t.Fatalf("first registration: %+v", first)
	}

	t.Run("repeated equivalent YAML and JSON return the same definition", func(t *testing.T) {
		equivalents := map[string]pipeline.RegisterCommand{
			"same yaml": {Format: pipeline.FormatYAML, Body: []byte(src)},
			"reordered yaml": {Format: pipeline.FormatYAML, Body: []byte(`
spec:
  steps:
    - timeoutSeconds: 60
      args: [go, test]
      command: [/bin/sh, -ec]
      image: ` + pipelinetest.Image + `
      kind: container
      id: a
metadata: {name: sample}
kind: Pipeline
apiVersion: kodama/v1alpha1
`)},
			"canonical json": {Format: pipeline.FormatJSON, Body: first.Definition.Content.Canonical()},
			"compact json": {Format: pipeline.FormatJSON, Body: []byte(`{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"sample"},` +
				`"spec":{"steps":[{"id":"a","kind":"container","image":"` + pipelinetest.Image + `","command":["/bin/sh","-ec"],"args":["go","test"],"timeoutSeconds":60}]}}`)},
		}
		for name, cmd := range equivalents {
			cmd.Scope, cmd.Caller = scopeA, pipeline.Caller{PrincipalID: pipeSvc}
			res, err := e.svc.Register(context.Background(), cmd)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if res.Created || res.Definition.ID != first.Definition.ID || res.Definition.Version != 1 || res.Definition.Digest != first.Definition.Digest {
				t.Fatalf("%s: got %+v, want the first definition", name, res)
			}
		}
		if got := e.definitions(scopeA); got != 1 {
			t.Fatalf("definitions = %d", got)
		}
		if got := e.audits(scopeA); got != 1 {
			t.Fatalf("a duplicate must write no creation audit: %d rows", got)
		}
	})

	t.Run("the creation audit is safe and attributable", func(t *testing.T) {
		var actorType, actorID, action, resourceID, meta string
		var version int64
		if err := e.owner.QueryRow(context.Background(), `SELECT actor_type, actor_id, action, resource_id, resource_version, metadata::text
			FROM audit WHERE tenant_id = $1 AND resource_type = 'pipeline_definition'`, tenantA).Scan(&actorType, &actorID, &action, &resourceID, &version, &meta); err != nil {
			t.Fatal(err)
		}
		if actorType != "user" || actorID != engineer || action != "pipeline_definition.registered" || resourceID != string(first.Definition.ID) || version != 1 {
			t.Fatalf("audit row: %s %s %s %s %d", actorType, actorID, action, resourceID, version)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(meta), &m); err != nil || m["name"] != "sample" || m["digest"] != string(first.Definition.Digest) || len(m) != 3 {
			t.Fatalf("audit metadata: %v %v", m, err)
		}
		if strings.Contains(meta, "/bin/sh") {
			t.Fatal("audit metadata must not carry document content")
		}
	})

	t.Run("a service actor is audited as system", func(t *testing.T) {
		res := e.mustRegister(scopeA, pipeSvc, pipelinetest.Doc(pipelinetest.Container("svc")))
		var actorType, createdByType string
		if err := e.owner.QueryRow(context.Background(), `SELECT a.actor_type, d.created_by_type FROM audit a JOIN pipeline_definitions d
			ON d.tenant_id = a.tenant_id AND d.id = a.resource_id WHERE a.resource_id = $1`, string(res.Definition.ID)).Scan(&actorType, &createdByType); err != nil {
			t.Fatal(err)
		}
		if actorType != "system" || createdByType != "service" {
			t.Fatalf("actor mapping: audit=%s definition=%s", actorType, createdByType)
		}
	})

	t.Run("changed content creates a new immutable version (the service actor test already created version 2)", func(t *testing.T) {
		before := e.definitions(scopeA)
		changed := e.mustRegister(scopeA, engineer, pipelinetest.Doc(pipelinetest.Container("a", "args: [go, vet]")))
		if !changed.Created || changed.Definition.Version != 3 || changed.Definition.ID == first.Definition.ID || changed.Definition.Name != "sample" {
			t.Fatalf("changed registration: %+v", changed)
		}
		if e.definitions(scopeA) != before+1 {
			t.Fatal("exactly one definition must have been added")
		}
		old, err := e.svc.Get(context.Background(), scopeA, pipeline.Caller{PrincipalID: engineer}, first.Definition.ID)
		if err != nil || !bytes.Equal(old.Content.Canonical(), first.Definition.Content.Canonical()) || old.Version != 1 {
			t.Fatalf("version 1 must be untouched: %+v %v", old, err)
		}
		back := e.mustRegister(scopeA, engineer, src)
		if back.Created || back.Definition.ID != first.Definition.ID {
			t.Fatalf("returning to version 1's content resolves to version 1: %+v", back)
		}
		if e.definitions(scopeA) != before+1 {
			t.Fatal("no further version expected")
		}
	})

	t.Run("a different name versions independently", func(t *testing.T) {
		other := e.mustRegister(scopeA, engineer, pipelinetest.DocNamed("other", "{}", pipelinetest.Container("a", "args: [go, test]")))
		if !other.Created || other.Definition.Version != 1 || other.Definition.Digest == first.Definition.Digest {
			t.Fatalf("a new name starts at version 1 with its own digest: %+v", other)
		}
	})

	t.Run("stored bytes are exactly the canonical bytes", func(t *testing.T) {
		var body, digest string
		if err := e.owner.QueryRow(context.Background(), `SELECT body, digest FROM pipeline_definitions WHERE id = $1`, string(first.Definition.ID)).Scan(&body, &digest); err != nil {
			t.Fatal(err)
		}
		if body != jsonOf(t, first.Definition.Content) || digest != string(first.Definition.Digest) || pipeline.DigestOf([]byte(body)) != first.Definition.Digest {
			t.Fatal("the stored body is not the canonical bytes")
		}
	})

	t.Run("get and list return verified content", func(t *testing.T) {
		got, err := e.svc.Get(context.Background(), scopeA, pipeline.Caller{PrincipalID: engineer}, first.Definition.ID)
		if err != nil || got.ID != first.Definition.ID || got.Name != "sample" || got.CreatedBy.ID != engineer || got.CreatedBy.Type != pipeline.ActorUser {
			t.Fatalf("get: %+v %v", got, err)
		}
		if !got.CreatedAt.Equal(first.Definition.CreatedAt) {
			t.Fatalf("created_at %v != %v", got.CreatedAt, first.Definition.CreatedAt)
		}
		all, err := e.svc.List(context.Background(), scopeA, pipeline.Caller{PrincipalID: engineer}, pipeline.ListQuery{Limit: 50})
		if err != nil || len(all) != e.definitions(scopeA) {
			t.Fatalf("list: %d %v", len(all), err)
		}
		for i := 1; i < len(all); i++ {
			if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
				t.Fatal("list is not in (created_at, id) order")
			}
		}
		named, err := e.svc.List(context.Background(), scopeA, pipeline.Caller{PrincipalID: engineer}, pipeline.ListQuery{Name: "other", Limit: 50})
		if err != nil || len(named) != 1 || named[0].Name != "other" {
			t.Fatalf("name filter: %+v %v", named, err)
		}
		// Keyset pagination visits every row once.
		var seen []string
		var after *pipeline.Position
		for {
			page, err := e.svc.List(context.Background(), scopeA, pipeline.Caller{PrincipalID: engineer}, pipeline.ListQuery{Limit: 2, After: after})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range page {
				seen = append(seen, string(d.ID))
			}
			if len(page) < 2 {
				break
			}
			last := page[len(page)-1]
			after = &pipeline.Position{CreatedAt: last.CreatedAt, ID: last.ID}
		}
		if len(seen) != len(all) {
			t.Fatalf("pagination saw %d of %d rows", len(seen), len(all))
		}
		for i := range all {
			if seen[i] != string(all[i].ID) {
				t.Fatalf("pagination order differs at %d", i)
			}
		}
		if _, err := e.svc.Get(context.Background(), scopeA, pipeline.Caller{PrincipalID: engineer}, "pd_unknown"); err == nil {
			t.Fatal("an unknown definition must not be found")
		}
	})
}
