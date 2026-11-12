package recoverysupport

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/installation"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// FixtureScope is the tenant/project every durable row belongs to.
func FixtureScope() tenancy.Scope {
	return pgtest.Scope("tenant-i02f", "project-i02f")
}

// DurableSnapshot is the exact rendered state of every durable family the
// suite covers. It is produced by querying the real server, never by trusting
// the fixture values.
type DurableSnapshot struct {
	Bindings         [][]string
	Verifications    [][]string
	Pipelines        [][]string
	Conversations    [][]string
	Decisions        [][]string
	Receipts         [][]string
	QueuedExecutions [][]string
	Events           [][]string
}

// SeedDurable inserts one row in every durable family and returns the exact
// state read back from the source server.
func SeedDurable(t testing.TB, shard *pgtest.Shard) DurableSnapshot {
	t.Helper()
	ctx := t.Context()
	scope := FixtureScope()
	shard.SeedScope(ctx, string(scope.TenantID), string(scope.ProjectID))
	shard.SeedService(ctx, scope, "service-1", "service one", "engineer")
	shard.SeedResource(ctx, scope, "resource-1")
	shard.SeedBinding(ctx, scope, "binding-1", "resource-1", "service-1")
	shard.SeedVerification(ctx, scope, "verification-1", "binding-1", "resource-1")
	shard.SeedPipeline(ctx, scope, "pipeline-1", "build", []byte("name: build\nsteps: []\n"), 3, `["main"]`)
	shard.SeedTask(ctx, scope, "task-1", "open", 2, "pipeline-1")
	shard.SeedTaskMessage(ctx, scope, "message-1", "task-1", "principal-1", "please build")
	shard.SeedExecution(ctx, scope, "execution-1", "queued", 1, "task-1", "pipeline-1")
	shard.SeedActionNamed(ctx, scope, "action-1", "execution-1", "build", 0, "pending", 1)
	shard.SeedRequestAction(ctx, scope, "action-2", "execution-1", "approve", "pending", 1,
		[]byte(`{"assignment":"principal-1"}`), []byte(`{"subject":"deploy"}`),
		"pass", "principal-1", pgtest.FixtureTime)
	shard.SeedReceipt(ctx, scope, "receipt-1", "principal-1", "task.start", "caller-1", "content-1", "result-1")
	shard.SeedEvent(ctx, scope, "event-row-1", "execution.completed", "execution-1", 1)
	return SnapshotDurable(t, shard)
}

// SnapshotDurable reads the exact durable families from the server. It opens a
// fresh pool so a snapshot taken after pg_restore never reuses a plan cached
// against the pre-restore relation OIDs.
func SnapshotDurable(t testing.TB, shard *pgtest.Shard) DurableSnapshot {
	t.Helper()
	ctx := t.Context()
	db := freshDB(t, shard)
	defer db.Close()
	return DurableSnapshot{
		Bindings:         query(t, ctx, db, 4, `SELECT id, resource_id, service_id, version::text FROM resource_bindings ORDER BY id`),
		Verifications:    query(t, ctx, db, 6, `SELECT id, binding_id, resource_id, operation, result, approved::text FROM verifications ORDER BY id`),
		Pipelines:        query(t, ctx, db, 4, `SELECT id, name, encode(yaml_sha256, 'hex'), version::text FROM pipelines ORDER BY id`),
		Conversations:    query(t, ctx, db, 4, `SELECT id, task_id, author_id, body FROM task_messages ORDER BY id`),
		Decisions:        query(t, ctx, db, 3, `SELECT id, request_decision, request_actor FROM actions WHERE request_decision IS NOT NULL ORDER BY id`),
		Receipts:         query(t, ctx, db, 4, `SELECT id, operation, caller_key, result FROM receipts ORDER BY id`),
		QueuedExecutions: query(t, ctx, db, 3, `SELECT id, state, version::text FROM executions WHERE state = 'queued' ORDER BY id`),
		Events:           query(t, ctx, db, 6, `SELECT id, type, subject_id, subject_version::text, publish_state, publish_sequence::text FROM events ORDER BY id`),
	}
}

