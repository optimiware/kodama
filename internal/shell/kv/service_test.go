package kv_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/kv"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type seqIDs struct {
	mu   sync.Mutex
	next int
}

func (s *seqIDs) NewEventID() (id.EventID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return id.EventID(fmt.Sprintf("evt-kv-%d", s.next)), nil
}

// fakeStore is a minimal capability-local Executor + Scanner fake so
// internal/shell/kv unit tests stay free of concrete adapter imports.
type fakeStore struct {
	mu            sync.Mutex
	now           func() time.Time
	rows          map[string]map[string]map[string]any // table -> id -> row values
	beforeExecute func(scope tenancy.Scope, unit store.Unit)
}

func newFakeStore(now func() time.Time) *fakeStore {
	return &fakeStore{
		now:  now,
		rows: make(map[string]map[string]map[string]any),
	}
}

func (s *fakeStore) visible(scope tenancy.Scope, table, rowID string, tenantWide bool) (map[string]any, bool) {
	byID := s.rows[table]
	if byID == nil {
		return nil, false
	}
	values, ok := byID[rowID]
	if !ok {
		return nil, false
	}
	if tenant, has := values["tenant_id"].(string); has && tenant != string(scope.TenantID) {
		return nil, false
	}
	if proj, hasProj := values["project_id"]; hasProj {
		if tenantWide || scope.TenantScoped() {
			if proj != nil && proj != "" {
				return nil, false
			}
		} else {
			if ps, ok := proj.(string); !ok || ps != string(scope.ProjectID) {
				return nil, false
			}
		}
	}
	return values, true
}

func (s *fakeStore) Read(_ context.Context, scope tenancy.Scope, table, rowID string) (store.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, ok := s.visible(scope, table, rowID, false)
	if !ok {
		return store.Row{}, fault.New(fault.NotFound, "row not found in scope")
	}
	return store.Row{Table: table, Values: maps.Clone(values)}, nil
}

func (s *fakeStore) Query(_ context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Row
	for rowID := range s.rows[query.Table] {
		values, ok := s.visible(scope, query.Table, rowID, false)
		if !ok {
			continue
		}
		matched := true
		for col, want := range query.Equals {
			if values[col] != want {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, store.Row{Table: query.Table, Values: maps.Clone(values)})
		}
	}
	return store.QueryPage{Rows: out}, nil
}

func (s *fakeStore) Execute(_ context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	s.mu.Lock()
	hook := s.beforeExecute
	s.mu.Unlock()
	if hook != nil {
		hook(scope, unit)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lock := range unit.Locks {
		if lock.ExpectAbsent {
			for _, values := range s.rows[lock.Table] {
				matched := true
				for col, want := range lock.Key {
					if values[col] != want {
						matched = false
						break
					}
				}
				if matched {
					return store.UnitResult{}, fault.New(fault.VersionConflict, "absence guard failed")
				}
			}
			continue
		}
		for _, rowID := range lock.IDs {
			values, ok := s.visible(scope, lock.Table, rowID, lock.TenantWide)
			if !ok {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "locked row missing")
			}
			if lock.ExpectVersion != 0 {
				var current uint64
				switch v := values["version"].(type) {
				case int64:
					current = uint64(v)
				case uint64:
					current = v
				}
				if current != lock.ExpectVersion {
					return store.UnitResult{}, fault.New(fault.VersionConflict, "locked row version mismatch")
				}
			}
			if lock.Deadline != nil {
				stamp, ok := values[lock.Deadline.Column].(time.Time)
				if !ok || !stamp.After(s.now()) {
					return store.UnitResult{}, fault.New(fault.DeadlineExpired, "deadline expired")
				}
			}
		}
	}
	for _, write := range unit.Writes {
		if s.rows[write.Table] == nil {
			s.rows[write.Table] = make(map[string]map[string]any)
		}
		if write.Insert {
			if _, exists := s.rows[write.Table][write.ID]; exists {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "row already exists")
			}
			row := maps.Clone(write.Values)
			row["id"] = write.ID
			row["tenant_id"] = string(scope.TenantID)
			if scope.ProjectScoped() {
				row["project_id"] = string(scope.ProjectID)
			} else {
				row["project_id"] = nil
			}
			s.rows[write.Table][write.ID] = row
		}
	}
	return store.UnitResult{}, nil
}

