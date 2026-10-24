package root_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/root"
)

func str(s string) *string { return &s }

func seed(t *testing.T, st root.ContextStore) {
	t.Helper()
	err := st.Update(context.Background(), func(s *root.ContextSet) error {
		auth := root.AuthConfig{
			Issuer: "https://idp.example.invalid", ClientID: "kodama-cli",
			AuthorizationEndpoint: "https://idp.example.invalid/auth", TokenEndpoint: "https://idp.example.invalid/token",
			RedirectURI: "http://127.0.0.1:18999/callback", Scopes: []string{"openid"},
		}
		if err := s.Define("prod", root.Context{Factory: "https://factory.example.invalid", Auth: auth, Tenant: "t017_one", Project: "p017_one"}); err != nil {
			return err
		}
		if err := s.Define("other", root.Context{Factory: "https://other.example.invalid", Auth: auth, Tenant: "t017_two"}); err != nil {
			return err
		}
		return s.Select("prod")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func resolve(t *testing.T, st root.ContextStore, o root.Overrides) (root.EffectiveContext, error) {
	t.Helper()
	return root.ResolveContext(context.Background(), st, o)
}

func Test_context_override(t *testing.T) {
	st := newMem()
	seed(t, st)
	before, _ := st.Load(context.Background())

	t.Run("saved selection applies when nothing is overridden", func(t *testing.T) {
		eff, err := resolve(t, st, root.Overrides{})
		if err != nil || eff.Name != "prod" || eff.Tenant != "t017_one" || eff.Project != "p017_one" {
			t.Fatalf("%+v %v", eff, err)
		}
	})

	t.Run("explicit flags beat the saved context", func(t *testing.T) {
		cases := []struct {
			name           string
			o              root.Overrides
			tenant, projct string
			factory        string
		}{
			{"same tenant keeps project", root.Overrides{Tenant: str("t017_one")}, "t017_one", "p017_one", "https://factory.example.invalid"},
			{"new tenant clears inherited project", root.Overrides{Tenant: str("t017_two")}, "t017_two", "", "https://factory.example.invalid"},
			{"new tenant with explicit project", root.Overrides{Tenant: str("t017_two"), Project: str("p017_x")}, "t017_two", "p017_x", "https://factory.example.invalid"},
			{"project only keeps tenant", root.Overrides{Project: str("p017_y")}, "t017_one", "p017_y", "https://factory.example.invalid"},
			{"new factory clears scope", root.Overrides{Factory: str("https://F.example.invalid/")}, "", "", "https://f.example.invalid"},
			{"new factory with explicit scope", root.Overrides{Factory: str("https://f.example.invalid"), Tenant: str("t017_z"), Project: str("p017_z")}, "t017_z", "p017_z", "https://f.example.invalid"},
			{"named context", root.Overrides{Context: str("other")}, "t017_two", "", "https://other.example.invalid"},
		}
		for _, c := range cases {
			eff, err := resolve(t, st, c.o)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if string(eff.Tenant) != c.tenant || string(eff.Project) != c.projct || eff.Factory != c.factory {
				t.Errorf("%s: got %s/%s @ %s", c.name, eff.Tenant, eff.Project, eff.Factory)
			}
		}
	})

	t.Run("explicitly empty or invalid overrides are rejected, not ignored", func(t *testing.T) {
		bad := map[string]root.Overrides{
			"empty tenant":       {Tenant: str("")},
			"blank project":      {Project: str("  ")},
			"empty factory":      {Factory: str("")},
			"empty context":      {Context: str("")},
			"empty issuer":       {Issuer: str("")},
			"empty output":       {Output: str("")},
			"bad output":         {Output: str("yaml")},
			"bad store":          {CredentialStore: str("memory")},
			"project, no tenant": {Context: str("other"), Project: str("p017_q"), Tenant: str("")},
			"bad tenant id":      {Tenant: str("bad tenant")},
			"unknown context":    {Context: str("nope")},
			"relative factory":   {Factory: str("factory.example.invalid")},
			"factory with path":  {Factory: str("https://f.example.invalid/api")},
		}
		for name, o := range bad {
			_, err := resolve(t, st, o)
			if err == nil {
				t.Errorf("%s: accepted", name)
				continue
			}
			if p := root.Classify(err); p.Exit != root.ExitUsage {
				t.Errorf("%s: exit %d (%v)", name, p.Exit, err)
			}
		}
		// a project can never exist without a tenant, even through an empty saved context
		empty := newMem()
		if _, err := resolve(t, empty, root.Overrides{Project: str("p017_only")}); err == nil {
			t.Error("project without tenant accepted")
		}
	})

	t.Run("transient overrides never persist", func(t *testing.T) {
		after, _ := st.Load(context.Background())
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("saved state changed:\n%+v\n%+v", before, after)
		}
	})

	t.Run("credentials are isolated per factory and authentication configuration", func(t *testing.T) {
		a, _ := resolve(t, st, root.Overrides{})
		same, _ := resolve(t, st, root.Overrides{Tenant: str("t017_two")})
		if a.CredentialKey() != same.CredentialKey() {
			t.Error("tenant selection must not change the credential identity")
		}
		for name, o := range map[string]root.Overrides{
			"other factory":   {Factory: str("https://other.example.invalid")},
			"other issuer":    {Issuer: str("https://idp2.example.invalid")},
			"other client":    {ClientID: str("someone-else")},
			"other token url": {TokenEndpoint: str("https://idp.example.invalid/token2")},
			"other scopes":    {Scopes: str("openid,profile")},
		} {
			b, err := resolve(t, st, o)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if a.CredentialKey() == b.CredentialKey() {
				t.Errorf("%s reuses another login's credential key", name)
			}
		}
		x, _ := resolve(t, st, root.Overrides{Scopes: str("b,a")})
		y, _ := resolve(t, st, root.Overrides{Scopes: str("a, b")})
		if x.CredentialKey() != y.CredentialKey() {
			t.Error("scope order must not matter")
		}
	})

	t.Run("commands through the runner honor precedence and persistence", func(t *testing.T) {
		fsx := fileStore(t)
		seed(t, fsx)
		r, out, errb := newRunner(t, root.RunnerConfig{Contexts: fsx}, root.NewContextCommands())

		if code := r.Run(context.Background(), []string{"context", "show", "--tenant", "t017_two", "--output", "json"}); code != 0 {
			t.Fatalf("show: %d %s", code, errb)
		}
		var v map[string]any
		if err := json.Unmarshal(out.Bytes(), &v); err != nil || v["tenant"] != "t017_two" || v["project"] != "" || v["name"] != "prod" {
			t.Fatalf("show output %s (%v)", out, err)
		}
		if got, _ := fsx.Load(context.Background()); got.Contexts["prod"].Tenant != "t017_one" {
			t.Fatal("show persisted a transient override")
		}

		out.Reset()
		errb.Reset()
		if code := r.Run(context.Background(), []string{"context", "show", "--tenant", "", "--output", "json"}); code != 2 {
			t.Fatalf("empty tenant exit %d", code)
		}
		if out.Len() != 0 || !strings.Contains(errb.String(), `"code":"usage"`) {
			t.Fatalf("stdout %q stderr %q", out, errb)
		}

		// use: explicit persisted transition
		if code := r.Run(context.Background(), []string{"context", "use", "--tenant", "t017_new", "--project", "p017_new", "--output", "json"}); code != 0 {
			t.Fatal(errb.String())
		}
		got, _ := fsx.Load(context.Background())
		if c := got.Contexts["prod"]; c.Tenant != "t017_new" || c.Project != "p017_new" || got.Current != "prod" {
			t.Fatalf("%+v", got)
		}
		if code := r.Run(context.Background(), []string{"context", "use", "--tenant", "t017_newer"}); code != 0 {
			t.Fatal(errb.String())
		}
		got, _ = fsx.Load(context.Background())
		if c := got.Contexts["prod"]; c.Tenant != "t017_newer" || c.Project != "" {
			t.Fatalf("tenant change must clear project: %+v", c)
		}
		// invalid transitions leave state unchanged
		errb.Reset()
		for _, args := range [][]string{
			{"context", "use"},
			{"context", "use", "--context", "ghost"},
			{"context", "use", "--factory", "https://x.example.invalid"},
			{"context", "set", "--context", "fresh"},
			{"context", "set", "--context", "bad name", "--factory", "https://x.example.invalid"},
		} {
			if code := r.Run(context.Background(), args); code != 2 {
				t.Errorf("%v: exit %d", args, code)
			}
		}
		again, _ := fsx.Load(context.Background())
		if !reflect.DeepEqual(got, again) {
			t.Fatal("rejected transitions changed saved state")
		}

		// set + select + list ordering
		if code := r.Run(context.Background(), []string{"context", "set", "--context", "alpha", "--factory", "https://a.example.invalid", "--select", "--output", "json"}); code != 0 {
			t.Fatal(errb.String())
		}
		out.Reset()
		if code := r.Run(context.Background(), []string{"context", "list", "--output", "json"}); code != 0 {
			t.Fatal(errb.String())
		}
		var list struct {
			Current  string `json:"current"`
			Contexts []struct {
				Name    string `json:"name"`
				Current bool   `json:"current"`
			} `json:"contexts"`
		}
		if err := json.Unmarshal(out.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range list.Contexts {
			names = append(names, c.Name)
		}
		if list.Current != "alpha" || !reflect.DeepEqual(names, []string{"alpha", "other", "prod"}) {
			t.Fatalf("%+v", list)
		}
	})
}
