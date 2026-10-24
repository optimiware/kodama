package secrets_test

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// fakeCatalog implements the three resolution ports from memory.
type fakeCatalog struct {
	vars      map[string]secrets.Variable
	secrets   map[string]int64 // name -> current version
	revoked   map[string]bool
	artifacts map[domain.ArtifactID]domain.Scope
	reads     int
}

func newCatalog() *fakeCatalog {
	return &fakeCatalog{
		vars: map[string]secrets.Variable{}, secrets: map[string]int64{}, revoked: map[string]bool{},
		artifacts: map[domain.ArtifactID]domain.Scope{},
	}
}

func (c *fakeCatalog) ActiveVariable(_ context.Context, scope domain.Scope, name string) (secrets.Variable, error) {
	c.reads++
	v, ok := c.vars[name]
	if !ok || v.Scope() != scope || v.Deleted() {
		return secrets.Variable{}, secrets.ErrNotFound
	}
	return v, nil
}

func (c *fakeCatalog) CurrentVersion(_ context.Context, _ domain.Scope, name string) (int64, error) {
	v, ok := c.secrets[name]
	if !ok {
		return 0, secrets.ErrNotFound
	}
	if c.revoked[name] {
		return 0, secrets.ErrNoActiveVersion
	}
	return v, nil
}

func (c *fakeCatalog) Owns(_ context.Context, scope domain.Scope, id domain.ArtifactID) (bool, error) {
	s, ok := c.artifacts[id]
	return ok && s == scope, nil
}

