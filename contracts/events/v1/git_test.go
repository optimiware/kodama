package eventsv1

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

var gitTypes = []string{TypeRepositoryCreated, TypeRepositoryStateChanged, TypeGitRefsUpdated}

func isGitType(t string) bool {
	for _, g := range gitTypes {
		if g == t {
			return true
		}
	}
	return false
}

func loadExamples(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile("fixtures/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples []map[string]any
	if err := json.Unmarshal(data, &examples); err != nil {
		t.Fatal(err)
	}
	return examples
}

func gitExample(t *testing.T, typ string) map[string]any {
	t.Helper()
	for _, ex := range loadExamples(t) {
		if ex["type"] == typ {
			return ex
		}
	}
	t.Fatalf("no example of %s", typ)
	return nil
}

// withData returns the encoding of a deep copy of an example whose data member
// was edited.
func withData(t *testing.T, ex map[string]any, edit func(d map[string]any)) []byte {
	t.Helper()
	raw, _ := json.Marshal(ex)
	var clone map[string]any
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	edit(clone["data"].(map[string]any))
	out, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Test_unit_events_git_types_schema is the T-126 acceptance for the additive
// repository.created, repository.state_changed and git.refs_updated event types:
// the typed Go validation and the JSON Schema agree on every fixture, and the
// envelope and the existing types stay unchanged.
func Test_unit_events_git_types_schema(t *testing.T) {
	schema := loadSchema(t)

	t.Run("types are registered on their aggregates", func(t *testing.T) {
		for typ, resource := range map[string]string{
			TypeRepositoryCreated: ResourceTypeRepository, TypeRepositoryStateChanged: ResourceTypeRepository, TypeGitRefsUpdated: ResourceTypeGitPush,
		} {
			spec, ok := Spec(typ)
			if !ok || spec.ResourceType != resource || !spec.ProjectRequired {
				t.Errorf("%s: %+v %v", typ, spec, ok)
			}
		}
		if ResourceTypeRepository != "repository" || ResourceTypeGitPush != "git_push" {
			t.Fatal("resource type")
		}
		// The types of the existing contract keep their binding.
		for typ, resource := range map[string]string{
			TypeWorkItemCreated: "work_item", TypeWorkspaceStateChanged: "workspace", TypeRunCreated: "run", TypeGateDecided: "gate",
			TypeDeploymentStateChanged: "deployment", TypeMembershipChanged: "membership", TypeRunResourceStateChanged: "run_resource", TypeUsageRecorded: "usage_record",
		} {
			if spec, ok := Spec(typ); !ok || spec.ResourceType != resource {
				t.Errorf("%s: %+v", typ, spec)
			}
		}
		if got := len(Types()); got != 20 {
			t.Errorf("%d types", got)
		}
	})

	t.Run("fixtures agree between Go and the schema", func(t *testing.T) {
		var cases []fixtureCase
		data, err := os.ReadFile("fixtures/cases.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &cases); err != nil {
			t.Fatal(err)
		}
		positives, negatives := map[string]int{}, map[string]int{}
		for _, c := range cases {
			typ, _ := c.Envelope["type"].(string)
			if !isGitType(typ) {
				continue
			}
			raw := c.bytes(t)
			env, err := Decode(raw)
			if (err == nil) != c.Valid {
				t.Errorf("%s: Decode valid=%v (%v), fixture expects %v", c.Name, err == nil, err, c.Valid)
				continue
			}
			if c.Valid {
				positives[typ]++
				if _, err := DecodeData(env); err != nil {
					t.Errorf("%s: DecodeData: %v", c.Name, err)
				}
				if !schemaVerdict(t, schema, raw) {
					t.Errorf("%s: Go accepts a payload the schema rejects", c.Name)
				}
			} else {
				negatives[typ]++
				if got := errorKind(err); got != c.Error {
					t.Errorf("%s: error kind %q, want %q", c.Name, got, c.Error)
				}
			}
			if c.SchemaValid == nil || schemaVerdict(t, schema, raw) != *c.SchemaValid {
				t.Errorf("%s: schema verdict differs from the fixture", c.Name)
			}
		}
		for _, typ := range gitTypes {
			if positives[typ] < 2 || negatives[typ] < 8 {
				t.Errorf("%s: %d positive and %d negative fixtures", typ, positives[typ], negatives[typ])
			}
		}
	})

	t.Run("examples round trip with stable digests", func(t *testing.T) {
		for _, typ := range gitTypes {
			raw, _ := json.Marshal(gitExample(t, typ))
			env, err := Decode(raw)
			if err != nil {
				t.Fatalf("%s: %v", typ, err)
			}
			enc, err := Encode(env)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Decode(enc)
			if err != nil {
				t.Fatal(err)
			}
			d1, _ := Digest(env)
			d2, _ := Digest(again)
			if d1 != d2 || !strings.HasPrefix(d1, "sha256:") {
				t.Errorf("%s: digest changed across a round trip", typ)
			}
		}
	})

	t.Run("nested and member mutations agree with the schema", func(t *testing.T) {
		mutations := map[string]func(d map[string]any, k string){
			"null":      func(d map[string]any, k string) { d[k] = nil },
			"false":     func(d map[string]any, k string) { d[k] = false },
			"removed":   func(d map[string]any, k string) { delete(d, k) },
			"empty":     func(d map[string]any, k string) { d[k] = "" },
			"emptyList": func(d map[string]any, k string) { d[k] = []any{} },
		}
		checked := 0
		for _, typ := range gitTypes {
			ex := gitExample(t, typ)
			// Each target selects the object whose members are mutated.
			targets := []func(d map[string]any) map[string]any{func(d map[string]any) map[string]any { return d }}
			if typ == TypeGitRefsUpdated {
				targets = append(targets,
					func(d map[string]any) map[string]any { return d["actor"].(map[string]any) },
					func(d map[string]any) map[string]any { return d["updates"].([]any)[0].(map[string]any) })
			}
			for ti, target := range targets {
				for key := range target(ex["data"].(map[string]any)) {
					for name, mutate := range mutations {
						raw := withData(t, ex, func(d map[string]any) { mutate(target(d), key) })
						_, derr := Decode(raw)
						if got, want := derr == nil, schemaVerdict(t, schema, raw); got != want {
							t.Errorf("%s target %d: %s %s: Go accepts=%v (%v), schema accepts=%v", typ, ti, key, name, got, derr, want)
						}
						checked++
					}
				}
			}
		}
		if checked < 80 {
			t.Fatalf("only %d mutations checked", checked)
		}
	})

	t.Run("member names are exact", func(t *testing.T) {
		ex := gitExample(t, TypeGitRefsUpdated)
		for name, edit := range map[string]func(d map[string]any){
			"Kind for kind": func(d map[string]any) {
				u := d["updates"].([]any)[0].(map[string]any)
				u["Kind"] = u["kind"]
				delete(u, "kind")
			},
			"Updates for updates":           func(d map[string]any) { d["Updates"] = d["updates"]; delete(d, "updates") },
			"ReceiveID for receiveId":       func(d map[string]any) { d["ReceiveID"] = d["receiveId"]; delete(d, "receiveId") },
			"OriginRunId for originRunId":   func(d map[string]any) { d["OriginRunId"] = d["originRunId"]; delete(d, "originRunId") },
			"RepositoryID for repositoryId": func(d map[string]any) { d["RepositoryID"] = d["repositoryId"]; delete(d, "repositoryId") },
		} {
			if _, err := Decode(withData(t, ex, edit)); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("the push is the aggregate and its resource is the receive", func(t *testing.T) {
		raw, _ := json.Marshal(gitExample(t, TypeGitRefsUpdated))
		for name, edit := range map[string]func(r map[string]any){
			"another receive":   func(r map[string]any) { r["id"] = "rcv_other" },
			"the repository":    func(r map[string]any) { r["type"], r["id"] = "repository", "repo_01" },
			"a missing version": func(r map[string]any) { r["version"] = 0 },
		} {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			edit(m["resource"].(map[string]any))
			out, _ := json.Marshal(m)
			if _, err := Decode(out); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s accepted: %v", name, err)
			}
		}
		// Pushes of one repository are separate aggregates, and the repository's
		// own events keep the repository resource.
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		a, _ := Decode(raw)
		m["resource"].(map[string]any)["id"] = "rcv_02"
		m["data"].(map[string]any)["receiveId"] = "rcv_02"
		other, _ := json.Marshal(m)
		b, err := Decode(other)
		if err != nil || a.Resource.Type != ResourceTypeGitPush || b.Resource.ID == a.Resource.ID || a.Resource.Version != b.Resource.Version {
			t.Fatalf("two pushes: %+v %+v %v", a.Resource, b.Resource, err)
		}
		if spec, _ := Spec(TypeRepositoryStateChanged); spec.ResourceType != ResourceTypeRepository {
			t.Fatalf("lifecycle events: %+v", spec)
		}
	})

	t.Run("updates are inline and validated as a payload at the contract ceiling", func(t *testing.T) {
		zero, a := strings.Repeat("0", 40), strings.Repeat("a", 40)
		updates := func(n int) []GitRefUpdate {
			out := make([]GitRefUpdate, n)
			for i := range out {
				out[i] = GitRefUpdate{Ref: "refs/heads/b" + strconv.Itoa(i), Old: zero, New: a, Kind: GitKindCreate}
			}
			return out
		}
		payload := func(u []GitRefUpdate) []byte {
			raw, _ := json.Marshal(GitRefsUpdated{ReceiveID: "rcv_01", RepositoryID: "repo_01", Actor: GitRefActor{Type: "user", ID: "usr_1"}, Updates: u})
			return raw
		}
		for _, n := range []int{1, 40, gitv1.MaxPushUpdates} {
			got, err := DecodeGitRefsUpdated(payload(updates(n)))
			if err != nil || len(got.Updates) != n || got.Updates[n-1].Ref != "refs/heads/b"+strconv.Itoa(n-1) {
				t.Errorf("%d updates: %v", n, err)
			}
		}
		for _, n := range []int{0, gitv1.MaxPushUpdates + 1} {
			if _, err := DecodeGitRefsUpdated(payload(updates(n))); !errors.Is(err, ErrInvalid) {
				t.Errorf("%d updates accepted: %v", n, err)
			}
		}
		// The envelope's data bound still holds for a large push: the payload is
		// valid, the generic envelope refuses to carry it.
		env := map[string]any{"id": "evt_1", "schemaVersion": 1, "type": TypeGitRefsUpdated, "occurredAt": "2026-10-30T10:05:00Z", "tenantId": "t_acme", "projectId": "p_inventory",
			"resource": map[string]any{"type": ResourceTypeGitPush, "id": "rcv_01", "version": 1}, "actor": map[string]any{"type": "service", "id": "svc_gitpush"},
			"correlationId": "req_01", "causationId": nil, "data": json.RawMessage(payload(updates(gitv1.MaxPushUpdates)))}
		out, _ := json.Marshal(env)
		if _, err := Decode(out); !errors.Is(err, ErrTooLarge) {
			t.Errorf("an envelope of a 1024-update push: %v", err)
		}
		// Anything but a valid payload is refused by the standalone decoder too.
		for name, bad := range map[string]string{"empty": ``, "an array": `[]`, "a legacy count": `{"receiveId":"rcv_01","repositoryId":"repo_01","actor":{"type":"user","id":"usr_1"},"originRunId":null,"updateCount":1}`} {
			if _, err := DecodeGitRefsUpdated([]byte(bad)); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})

	t.Run("the payload is derived from the record's applied entries", func(t *testing.T) {
		z, a, b, c := strings.Repeat("0", 40), strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
		reason := gitv1.ReasonGrantScope
		entry := func(ref, old, new string, kind gitv1.UpdateKind, state gitv1.EntryState) gitv1.PushEntry {
			e := gitv1.PushEntry{Ref: ref, Old: old, New: new, Kind: kind, State: state}
			if state == gitv1.EntryRefused {
				e.Reason = &reason
			}
			return e
		}
		run := "run_01"
		rec := gitv1.PushRecord{PushID: "push_01", RepositoryID: "repo_01", ReceiveID: "rcv_1", GrantID: "gg_01", CreatedAt: "2026-10-30T10:05:00Z",
			Actor:       gitv1.Actor{Type: gitv1.ActorWorkload, ID: "att_01", RunID: &run, AttemptID: &run},
			OriginRunID: &run,
			Entries: []gitv1.PushEntry{
				entry("refs/heads/a", a, b, gitv1.KindFastForward, gitv1.EntryApplied),
				entry("refs/heads/b", a, c, gitv1.KindRewrite, gitv1.EntryApplied),
				entry("refs/heads/c", z, b, gitv1.KindCreate, gitv1.EntryApplied),
				entry("refs/heads/d", a, z, gitv1.KindDelete, gitv1.EntryRefused),
				entry("refs/heads/e", a, c, gitv1.KindRewrite, gitv1.EntryRefused),
			}}
		rec.Status = gitv1.DeriveStatus(rec.Entries)
		ev, err := NewGitRefsUpdated(rec)
		if err != nil {
			t.Fatal(err)
		}
		want := []GitRefUpdate{
			{Ref: "refs/heads/a", Old: a, New: b, Kind: GitKindFastForward},
			{Ref: "refs/heads/b", Old: a, New: c, Kind: GitKindRewrite},
			{Ref: "refs/heads/c", Old: z, New: b, Kind: GitKindCreate},
		}
		if ev.ReceiveID != "rcv_1" || ev.RepositoryID != "repo_01" || ev.OriginRunID == nil || *ev.OriginRunID != "run_01" || fmt.Sprint(ev.Updates) != fmt.Sprint(want) {
			t.Fatalf("%+v", ev)
		}
		raw, _ := json.Marshal(ev)
		env := map[string]any{"id": "evt_1", "schemaVersion": 1, "type": TypeGitRefsUpdated, "occurredAt": "2026-10-30T10:05:00Z", "tenantId": "t_acme", "projectId": "p_inventory",
			"resource": map[string]any{"type": ResourceTypeGitPush, "id": "rcv_1", "version": 3}, "actor": map[string]any{"type": "service", "id": "svc_gitpush"},
			"correlationId": "req_01", "causationId": nil, "data": json.RawMessage(raw)}
		out, _ := json.Marshal(env)
		if _, err := Decode(out); err != nil {
			t.Fatalf("the derived event is invalid: %v", err)
		}
		// The updates always agree with the record: refusing one more entry
		// removes its update.
		rec.Entries[1].State, rec.Entries[1].Reason = gitv1.EntryRefused, &reason
		again, _ := NewGitRefsUpdated(rec)
		if len(again.Updates) != 2 || again.Updates[1].Ref != "refs/heads/c" {
			t.Fatalf("%+v", again)
		}
		// Only an applied record emits.
		rec.Entries[0].State = gitv1.EntryCommitting
		rec.Status = gitv1.DeriveStatus(rec.Entries)
		if _, err := NewGitRefsUpdated(rec); err == nil {
			t.Fatal("an in-flight record derived an event")
		}
		person := rec
		person.Actor = gitv1.Actor{Type: gitv1.ActorUser, ID: "usr_1"}
		if _, err := NewGitRefsUpdated(person); err == nil {
			t.Fatal("an invalid record derived an event")
		}
	})

	t.Run("patterns are shared with the git contract", func(t *testing.T) {
		for _, name := range []string{"RepositoryName", "BranchName", "RefName"} {
			if Patterns[name] != gitv1.Patterns[name] {
				t.Errorf("%s differs", name)
			}
		}
	})

	t.Run("event data carries references only", func(t *testing.T) {
		// A credential, file content or commit message is not a member of any
		// payload: an extra member is ignorable but never decoded into the type.
		raw := withData(t, gitExample(t, TypeGitRefsUpdated), func(d map[string]any) { d["password"] = "kgc_secret"; d["message"] = "wip" })
		env, err := Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		typed, err := DecodeData(env)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(typed)
		if strings.Contains(string(out), "kgc_secret") || strings.Contains(string(out), "wip") {
			t.Fatalf("typed payload carries %s", out)
		}
	})
}