type kvFixture struct {
	now   time.Time
	store *fakeStore
	svc   *kv.Service
	scope tenancy.Scope
}

func newKVFixture(t *testing.T) *kvFixture {
	t.Helper()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	fs := newFakeStore(func() time.Time { return now })
	svc, err := kv.NewService(kv.Config{
		Clock:    fixedClock{now: now},
		IDs:      &seqIDs{},
		Executor: fs,
		Scanner:  fs,
	})
	if err != nil {
		t.Fatalf("kv.NewService: %v", err)
	}
	return &kvFixture{
		now:   now,
		store: fs,
		svc:   svc,
		scope: tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"},
	}
}

func (f *kvFixture) seedHuman(t *testing.T, scope tenancy.Scope, rowID string, principalID id.PrincipalID, roles ...tenancy.Role) identity.Principal {
	t.Helper()
	rawRoles, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "memberships",
			ID:     rowID,
			Insert: true,
			Values: map[string]any{
				"principal_id": string(principalID),
				"roles":        rawRoles,
				"version":      int64(1),
				"created_at":   f.now,
			},
		}},
	}); err != nil {
		t.Fatalf("seed membership %s: %v", principalID, err)
	}
	return identity.Principal{
		ID:      principalID,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}

func (f *kvFixture) seedService(t *testing.T, scope tenancy.Scope, serviceID id.PrincipalID, state string, roles ...tenancy.Role) identity.Principal {
	t.Helper()
	if roles == nil {
		roles = []tenancy.Role{}
	}
	rawRoles, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal service roles: %v", err)
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "services",
			ID:     string(serviceID),
			Insert: true,
			Values: map[string]any{
				"name":       string(serviceID),
				"state":      state,
				"roles":      rawRoles,
				"version":    int64(1),
				"created_at": f.now,
			},
		}},
	}); err != nil {
		t.Fatalf("seed service %s: %v", serviceID, err)
	}
	return identity.Principal{
		ID:      serviceID,
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   roles,
		Enabled: state == "enabled",
		Version: 1,
	}
}

func (f *kvFixture) seedExecution(t *testing.T, scope tenancy.Scope, execID id.ExecutionID, actionID id.ActionID, principalID id.PrincipalID, execState, actionState string, deadline time.Time) identity.Principal {
	t.Helper()
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{
			{
				Table:  "executions",
				ID:     string(execID),
				Insert: true,
				Values: map[string]any{
					"principal_id": string(principalID),
					"state":        execState,
					"version":      int64(1),
					"yaml_sha256":  bytes.Repeat([]byte{1}, 32),
					"snapshot":     []byte(`{}`),
					"created_at":   f.now,
				},
			},
			{
				Table:  "actions",
				ID:     string(actionID),
				Insert: true,
				Values: map[string]any{
					"execution_id":       string(execID),
					"index":              int64(0),
					"name":               "step",
					"kind":               "container",
					"state":              actionState,
					"version":            int64(1),
					"timeout_ms":         int64(60000),
					"deadline":           deadline,
					"staged_outputs_set": false,
					"launch_state":       "issued",
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed execution %s: %v", execID, err)
	}
	return identity.Principal{
		ID:          principalID,
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: execID,
	}
}

func TestParseKeyValidation(t *testing.T) {
	validHex := strings.Repeat("a1", 32)
	validKey := "kv:sha256:" + validHex
	parsed, err := kv.ParseKey(validKey)
	if err != nil {
		t.Fatalf("ParseKey(%q): %v", validKey, err)
	}
	if string(parsed) != validKey {
		t.Fatalf("parsed = %q; want %q", parsed, validKey)
	}

	for _, bad := range []string{
		"",
		"sha256:" + validHex,
		"kv:sha256:" + strings.ToUpper(validHex),
		"kv:sha256:" + validHex[:63],
		"kv:sha256:" + validHex + "0",
		"kv:sha256:../" + validHex[3:],
		"kv:sha256:" + strings.Repeat("g", 64),
		"../etc/passwd",
		"kv:sha256:" + validHex + "\n",
	} {
		if _, err := kv.ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) succeeded; want invalid_input", bad)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("ParseKey(%q) class = %v; want invalid_input", bad, class)
		}
	}
}