// Preserved reports the durable counts the backup evidence must carry.
func Preserved(ctx context.Context, shard *pgtest.Shard) (installation.PreservedState, error) {
	db, err := shard.NewDB("postgres", 2)
	if err != nil {
		return installation.PreservedState{}, err
	}
	defer db.Close()
	receipts, err := pgtest.ScalarInt(ctx, db, `SELECT count(*) FROM receipts`)
	if err != nil {
		return installation.PreservedState{}, err
	}
	decisions, err := pgtest.ScalarInt(ctx, db, `SELECT count(*) FROM actions WHERE request_decision IS NOT NULL`)
	if err != nil {
		return installation.PreservedState{}, err
	}
	queued, err := pgtest.ScalarInt(ctx, db, `SELECT count(*) FROM executions WHERE state = 'queued'`)
	if err != nil {
		return installation.PreservedState{}, err
	}
	return installation.PreservedState{
		Receipts:         int(receipts),
		Decisions:        int(decisions),
		QueuedExecutions: int(queued),
	}, nil
}

// WriteDurableFiles creates the retained-log and keyfile trees the backup must
// capture. The key bytes are deliberately non-secret test material.
func WriteDurableFiles(t testing.TB, shard *Shard) {
	t.Helper()
	write := func(path string, data []byte) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(shard.RetainedLogsDir, "execution-1", "stdout.log"), []byte("build output line 1\nbuild output line 2\n"))
	write(filepath.Join(shard.RetainedLogsDir, "execution-1", "stderr.log"), []byte("warning line\n"))
	write(filepath.Join(shard.KeysDir, "keyfile.json"), []byte(`{"version":1,"material":"test-key-material-not-a-secret"}`))
}

// RequireDurableEqual fails unless every durable family is byte-for-byte the
// same rendered state.
func RequireDurableEqual(t testing.TB, want, got DurableSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("durable state differs after restore\nwant: %+v\ngot:  %+v", want, got)
	}
}

// ApplicationRoles is the cluster-level role set the migrations create and the
// database dump references in object ownership and grants. A restored target
// must already hold these roles: pg_dump records ownership and ACLs but never
// creates roles, so `kodama migrate` remains the bootstrap step that makes a
// restore possible.
func ApplicationRoles() []string {
	return []string{
		store.DefaultOwnerRole,
		store.DefaultRuntimeRole,
		store.ControllerRole,
		store.PublisherRole,
		store.AuthRole,
	}
}

// RoleACL reads the exact ownership and privilege rows the application roles
// hold on public relations. aclexplode renders table and column ACLs as
// canonical (object, owner, grantee, privilege, column, grantable) rows, so a
// source and a restored target can be compared without depending on ACL array
// order. Ownership rows are included because --no-owner silently makes the
// restoring administrator the owner, which no role grant repairs.
func RoleACL(t testing.TB, shard *pgtest.Shard) [][]string {
	t.Helper()
	db := freshDB(t, shard)
	defer db.Close()
	return query(t, t.Context(), db, 6, roleACLQuery(ApplicationRoles()))
}

