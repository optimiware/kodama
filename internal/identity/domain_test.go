package identity

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func binding(t *testing.T, id, user string, project domain.ProjectID, role Role) Binding {
	t.Helper()
	b, err := Grant(BindingID(id), "t_acme", UserID(user), project, role, now)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoleMatrix(t *testing.T) {
	user := UserActor("u_1")
	acme := func(p domain.ProjectID) Target { return Target{TenantID: "t_acme", ProjectID: p} }

	t.Run("exactly five fixed roles", func(t *testing.T) {
		if got := Roles(); len(got) != 5 {
			t.Fatalf("roles: %v", got)
		}
		for _, bad := range []Role{"", "owner", "Admin", "superuser", "custom"} {
			if bad.Valid() {
				t.Errorf("%q is a role", bad)
			}
			if _, err := Grant("mb_1", "t_acme", "u_1", "", bad, now); !errors.Is(err, ErrInvalid) {
				t.Errorf("custom role %q granted: %v", bad, err)
			}
		}
	})

	t.Run("admin is neither reviewer nor plaintext reader", func(t *testing.T) {
		admin := []Binding{binding(t, "mb_1", "u_1", "", RoleAdmin)}
		for _, p := range []Permission{PermWorkComplete, PermGateDecide, PermWorkWork, PermWorkCreate} {
			if err := Decide(admin, user, acme("p_a"), p); !errors.Is(err, ErrDenied) {
				t.Errorf("admin %s: %v", p, err)
			}
		}
		for _, p := range []Permission{PermMembershipManage, PermProjectSettings, PermCredentialManage, PermProjectCreate} {
			if err := Decide(admin, user, acme("p_a"), p); err != nil {
				t.Errorf("admin %s: %v", p, err)
			}
		}
	})

	t.Run("no role grants secret plaintext", func(t *testing.T) {
		for _, r := range Roles() {
			for _, p := range PermissionsOf(r) {
				if strings.Contains(string(p), "plaintext") || strings.Contains(string(p), "secret") {
					t.Errorf("%s grants %s", r, p)
				}
			}
		}
		if Permission("secret:plaintext").Known() || Permission("secret:read").Known() {
			t.Fatal("a plaintext permission exists")
		}
	})

	t.Run("only reviewer completes and decides gates", func(t *testing.T) {
		for _, r := range Roles() {
			b := []Binding{binding(t, "mb_1", "u_1", "", r)}
			for _, p := range []Permission{PermWorkComplete, PermGateDecide} {
				err := Decide(b, user, acme("p_a"), p)
				if (r == RoleReviewer) != (err == nil) {
					t.Errorf("%s %s: %v", r, p, err)
				}
			}
		}
	})

	t.Run("human-only permissions refuse non-human actors", func(t *testing.T) {
		b := []Binding{binding(t, "mb_1", "u_1", "", RoleReviewer)}
		for _, typ := range []ActorType{ActorService, ActorWorkload, ActorOperator} {
			a := Actor{ID: "u_1", Type: typ}
			if err := Decide(b, a, acme("p_a"), PermWorkComplete); !errors.Is(err, ErrDenied) {
				t.Errorf("%s completed work: %v", typ, err)
			}
			if err := Decide(b, a, acme("p_a"), PermWorkRead); !errors.Is(err, ErrDenied) {
				t.Errorf("%s holds a human role: %v", typ, err)
			}
		}
	})

	t.Run("roles compose within one scope only", func(t *testing.T) {
		b := []Binding{binding(t, "mb_1", "u_1", "", RoleViewer), binding(t, "mb_2", "u_1", "p_a", RoleReviewer)}
		if err := Decide(b, user, acme("p_a"), PermWorkComplete); err != nil {
			t.Fatalf("composed: %v", err)
		}
		if err := Decide(b, user, acme("p_b"), PermWorkComplete); !errors.Is(err, ErrDenied) {
			t.Fatalf("project reviewer widened to another project: %v", err)
		}
		if err := Decide(b, user, acme("p_b"), PermWorkRead); err != nil {
			t.Fatalf("tenant viewer: %v", err)
		}
	})

	t.Run("project binding never reaches another project or tenant", func(t *testing.T) {
		b := []Binding{binding(t, "mb_1", "u_1", "p_a", RoleAdmin)}
		if err := Decide(b, user, acme("p_b"), PermMembershipManage); !errors.Is(err, ErrHidden) {
			t.Fatalf("other project: %v", err)
		}
		if err := Decide(b, user, Target{TenantID: "t_other", ProjectID: "p_a"}, PermProjectRead); !errors.Is(err, ErrHidden) {
			t.Fatalf("other tenant: %v", err)
		}
		if err := Decide(b, user, Target{TenantID: "t_acme"}, PermProjectCreate); !errors.Is(err, ErrDenied) {
			t.Fatalf("project admin created a project: %v", err)
		}
		if err := Decide(b, user, Target{TenantID: "t_acme"}, PermMembershipManage); !errors.Is(err, ErrDenied) {
			t.Fatalf("project admin administers tenant scope: %v", err)
		}
		if err := Decide(b, user, Target{TenantID: "t_acme", AnyProject: true}, PermMembershipManage); err != nil {
			t.Fatalf("any-scope: %v", err)
		}
	})

	t.Run("no binding is hidden, not denied", func(t *testing.T) {
		if err := Decide(nil, user, acme("p_a"), PermWorkRead); !errors.Is(err, ErrHidden) {
			t.Fatal(err)
		}
		b := []Binding{binding(t, "mb_1", "u_2", "", RoleAdmin)} // someone else's binding
		if err := Decide(b, user, acme("p_a"), PermProjectRead); !errors.Is(err, ErrHidden) {
			t.Fatalf("another user's binding applied: %v", err)
		}
	})

	t.Run("unknown permission denies by default", func(t *testing.T) {
		b := []Binding{binding(t, "mb_1", "u_1", "", RoleAdmin)}
		for _, p := range []Permission{"", "work_item:delete", "admin:*", "*"} {
			if err := Decide(b, user, acme("p_a"), p); !errors.Is(err, ErrDenied) {
				t.Errorf("%q: %v", p, err)
			}
		}
	})

	t.Run("work permissions equal the work module's names", func(t *testing.T) {
		for p, want := range map[Permission]string{PermWorkRead: "work_item:read", PermWorkComplete: "work_item:complete", PermWorkEditTechnical: "work_item:edit_technical"} {
			if string(p) != want {
				t.Errorf("%s != %s", p, want)
			}
		}
	})
}

func TestIdentityTransitions(t *testing.T) {
	t.Run("binding transitions", func(t *testing.T) {
		b := binding(t, "mb_1", "u_1", "p_a", RoleViewer)
		if b.Version != 1 || b.Tenant != "t_acme" || b.Project != "p_a" {
			t.Fatalf("%+v", b)
		}
		next, err := b.ChangeRole(RoleReviewer)
		if err != nil || next.Version != 2 || next.Role != RoleReviewer || next.User != b.User || next.Project != b.Project || next.Tenant != b.Tenant || next.ID != b.ID {
			t.Fatalf("%+v %v", next, err)
		}
		if _, err := b.ChangeRole(RoleViewer); !errors.Is(err, ErrNoChange) {
			t.Fatalf("same role: %v", err)
		}
		if _, err := b.ChangeRole("owner"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("custom role: %v", err)
		}
		if b.Revoke().Binding.ID != b.ID {
			t.Fatal("revocation lost the binding identity")
		}
	})

	t.Run("duplicates and invalid grants are rejected", func(t *testing.T) {
		a := binding(t, "mb_1", "u_1", "p_a", RoleViewer)
		dup := binding(t, "mb_2", "u_1", "p_a", RoleViewer)
		if err := CheckNoDuplicate(dup, []Binding{a}); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate: %v", err)
		}
		other := binding(t, "mb_3", "u_1", "", RoleViewer) // other scope
		if err := CheckNoDuplicate(other, []Binding{a}); err != nil {
			t.Fatalf("different scope flagged: %v", err)
		}
		for _, tc := range []struct{ id, user string }{{"", "u_1"}, {"mb 1", "u_1"}, {"mb_1", ""}, {"mb_1", "u/1"}} {
			if _, err := Grant(BindingID(tc.id), "t_acme", UserID(tc.user), "", RoleViewer, now); !errors.Is(err, ErrInvalid) {
				t.Errorf("%+v accepted", tc)
			}
		}
	})

	t.Run("project ownership and key are immutable", func(t *testing.T) {
		scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_a"}
		p, err := NewProject(scope, "inventory", "Inventory", nil, 0, now)
		if err != nil || p.ActiveWIPLimit != DefaultActiveWIP || p.Version != 1 || string(p.Settings) != "{}" {
			t.Fatalf("%+v %v", p, err)
		}
		name, wip := "Inventory 2", 9
		next, changed, err := p.Update(ProjectPatch{DisplayName: &name, ActiveWIPLimit: &wip, Settings: json.RawMessage(`{"region": "eu"}`)}, now.Add(time.Minute))
		if err != nil || next.Version != 2 || next.Key != p.Key || next.Scope != p.Scope ||
			!slices.Equal(changed, []string{SettingDisplayName, SettingSettings, SettingActiveWIPLimit}) {
			t.Fatalf("%+v %v %v", next, changed, err)
		}
		if _, _, err := next.Update(ProjectPatch{DisplayName: &name}, now); !errors.Is(err, ErrNoChange) {
			t.Fatalf("unchanged patch: %v", err)
		}
		if _, _, err := next.Update(ProjectPatch{Settings: json.RawMessage(`{ "region":"eu" }`)}, now); !errors.Is(err, ErrNoChange) {
			t.Fatalf("equal settings: %v", err)
		}
		bound, changed, err := next.BindingsChanged(now)
		if err != nil || bound.Version != 3 || !slices.Equal(changed, []string{SettingRoleBindings}) {
			t.Fatalf("%+v %v %v", bound, changed, err)
		}
		both, changed, err := next.Apply(ProjectPatch{DisplayName: &[]string{"x"}[0]}, true, now)
		if err != nil || both.Version != 3 || !slices.Equal(changed, []string{SettingDisplayName, SettingRoleBindings}) {
			t.Fatalf("one version step for both: %+v %v %v", both, changed, err)
		}
		if _, _, err := p.Apply(ProjectPatch{}, false, now); !errors.Is(err, ErrNoChange) {
			t.Fatalf("empty: %v", err)
		}
	})

	t.Run("project validation", func(t *testing.T) {
		scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_a"}
		bad := func(name string, f func() error) {
			if err := f(); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s accepted: %v", name, err)
			}
		}
		mk := func(key, name string, settings string, wip int) error {
			_, err := NewProject(scope, key, name, json.RawMessage(settings), wip, now)
			return err
		}
		bad("empty key", func() error { return mk("", "", "{}", 1) })
		bad("slash key", func() error { return mk("a/b", "", "{}", 1) })
		bad("long key", func() error { return mk(strings.Repeat("a", 64), "", "{}", 1) })
		bad("long name", func() error { return mk("k", strings.Repeat("é", 257), "{}", 1) })
		bad("control name", func() error { return mk("k", "a\x00b", "{}", 1) })
		bad("wip too big", func() error { return mk("k", "", "{}", 100001) })
		bad("wip negative", func() error { return mk("k", "", "{}", -1) })
		bad("array settings", func() error { return mk("k", "", "[]", 1) })
		bad("null settings", func() error { return mk("k", "", "null", 1) })
		bad("bad member", func() error { return mk("k", "", `{"1x":1}`, 1) })
		bad("trailing", func() error { return mk("k", "", `{} {}`, 1) })
		bad("too deep", func() error { return mk("k", "", `{"a":[[[[[[[1]]]]]]]}`, 1) })
		bad("too large", func() error { return mk("k", "", `{"a":"`+strings.Repeat("x", MaxSettingsBytes)+`"}`, 1) })
		if err := mk("k", "ok", `{"a":{"b":[1,2]},"c":"]"}`, 1); err != nil {
			t.Errorf("valid settings: %v", err)
		}
		if err := mk("k", "ok", `{"a":"[[[[[[[[[[[["}`, 1); err != nil {
			t.Errorf("brackets inside strings counted: %v", err)
		}
	})

	t.Run("principal validation", func(t *testing.T) {
		for _, p := range []Principal{{"", "s"}, {"i", ""}, {"i\n", "s"}, {"i", strings.Repeat("s", 513)}} {
			if p.Validate() == nil {
				t.Errorf("%+v accepted", p)
			}
		}
		if (Principal{Issuer: "https://i.example", Subject: "oauthsonas|engineer"}).Validate() != nil {
			t.Error("valid principal rejected")
		}
	})

	t.Run("reach", func(t *testing.T) {
		b := []Binding{binding(t, "mb_1", "u_1", "p_a", RoleAdmin), binding(t, "mb_2", "u_1", "p_b", RoleViewer)}
		r := ReachOf(b, "t_acme", PermMembershipManage)
		if r.All || !slices.Equal(r.Projects, []domain.ProjectID{"p_a"}) || r.Contains("p_b") || !r.Contains("p_a") {
			t.Fatalf("%+v", r)
		}
		if r := ReachOf(append(b, binding(t, "mb_3", "u_1", "", RoleAdmin)), "t_acme", PermMembershipManage); !r.All || !r.Contains("p_zzz") {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("bootstrap digest binds every parameter", func(t *testing.T) {
		p := Principal{Issuer: "i", Subject: "s"}
		base := BootstrapDigest("t_a", "acme", "Acme", p)
		for _, other := range []string{
			BootstrapDigest("t_b", "acme", "Acme", p), BootstrapDigest("t_a", "acme2", "Acme", p), BootstrapDigest("t_a", "acme", "Acme2", p),
			BootstrapDigest("t_a", "acme", "Acme", Principal{Issuer: "i2", Subject: "s"}), BootstrapDigest("t_a", "acme", "Acme", Principal{Issuer: "i", Subject: "s2"}),
			BootstrapDigest("t_a", "acm", "eAcme", p),
		} {
			if other == base {
				t.Fatal("digest collision")
			}
		}
	})
}