func TestPutGetContentAddressingIdempotencyAndBounds(t *testing.T) {
	f := newKVFixture(t)
	ctx := context.Background()
	eng := f.seedHuman(t, f.scope, "mem-eng", "human-eng", tenancy.RoleEngineer)

	payload := []byte("# Implementation Plan\n\nStep 1: ship KV store.\n")
	sum := sha256.Sum256(payload)
	wantKey := kv.Key("kv:sha256:" + hex.EncodeToString(sum[:]))

	first, err := f.svc.Put(ctx, kv.PutCommand{
		Scope:       f.scope,
		Actor:       eng,
		Data:        payload,
		ContentType: "text/markdown; charset=utf-8",
	})
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if first.Existed {
		t.Fatalf("first Put Existed = true; want false")
	}
	if first.Metadata.Key != wantKey || first.Metadata.SizeBytes != int64(len(payload)) || first.Metadata.CreatedBy != eng.ID {
		t.Fatalf("first Put metadata = %+v; want key=%s size=%d", first.Metadata, wantKey, len(payload))
	}

	// Re-putting identical bytes in the same project converges idempotently
	// and never mutates the original record metadata.
	second, err := f.svc.Put(ctx, kv.PutCommand{
		Scope:       f.scope,
		Actor:       eng,
		Data:        payload,
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if !second.Existed {
		t.Fatalf("second Put Existed = false; want true")
	}
	if second.Metadata.Key != wantKey || second.Metadata.ContentType != first.Metadata.ContentType {
		t.Fatalf("second Put mutated existing metadata: %+v vs %+v", second.Metadata, first.Metadata)
	}

	got, err := f.svc.Get(ctx, kv.GetCommand{Scope: f.scope, Actor: eng, Key: wantKey})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.Data, payload) {
		t.Fatalf("Get data mismatch: got %q; want %q", got.Data, payload)
	}

	// Empty payload is allowed and content-addressed.
	emptyRes, err := f.svc.Put(ctx, kv.PutCommand{Scope: f.scope, Actor: eng, Data: []byte{}})
	if err != nil {
		t.Fatalf("empty Put: %v", err)
	}
	if emptyRes.Metadata.SizeBytes != 0 || emptyRes.Metadata.Key != kv.KeyFor(nil) {
		t.Fatalf("empty Put metadata = %+v", emptyRes.Metadata)
	}
	emptyGot, err := f.svc.Get(ctx, kv.GetCommand{Scope: f.scope, Actor: eng, Key: emptyRes.Metadata.Key})
	if err != nil || len(emptyGot.Data) != 0 {
		t.Fatalf("empty Get = (%d bytes, %v)", len(emptyGot.Data), err)
	}

	// Exact 16 MiB boundary succeeds; 16 MiB + 1 byte is refused with invalid_input.
	atBound := bytes.Repeat([]byte("a"), kv.MaxValueBytes)
	atBoundRes, err := f.svc.Put(ctx, kv.PutCommand{Scope: f.scope, Actor: eng, Data: atBound})
	if err != nil {
		t.Fatalf("16 MiB Put refused: %v", err)
	}
	if atBoundRes.Metadata.SizeBytes != kv.MaxValueBytes {
		t.Fatalf("16 MiB size = %d; want %d", atBoundRes.Metadata.SizeBytes, kv.MaxValueBytes)
	}

	overBound := make([]byte, kv.MaxValueBytes+1)
	if _, err := f.svc.Put(ctx, kv.PutCommand{Scope: f.scope, Actor: eng, Data: overBound}); err == nil {
		t.Fatal("oversized Put (>16 MiB) succeeded; want invalid_input")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("oversized Put class = %v; want invalid_input", class)
	}
}

func TestTenantAndProjectIsolation(t *testing.T) {
	f := newKVFixture(t)
	ctx := context.Background()

	scopeA := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	scopeB := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-b"}
	scopeOtherTenant := tenancy.Scope{TenantID: "tenant-b", ProjectID: "project-tb"}

	engA := f.seedHuman(t, scopeA, "mem-a", "human-a", tenancy.RoleEngineer)
	engB := f.seedHuman(t, scopeB, "mem-b", "human-b", tenancy.RoleEngineer)
	engOtherTenant := f.seedHuman(t, scopeOtherTenant, "mem-tb", "human-tb", tenancy.RoleEngineer)

	payload := []byte("confidential project A plan")
	putA, err := f.svc.Put(ctx, kv.PutCommand{Scope: scopeA, Actor: engA, Data: payload})
	if err != nil {
		t.Fatalf("Put in project A: %v", err)
	}

	// Project B in the same tenant MUST NOT see Project A's key.
	if _, err := f.svc.Get(ctx, kv.GetCommand{Scope: scopeB, Actor: engB, Key: putA.Metadata.Key}); err == nil {
		t.Fatal("Project B read Project A's KV key")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("Project B Get class = %v; want not_found", class)
	}

	// Tenant B MUST NOT see Tenant A's key.
	if _, err := f.svc.Get(ctx, kv.GetCommand{Scope: scopeOtherTenant, Actor: engOtherTenant, Key: putA.Metadata.Key}); err == nil {
		t.Fatal("Tenant B read Tenant A's KV key")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("Tenant B Get class = %v; want not_found", class)
	}

	// Putting identical bytes in Project B stores an independent record (existed=false!).
	putB, err := f.svc.Put(ctx, kv.PutCommand{Scope: scopeB, Actor: engB, Data: payload})
	if err != nil {
		t.Fatalf("Put identical bytes in project B: %v", err)
	}
	if putB.Existed {
		t.Fatal("Project B Put reported Existed=true from Project A's object; cross-project existence leaked!")
	}
	if putB.Metadata.Key != putA.Metadata.Key {
		t.Fatalf("identical content produced different keys: %s vs %s", putB.Metadata.Key, putA.Metadata.Key)
	}
}

func TestAuthorizationMatrix(t *testing.T) {
	f := newKVFixture(t)
	ctx := context.Background()

	eng := f.seedHuman(t, f.scope, "m-eng", "h-eng", tenancy.RoleEngineer)
	op := f.seedHuman(t, f.scope, "m-op", "h-op", tenancy.RoleOperator)
	rev := f.seedHuman(t, f.scope, "m-rev", "h-rev", tenancy.RoleReviewer)
	viewer := f.seedHuman(t, f.scope, "m-view", "h-view", tenancy.RoleViewer)
	admin := f.seedHuman(t, f.scope, "m-adm", "h-adm", tenancy.RoleAdmin)
	nonMember := identity.Principal{ID: "h-outsider", Kind: identity.KindHuman, Scope: f.scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1}

	svcRoleless := f.seedService(t, f.scope, "svc-opencode", "enabled")
	svcDisabled := f.seedService(t, f.scope, "svc-disabled", "disabled", tenancy.RoleEngineer)

	execActive := f.seedExecution(t, f.scope, "exec-active", "act-active", "exec-p-1", "running", "running", f.now.Add(time.Hour))
	execFinished := f.seedExecution(t, f.scope, "exec-done", "act-done", "exec-p-2", "succeeded", "succeeded", f.now.Add(time.Hour))
	execExpired := f.seedExecution(t, f.scope, "exec-exp", "act-exp", "exec-p-3", "running", "running", f.now.Add(-time.Second))

	allowedPut := []struct {
		name  string
		actor identity.Principal
	}{
		{"engineer", eng},
		{"operator", op},
		{"reviewer", rev},
		{"roleless service", svcRoleless},
		{"active execution", execActive},
	}
	var seededKey kv.Key
	for _, tc := range allowedPut {
		res, err := f.svc.Put(ctx, kv.PutCommand{
			Scope: f.scope,
			Actor: tc.actor,
			Data:  []byte("payload-from-" + tc.name),
		})
		if err != nil {
			t.Fatalf("Put as %s failed: %v", tc.name, err)
		}
		seededKey = res.Metadata.Key
	}

	deniedPut := []struct {
		name      string
		actor     identity.Principal
		wantClass fault.Class
	}{
		{"viewer", viewer, fault.Forbidden},
		{"pure admin", admin, fault.Forbidden},
		{"non-member", nonMember, fault.Forbidden},
		{"disabled service", svcDisabled, fault.Forbidden},
		{"finished execution", execFinished, fault.Forbidden},
		{"expired execution", execExpired, fault.DeadlineExpired},
	}
	for _, tc := range deniedPut {
		if _, err := f.svc.Put(ctx, kv.PutCommand{Scope: f.scope, Actor: tc.actor, Data: []byte("denied")}); err == nil {
			t.Errorf("Put as %s succeeded; want %s", tc.name, tc.wantClass)
		} else if class, ok := fault.ClassOf(err); !ok || class != tc.wantClass {
			t.Errorf("Put as %s class = %v (%v); want %s", tc.name, class, err, tc.wantClass)
		}
	}

	for _, tc := range []struct {
		name  string
		actor identity.Principal
	}{
		{"engineer", eng},
		{"operator", op},
		{"reviewer", rev},
		{"viewer", viewer},
		{"admin", admin},
		{"roleless service", svcRoleless},
		{"active execution", execActive},
	} {
		if _, err := f.svc.Get(ctx, kv.GetCommand{Scope: f.scope, Actor: tc.actor, Key: seededKey}); err != nil {
			t.Errorf("Get as %s failed: %v", tc.name, err)
		}
	}

	for _, tc := range []struct {
		name      string
		actor     identity.Principal
		wantClass fault.Class
	}{
		{"non-member", nonMember, fault.Forbidden},
		{"disabled service", svcDisabled, fault.Forbidden},
		{"finished execution", execFinished, fault.Forbidden},
		{"expired execution", execExpired, fault.DeadlineExpired},
	} {
		if _, err := f.svc.Get(ctx, kv.GetCommand{Scope: f.scope, Actor: tc.actor, Key: seededKey}); err == nil {
			t.Errorf("Get as %s succeeded; want %s", tc.name, tc.wantClass)
		} else if class, ok := fault.ClassOf(err); !ok || class != tc.wantClass {
			t.Errorf("Get as %s class = %v (%v); want %s", tc.name, class, err, tc.wantClass)
		}
	}
}

type funcClock func() time.Time

func (c funcClock) Now() time.Time { return c() }

func TestPutRaceConvergence(t *testing.T) {
	t.Run("concurrent identical writer converges preserving original metadata", func(t *testing.T) {
		ctx := context.Background()
		t1 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
		t2 := t1.Add(5 * time.Minute)
		var clockMu sync.Mutex
		current := t2
		clock := funcClock(func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return current
		})
		fs := newFakeStore(clock)
		svc, err := kv.NewService(kv.Config{
			Clock:    clock,
			IDs:      &seqIDs{},
			Executor: fs,
			Scanner:  fs,
		})
		if err != nil {
			t.Fatalf("kv.NewService: %v", err)
		}
		f := &kvFixture{
			now:   t1,
			store: fs,
			svc:   svc,
			scope: tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"},
		}
		winner := f.seedHuman(t, f.scope, "mem-winner", "human-winner", tenancy.RoleEngineer)
		loser := f.seedHuman(t, f.scope, "mem-loser", "human-loser", tenancy.RoleEngineer)

		payload := []byte("deterministic race payload")
		wantKey := kv.KeyFor(payload)

		// Right before loser's kv_objects insert executes (after loser already
		// passed readExisting -> NotFound), winner commits the identical bytes
		// at t1 with a distinct content type.
		injected := false
		fs.beforeExecute = func(_ tenancy.Scope, unit store.Unit) {
			if injected || len(unit.Writes) == 0 || unit.Writes[0].Table != store.KVObjectsTable {
				return
			}
			injected = true
			clockMu.Lock()
			current = t1
			clockMu.Unlock()
			winRes, winErr := svc.Put(ctx, kv.PutCommand{
				Scope:       f.scope,
				Actor:       winner,
				Data:        payload,
				ContentType: "text/markdown; charset=utf-8",
			})
			clockMu.Lock()
			current = t2
			clockMu.Unlock()
			if winErr != nil {
				t.Errorf("winner concurrent Put failed: %v", winErr)
			}
			if winRes.Existed {
				t.Errorf("winner concurrent Put reported Existed=true; want false")
			}
		}

		res, err := svc.Put(ctx, kv.PutCommand{
			Scope:       f.scope,
			Actor:       loser,
			Data:        payload,
			ContentType: "application/octet-stream",
		})
		if err != nil {
			t.Fatalf("loser Put failed instead of converging: %v", err)
		}
		if !injected {
			t.Fatal("expected race hook to fire")
		}
		if !res.Existed {
			t.Fatalf("converged Put Existed = false; want true")
		}
		if res.Metadata.Key != wantKey {
			t.Fatalf("converged Key = %s; want %s", res.Metadata.Key, wantKey)
		}
		if res.Metadata.CreatedBy != winner.ID {
			t.Fatalf("converged CreatedBy = %s; want original %s", res.Metadata.CreatedBy, winner.ID)
		}
		if !res.Metadata.CreatedAt.Equal(t1) {
			t.Fatalf("converged CreatedAt = %v; want original %v", res.Metadata.CreatedAt, t1)
		}
		if res.Metadata.ContentType != "text/markdown; charset=utf-8" {
			t.Fatalf("converged ContentType = %q; want original text/markdown; charset=utf-8", res.Metadata.ContentType)
		}
	})

	t.Run("revoked caller hitting conflict still receives forbidden", func(t *testing.T) {
		ctx := context.Background()
		f := newKVFixture(t)
		winner := f.seedHuman(t, f.scope, "mem-winner", "human-winner", tenancy.RoleEngineer)
		revoked := f.seedHuman(t, f.scope, "mem-revoked", "human-revoked", tenancy.RoleEngineer)

		payload := []byte("revoked caller race payload")
		injected := false
		f.store.beforeExecute = func(_ tenancy.Scope, unit store.Unit) {
			if injected || len(unit.Writes) == 0 || unit.Writes[0].Table != store.KVObjectsTable {
				return
			}
			injected = true
			// Winner commits the identical object, and simultaneously the
			// racing caller's membership is revoked before its insert runs.
			if _, err := f.svc.Put(ctx, kv.PutCommand{
				Scope:       f.scope,
				Actor:       winner,
				Data:        payload,
				ContentType: "text/plain; charset=utf-8",
			}); err != nil {
				t.Errorf("winner Put failed: %v", err)
			}
			f.store.mu.Lock()
			delete(f.store.rows["memberships"], "mem-revoked")
			f.store.mu.Unlock()
		}

		if _, err := f.svc.Put(ctx, kv.PutCommand{
			Scope: f.scope,
			Actor: revoked,
			Data:  payload,
		}); err == nil {
			t.Fatal("revoked caller Put converged instead of returning forbidden")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("revoked caller Put class = %v (%v); want forbidden", class, err)
		}
		if !injected {
			t.Fatal("expected race hook to fire")
		}
	})

	t.Run("unrelated version conflict with key still absent returns version_conflict", func(t *testing.T) {
		ctx := context.Background()
		f := newKVFixture(t)
		eng := f.seedHuman(t, f.scope, "mem-eng", "human-eng", tenancy.RoleEngineer)

		payload := []byte("unrelated conflict payload")
		injected := false
		f.store.beforeExecute = func(_ tenancy.Scope, unit store.Unit) {
			if injected || len(unit.Writes) == 0 || unit.Writes[0].Table != store.KVObjectsTable {
				return
			}
			injected = true
			// Bump the locked membership row's version without inserting any KV
			// object so Execute fails with VersionConflict while the key stays
			// absent.
			f.store.mu.Lock()
			f.store.rows["memberships"]["mem-eng"]["version"] = int64(2)
			f.store.mu.Unlock()
		}

		if _, err := f.svc.Put(ctx, kv.PutCommand{
			Scope: f.scope,
			Actor: eng,
			Data:  payload,
		}); err == nil {
			t.Fatal("unrelated VersionConflict Put succeeded; want version_conflict")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("unrelated VersionConflict Put class = %v (%v); want version_conflict", class, err)
		}
		if !injected {
			t.Fatal("expected race hook to fire")
		}

		// Confirm the key is still absent.
		if _, err := f.svc.Get(ctx, kv.GetCommand{
			Scope: f.scope,
			Actor: eng,
			Key:   kv.KeyFor(payload),
		}); err == nil {
			t.Fatal("key unexpectedly exists after unrelated VersionConflict")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("Get after unrelated VersionConflict class = %v; want not_found", class)
		}
	})
}