func roleACLQuery(roles []string) string {
	quoted := make([]string, 0, len(roles))
	for _, role := range roles {
		quoted = append(quoted, "'"+role+"'")
	}
	roleList := strings.Join(quoted, ", ")
	return `
SELECT 'relation:'||n.nspname||'.'||c.relname||':'||c.relkind::text,
       pg_get_userbyid(c.relowner),
       CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END,
       a.privilege_type,
       '',
       a.is_grantable::text
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(c.relacl) a
WHERE n.nspname = 'public'
  AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
  AND (pg_get_userbyid(c.relowner) IN (` + roleList + `)
       OR (a.grantee <> 0 AND pg_get_userbyid(a.grantee) IN (` + roleList + `)))
UNION ALL
SELECT 'column:'||n.nspname||'.'||c.relname||'.'||att.attname,
       pg_get_userbyid(c.relowner),
       CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END,
       a.privilege_type,
       att.attname,
       a.is_grantable::text
FROM pg_attribute att
JOIN pg_class c ON c.oid = att.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(att.attacl) a
WHERE n.nspname = 'public'
  AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
  AND att.attnum > 0
  AND NOT att.attisdropped
  AND (pg_get_userbyid(c.relowner) IN (` + roleList + `)
       OR (a.grantee <> 0 AND pg_get_userbyid(a.grantee) IN (` + roleList + `)))
UNION ALL
SELECT 'owner:'||n.nspname||'.'||c.relname||':'||c.relkind::text,
       pg_get_userbyid(c.relowner),
       pg_get_userbyid(c.relowner),
       'OWNER',
       '',
       'false'
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
  AND pg_get_userbyid(c.relowner) IN (` + roleList + `)
ORDER BY 1, 2, 3, 4, 5, 6`
}

// RoleRows runs one query over a connection bound to the named application
// role, so a missing grant fails the read instead of succeeding through the
// administrator's privileges.
func RoleRows(t testing.TB, shard *pgtest.Shard, role string, columns int, statement string) [][]string {
	t.Helper()
	db, err := shard.NewDB(role, 2)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	defer db.Close()
	rows, err := pgtest.QueryStringsN(t.Context(), db, columns, statement)
	if err != nil {
		t.Fatalf("role %s query %q: %v", role, statement, err)
	}
	return rows
}

// RequireRoleACLEqual fails unless the restored target carries exactly the
// source's application-role ownership and privilege rows.
func RequireRoleACLEqual(t testing.TB, want, got *pgtest.Shard) {
	t.Helper()
	wantRows := RoleACL(t, want)
	gotRows := RoleACL(t, got)
	if !reflect.DeepEqual(wantRows, gotRows) {
		t.Fatalf("application-role ownership/privileges differ after restore\nwant %d rows: %v\ngot  %d rows: %v",
			len(wantRows), wantRows, len(gotRows), gotRows)
	}
}

// RequireTreesEqual fails unless the two directory trees contain exactly the
// same regular files with the same bytes.
func RequireTreesEqual(t testing.TB, wantRoot, gotRoot string) {
	t.Helper()
	want := tree(t, wantRoot)
	got := tree(t, gotRoot)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("trees differ\nwant: %v\ngot:  %v", sortedKeys(want), sortedKeys(got))
	}
	for name, wantBytes := range want {
		if gotBytes := got[name]; string(gotBytes) != string(wantBytes) {
			t.Fatalf("%s = %q; want %q", name, gotBytes, wantBytes)
		}
	}
}

// RequireTreeLacks fails if any file name or regular-file content under root
// contains needle. It proves a disposable workspace never entered the set.
func RequireTreeLacks(t testing.TB, root, needle string) {
	t.Helper()
	found := ""
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.Contains(entry.Name(), needle) {
			found = path
			return fs.SkipAll
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), needle) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	if found != "" {
		t.Fatalf("tree %s contains %q at %s", root, needle, found)
	}
}

// RequireTreeEmpty fails unless root contains no regular file.
func RequireTreeEmpty(t testing.TB, root string) {
	t.Helper()
	if files := tree(t, root); len(files) != 0 {
		t.Fatalf("tree %s contains %v; want empty", root, sortedKeys(files))
	}
}

func freshDB(t testing.TB, shard *pgtest.Shard) *pgdriver.DB {
	t.Helper()
	db, err := shard.NewDB("postgres", 2)
	if err != nil {
		t.Fatalf("open fresh admin pool: %v", err)
	}
	return db
}

func query(t testing.TB, ctx context.Context, db *pgdriver.DB, columns int, statement string) [][]string {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, db, columns, statement)
	if err != nil {
		t.Fatalf("query %q: %v", statement, err)
	}
	return rows
}

func tree(t testing.TB, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(relative)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func sortedKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
