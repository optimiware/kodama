package postgres_test

import (
	"sync"
	"testing"
)

const widgetA1 = createA1 + "/"

func Test_stale_edit(t *testing.T) {
	e := newEnv(t)
	created := e.create("alice", "tenant-a", "pa1", "k", `{"name":"v1"}`)
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	id := created.widget(t).ID
	path := widgetA1 + id
	versionOf := func() (v int64) {
		t.Helper()
		if err := e.mig.QueryRow(testCtx(t), `SELECT version FROM t004_widgets WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Run("two writers with one ETag produce one update and one 412", func(t *testing.T) {
		const writers = 6
		results := make([]result, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = e.call("PATCH", path, "alice", `{"name":"writer`+string(rune('a'+i))+`"}`, "If-Match", `"1"`)
			}()
		}
		close(start)
		wg.Wait()
		okCount, stale := 0, 0
		for _, r := range results {
			switch {
			case r.Code == 200:
				okCount++
				if r.Header.Get("ETag") != `"2"` || r.widget(t).Version != 2 {
					t.Errorf("winner: %v %s", r.Header, r.Body)
				}
			case r.Code == 412 && r.errorCode(t) == "version_conflict":
				stale++
			default:
				t.Errorf("unexpected: %d %s", r.Code, r.Body)
			}
		}
		if okCount != 1 || stale != writers-1 {
			t.Fatalf("ok=%d stale=%d", okCount, stale)
		}
		if versionOf() != 2 || e.history(t) != 2 {
			t.Fatalf("version=%d history=%d, want 2 and 2", versionOf(), e.history(t))
		}
	})

	t.Run("sequential stale edit is rejected and changes nothing", func(t *testing.T) {
		beforeHistory := e.history(t)
		r := e.call("PATCH", path, "alice", `{"name":"late"}`, "If-Match", `"1"`)
		if r.Code != 412 || r.errorCode(t) != "version_conflict" {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		var name string
		if err := e.mig.QueryRow(testCtx(t), `SELECT name FROM t004_widgets WHERE id = $1`, id).Scan(&name); err != nil || name == "late" {
			t.Fatalf("stale edit changed the row: %q %v", name, err)
		}
		if versionOf() != 2 || e.history(t) != beforeHistory {
			t.Fatalf("version=%d history=%d", versionOf(), e.history(t))
		}
	})

	t.Run("the current ETag succeeds", func(t *testing.T) {
		r := e.call("PATCH", path, "alice", `{"name":"current"}`, "If-Match", `"2"`)
		if r.Code != 200 || r.widget(t).Version != 3 || r.Header.Get("ETag") != `"3"` {
			t.Fatalf("%d %v %s", r.Code, r.Header, r.Body)
		}
	})

	t.Run("missing version is 428 and malformed is 400, both without effect", func(t *testing.T) {
		v, h := versionOf(), e.history(t)
		if r := e.call("PATCH", path, "alice", `{"name":"x"}`); r.Code != 428 || r.errorCode(t) != "version_required" {
			t.Fatalf("missing: %d %s", r.Code, r.Body)
		}
		for _, bad := range []string{`3`, `W/"3"`, `*`, `"3", "4"`} {
			if r := e.call("PATCH", path, "alice", `{"name":"x"}`, "If-Match", bad); r.Code != 400 {
				t.Errorf("If-Match %s: %d %s", bad, r.Code, r.Body)
			}
		}
		if versionOf() != v || e.history(t) != h {
			t.Fatalf("rejected mutations changed state: version=%d history=%d", versionOf(), e.history(t))
		}
	})

	t.Run("another tenant's or project's resource is not found, not conflicting", func(t *testing.T) {
		r := e.call("PATCH", "/api/v1/tenants/tenant-b/projects/pb1/widgets/"+id, "alice", `{"name":"x"}`, "If-Match", `"3"`)
		if r.Code != 404 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		r = e.call("PATCH", "/api/v1/tenants/tenant-a/projects/pa2/widgets/"+id, "alice", `{"name":"x"}`, "If-Match", `"3"`)
		if r.Code != 404 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
	})
}