func newResolver(t *testing.T, c *fakeCatalog) *secrets.ConfigurationResolver {
	t.Helper()
	r, err := secrets.NewConfigurationResolver(c, c, c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func str(s string) *runtimev1.Value { v := runtimev1.StringValue(s); return &v }

// Test_precedence is acceptance precedence (VS-02, VS-A01): defaults, project
// variables of the declared name and explicit run inputs resolve
// deterministically into an immutable, secret-free snapshot.
func Test_precedence(t *testing.T) {
	ctx := context.Background()

	t.Run("default then project variable then run input", func(t *testing.T) {
		c := newCatalog()
		c.vars["region"] = mustVariable(t, "region", runtimev1.StringValue("eu"))
		c.vars["tier"] = mustVariable(t, "tier", runtimev1.StringValue("gold"))
		r := newResolver(t, c)
		req := secrets.ConfigurationRequest{
			Scope: scopeA,
			Inputs: []secrets.InputDecl{
				{Name: "region", Type: secrets.InputString, Default: str("us")}, // variable overrides default
				{Name: "tier", Type: secrets.InputString, Default: str("silver")},
				{Name: "mode", Type: secrets.InputString, Default: str("fast")}, // only a default
				{Name: "retries", Type: secrets.InputInteger, Default: ptr(runtimev1.IntValue(3))},
			},
			Explicit: map[string]runtimev1.Value{"tier": runtimev1.StringValue("platinum")}, // run input wins
		}
		snap, err := r.Resolve(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]struct {
			val runtimev1.Value
			src secrets.InputSource
		}{
			"region":  {runtimev1.StringValue("eu"), secrets.SourceProjectVariable},
			"tier":    {runtimev1.StringValue("platinum"), secrets.SourceRunInput},
			"mode":    {runtimev1.StringValue("fast"), secrets.SourceDefault},
			"retries": {runtimev1.IntValue(3), secrets.SourceDefault},
		}
		if len(snap.Inputs()) != len(want) {
			t.Fatalf("inputs %v", snap.Inputs())
		}
		for name, w := range want {
			got, ok := snap.Input(name)
			if !ok || got.Value != w.val || got.Source != w.src {
				t.Errorf("%s = %+v, want %v from %s", name, got, w.val, w.src)
			}
		}
		region, _ := snap.Input("region")
		if region.VariableID != "var_1" || region.VariableVersion != 1 {
			t.Errorf("pinned variable provenance %+v", region)
		}
		tier, _ := snap.Input("tier")
		if tier.VariableID != "" || tier.VariableVersion != 0 {
			t.Errorf("overridden input kept variable provenance %+v", tier)
		}

		// Deterministic: same request, same order, same digest.
		snap2, _ := r.Resolve(ctx, req)
		d1, err1 := snap.Digest()
		d2, err2 := snap2.Digest()
		if err1 != nil || err2 != nil || d1 != d2 {
			t.Fatalf("digests differ: %s %s", d1, d2)
		}
		if err := d1.Validate(); err != nil {
			t.Fatalf("digest syntax: %v", err)
		}
		names := []string{}
		for _, in := range snap.Inputs() {
			names = append(names, in.Name)
		}
		if names[0] != "mode" || names[3] != "tier" {
			t.Fatalf("inputs are not name-ordered: %v", names)
		}

		// A later variable edit does not change the immutable snapshot.
		edited := c.vars["region"]
		if _, _, err := edited.Edit(1, secrets.VariableEdit{Value: str("ap")}, t0); err != nil {
			t.Fatal(err)
		}
		c.vars["region"] = edited
		if got, _ := snap.Input("region"); got.Value != runtimev1.StringValue("eu") || got.VariableVersion != 1 {
			t.Fatal("snapshot changed after a variable edit")
		}
		// And the snapshot hands out copies.
		in := snap.Inputs()
		in[0].Name = "mutated"
		if got, _ := snap.Input("mode"); got.Name != "mode" {
			t.Fatal("snapshot aliases its internal slice")
		}
		vals := snap.Values()
		delete(vals, "mode")
		if _, ok := snap.Input("mode"); !ok {
			t.Fatal("snapshot aliases its value map")
		}
		// Re-resolving now pins the edited version 2.
		snap3, _ := r.Resolve(ctx, req)
		if got, _ := snap3.Input("region"); got.VariableVersion != 2 || got.Value != runtimev1.StringValue("ap") {
			t.Fatalf("new resolution did not pin the new version: %+v", got)
		}
		d3, _ := snap3.Digest()
		if d3 == d1 {
			t.Fatal("digest ignores the pinned value")
		}
	})

	t.Run("unrelated project variables are not implicit inputs", func(t *testing.T) {
		c := newCatalog()
		c.vars["unrelated"] = mustVariable(t, "unrelated", runtimev1.StringValue("x"))
		snap, err := newResolver(t, c).Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA})
		if err != nil || len(snap.Inputs()) != 0 {
			t.Fatalf("implicit input: %v %v", snap.Inputs(), err)
		}
		if c.reads != 0 {
			t.Fatal("undeclared variables were read")
		}
	})

	t.Run("unknown explicit names fail admission", func(t *testing.T) {
		c := newCatalog()
		_, err := newResolver(t, c).Resolve(ctx, secrets.ConfigurationRequest{
			Scope:    scopeA,
			Inputs:   []secrets.InputDecl{{Name: "a", Type: secrets.InputString, Default: str("x")}},
			Explicit: map[string]runtimev1.Value{"b": runtimev1.StringValue("y")},
		})
		if !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("unknown run input: %v", err)
		}
	})

	t.Run("type mismatches and unsupported types", func(t *testing.T) {
		c := newCatalog()
		c.vars["count"] = mustVariable(t, "count", runtimev1.StringValue("three"))
		r := newResolver(t, c)
		cases := map[string]secrets.ConfigurationRequest{
			"variable type differs from declaration": {Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "count", Type: secrets.InputInteger}}},
			"run input type differs": {Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "flag", Type: secrets.InputBoolean}},
				Explicit: map[string]runtimev1.Value{"flag": runtimev1.StringValue("true")}},
			"default type differs": {Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "n", Type: secrets.InputInteger, Default: str("1")}}},
			"unsupported type":     {Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "n", Type: "float"}}},
			"duplicate declaration": {Scope: scopeA, Inputs: []secrets.InputDecl{
				{Name: "n", Type: secrets.InputString}, {Name: "n", Type: secrets.InputString}}},
			"invalid input name":   {Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "Bad", Type: secrets.InputString}}},
			"artifact vs variable": {Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "count", Type: secrets.InputArtifact}}},
			"oversized run input": {Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "s", Type: secrets.InputString}},
				Explicit: map[string]runtimev1.Value{"s": runtimev1.StringValue(string(make([]byte, 4097)))}},
			"scope without project": {Scope: domain.Scope{TenantID: "t_acme"}},
		}
		for name, req := range cases {
			if _, err := r.Resolve(ctx, req); !errors.Is(err, secrets.ErrInvalid) && !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("required inputs", func(t *testing.T) {
		c := newCatalog()
		r := newResolver(t, c)
		decl := []secrets.InputDecl{{Name: "region", Type: secrets.InputString, Required: true}}
		if _, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Inputs: decl}); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("missing required input: %v", err)
		}
		// Supplied by a project variable, by a run input, or optional-and-absent.
		c.vars["region"] = mustVariable(t, "region", runtimev1.StringValue("eu"))
		if _, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Inputs: decl}); err != nil {
			t.Fatalf("required input from a variable: %v", err)
		}
		snap, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "opt", Type: secrets.InputString}}})
		if err != nil || len(snap.Inputs()) != 0 {
			t.Fatalf("optional absent input: %v %v", snap.Inputs(), err)
		}
	})

	t.Run("deleted variables and other projects do not apply", func(t *testing.T) {
		c := newCatalog()
		v := mustVariable(t, "region", runtimev1.StringValue("eu"))
		if _, err := v.Delete(1, t0); err != nil {
			t.Fatal(err)
		}
		c.vars["region"] = v
		snap, err := newResolver(t, c).Resolve(ctx, secrets.ConfigurationRequest{
			Scope: scopeA, Inputs: []secrets.InputDecl{{Name: "region", Type: secrets.InputString, Default: str("us")}}})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := snap.Input("region"); got.Source != secrets.SourceDefault {
			t.Fatalf("deleted variable still applies: %+v", got)
		}
		other := domain.Scope{TenantID: "t_acme", ProjectID: "p_other"}
		c.vars["region"] = mustVariable(t, "region", runtimev1.StringValue("eu"))
		snap, err = newResolver(t, c).Resolve(ctx, secrets.ConfigurationRequest{
			Scope: other, Inputs: []secrets.InputDecl{{Name: "region", Type: secrets.InputString, Default: str("us")}}})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := snap.Input("region"); got.Source != secrets.SourceDefault {
			t.Fatalf("another project's variable applies: %+v", got)
		}
	})

	t.Run("artifact ownership", func(t *testing.T) {
		c := newCatalog()
		c.artifacts["art_mine"] = scopeA
		c.artifacts["art_theirs"] = domain.Scope{TenantID: "t_globex", ProjectID: "p_billing"}
		r := newResolver(t, c)
		decl := []secrets.InputDecl{{Name: "report", Type: secrets.InputArtifact, Required: true}}
		snap, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Inputs: decl,
			Explicit: map[string]runtimev1.Value{"report": runtimev1.StringValue("art_mine")}})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := snap.Input("report"); got.Type != secrets.InputArtifact || got.Value.String() != "art_mine" {
			t.Fatalf("artifact input %+v", got)
		}
		for _, id := range []string{"art_theirs", "art_missing"} {
			_, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Inputs: decl,
				Explicit: map[string]runtimev1.Value{"report": runtimev1.StringValue(id)}})
			if !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("artifact %s: %v", id, err)
			}
		}
		if _, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Inputs: decl,
			Explicit: map[string]runtimev1.Value{"report": runtimev1.StringValue("not an id!")}}); !errors.Is(err, secrets.ErrInvalid) {
			t.Errorf("malformed artifact ID: %v", err)
		}
		// Without an ownership port an artifact input fails closed.
		noArt, _ := secrets.NewConfigurationResolver(c, c, nil)
		if _, err := noArt.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Inputs: decl,
			Explicit: map[string]runtimev1.Value{"report": runtimev1.StringValue("art_mine")}}); !errors.Is(err, secrets.ErrAuthorityMissing) {
			t.Errorf("no ownership port: %v", err)
		}
	})

	t.Run("secret references pin exact versions and carry no plaintext", func(t *testing.T) {
		c := newCatalog()
		c.secrets["erp-token"] = 4
		r := newResolver(t, c)
		req := secrets.ConfigurationRequest{Scope: scopeA, Secrets: []secrets.SecretUse{
			{Name: "erp-token", Ref: "secret://project/erp-token", Delivery: runtimev1.DeliveryFile},
			{Name: "ERP_ENV", Ref: "secret://project/erp-token", Delivery: runtimev1.DeliveryEnv},
		}}
		snap, err := r.Resolve(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		bs := snap.SecretBindings()
		if len(bs) != 2 || bs[0].Version != 4 || bs[0].Ref != "secret://project/erp-token" || bs[0].Delivery != runtimev1.DeliveryFile {
			t.Fatalf("bindings %+v", bs)
		}
		// Rotation after pinning does not change the snapshot.
		c.secrets["erp-token"] = 5
		if snap.SecretBindings()[0].Version != 4 {
			t.Fatal("pinned version changed after rotation")
		}
		snap2, _ := r.Resolve(ctx, req)
		if snap2.SecretBindings()[0].Version != 5 {
			t.Fatal("a new resolution must pin the new current version")
		}

		bad := map[string]secrets.SecretUse{
			"tenant scope":     {Name: "x", Ref: "secret://tenant/erp-token", Delivery: runtimev1.DeliveryFile},
			"unknown delivery": {Name: "x", Ref: "secret://project/erp-token", Delivery: "socket"},
			"bad binding name": {Name: "../x", Ref: "secret://project/erp-token", Delivery: runtimev1.DeliveryFile},
		}
		for name, use := range bad {
			if _, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Secrets: []secrets.SecretUse{use}}); !errors.Is(err, secrets.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := r.Resolve(ctx, secrets.ConfigurationRequest{Scope: scopeA, Secrets: []secrets.SecretUse{
			{Name: "x", Ref: "secret://project/missing", Delivery: runtimev1.DeliveryFile}}}); !errors.Is(err, secrets.ErrNotFound) {
			t.Errorf("missing secret: %v", err)
		}
		c.revoked["erp-token"] = true
		if _, err := r.Resolve(ctx, req); !errors.Is(err, secrets.ErrNoActiveVersion) {
			t.Errorf("revoked current version pinned: %v", err)
		}
		c.revoked["erp-token"] = false
		dup := secrets.ConfigurationRequest{Scope: scopeA, Secrets: []secrets.SecretUse{req.Secrets[0], req.Secrets[0]}}
		if _, err := r.Resolve(ctx, dup); !errors.Is(err, secrets.ErrInvalid) {
			t.Errorf("duplicate binding: %v", err)
		}
	})

	t.Run("resolver needs its catalogs", func(t *testing.T) {
		if _, err := secrets.NewConfigurationResolver(nil, newCatalog(), nil); err == nil {
			t.Error("resolver without a variable catalog")
		}
	})
}
