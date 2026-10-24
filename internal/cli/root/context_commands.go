package root

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

type contextView struct {
	Name    string           `json:"name"`
	Current bool             `json:"current"`
	Factory string           `json:"factory"`
	Tenant  domain.TenantID  `json:"tenant"`
	Project domain.ProjectID `json:"project"`
	Auth    AuthConfig       `json:"auth"`
}

func viewOf(name string, current bool, c Context) contextView {
	return contextView{Name: name, Current: current, Factory: c.Factory, Tenant: c.Tenant, Project: c.Project, Auth: c.Auth}
}

func (v contextView) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "context: %s\nfactory: %s\ntenant: %s\nproject: %s\n", v.Name, v.Factory, v.Tenant, v.Project)
	return b.String()
}

// NewContextCommands contributes the local `context set|use|show|list`
// commands. They read and write only the injected ContextStore.
func NewContextCommands() Contribution {
	return Contribution{
		Group:   "context",
		Summary: "manage saved factory, tenant and project contexts (local only)",
		Commands: []Command{
			{Name: "set", Summary: "create or update a named context (--context NAME; --select to make it current)", Need: NeedNone, Bind: bindSet},
			{Name: "use", Summary: "select a context and/or its tenant and project", Need: NeedNone, Bind: bindUse},
			{Name: "show", Summary: "show the effective context after applying flags", Need: NeedContext, Bind: bindShow},
			{Name: "list", Summary: "list saved contexts", Need: NeedNone, Bind: bindList},
		},
	}
}

func requireStore(inv *Invocation) (ContextStore, error) {
	if inv.Contexts == nil {
		return nil, NewError(ExitFailure, "context_store_error", "no context store is configured")
	}
	return inv.Contexts, nil
}

func storeErr(err error) error {
	switch {
	case err == nil || isUsage(err) || isCtxFailure(err):
		return err
	default:
		return NewError(ExitFailure, "context_store_error", "%v", err).WithCause(err)
	}
}

func isCtxFailure(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isUsage(err error) bool {
	e, ok := err.(*Error)
	return ok && e.Exit == ExitUsage
}

func bindSet(fs *flag.FlagSet) RunFunc {
	sel := fs.Bool("select", false, "make the context current")
	return func(ctx context.Context, inv *Invocation, args []string) (Result, error) {
		if len(args) > 0 {
			return Result{}, Usagef("unexpected argument %q", args[0])
		}
		if err := inv.Overrides.validate(); err != nil {
			return Result{}, err
		}
		store, err := requireStore(inv)
		if err != nil {
			return Result{}, err
		}
		var name string
		var saved Context
		err = store.Update(ctx, func(set *ContextSet) error {
			name = set.Current
			if inv.Overrides.Context != nil {
				name = *inv.Overrides.Context
			}
			if name == "" {
				return Usagef("no context name: pass --context NAME")
			}
			base, exists := set.Contexts[name]
			c, err := apply(base, inv.Overrides)
			if err != nil {
				return err
			}
			if !exists && c.Factory == "" {
				return Usagef("creating a context requires --factory")
			}
			if err := set.Define(name, c); err != nil {
				return err
			}
			if *sel {
				if err := set.Select(name); err != nil {
					return err
				}
			}
			saved = c
			return nil
		})
		if err != nil {
			return Result{}, storeErr(err)
		}
		cur, _ := currentName(ctx, store)
		v := viewOf(name, cur == name, saved)
		return Result{Data: v, Text: v.text()}, nil
	}
}

func currentName(ctx context.Context, store ContextStore) (string, error) {
	set, err := store.Load(ctx)
	return set.Current, err
}

func bindUse(fs *flag.FlagSet) RunFunc {
	return func(ctx context.Context, inv *Invocation, args []string) (Result, error) {
		o := inv.Overrides
		if len(args) > 0 {
			return Result{}, Usagef("unexpected argument %q; use --context NAME", args[0])
		}
		if err := o.validate(); err != nil {
			return Result{}, err
		}
		if o.Context == nil && o.Tenant == nil && o.Project == nil {
			return Result{}, Usagef("pass --context NAME to select a context and/or --tenant/--project to set its scope")
		}
		if o.Factory != nil || o.Issuer != nil || o.ClientID != nil || o.AuthorizationEndpoint != nil ||
			o.TokenEndpoint != nil || o.Scopes != nil || o.RedirectURI != nil {
			return Result{}, Usagef("`context use` changes only the selection, tenant and project; use `context set` to change the factory or authentication")
		}
		store, err := requireStore(inv)
		if err != nil {
			return Result{}, err
		}
		var name string
		var saved Context
		err = store.Update(ctx, func(set *ContextSet) error {
			name = set.Current
			if o.Context != nil {
				name = *o.Context
			}
			base, ok := set.Contexts[name]
			if !ok {
				if name == "" {
					return Usagef("no context is selected: pass --context NAME")
				}
				return Usagef("unknown context %q", name).WithCause(ErrUnknownContext)
			}
			c, err := apply(base, Overrides{Tenant: o.Tenant, Project: o.Project})
			if err != nil {
				return err
			}
			if err := set.Define(name, c); err != nil {
				return err
			}
			if err := set.Select(name); err != nil {
				return err
			}
			saved = c
			return nil
		})
		if err != nil {
			return Result{}, storeErr(err)
		}
		v := viewOf(name, true, saved)
		return Result{Data: v, Text: v.text()}, nil
	}
}

func bindShow(fs *flag.FlagSet) RunFunc {
	return func(ctx context.Context, inv *Invocation, args []string) (Result, error) {
		if len(args) > 0 {
			return Result{}, Usagef("unexpected argument %q", args[0])
		}
		eff := inv.Context
		cur := ""
		if inv.Contexts != nil {
			cur, _ = currentName(ctx, inv.Contexts)
		}
		v := viewOf(eff.Name, eff.Name != "" && eff.Name == cur, eff.Context)
		return Result{Data: v, Text: v.text()}, nil
	}
}

func bindList(fs *flag.FlagSet) RunFunc {
	return func(ctx context.Context, inv *Invocation, args []string) (Result, error) {
		if len(args) > 0 {
			return Result{}, Usagef("unexpected argument %q", args[0])
		}
		store, err := requireStore(inv)
		if err != nil {
			return Result{}, err
		}
		set, err := store.Load(ctx)
		if err != nil {
			return Result{}, storeErr(err)
		}
		views := []contextView{}
		var b strings.Builder
		for _, n := range set.Names() {
			v := viewOf(n, n == set.Current, set.Contexts[n])
			views = append(views, v)
			mark := " "
			if v.Current {
				mark = "*"
			}
			fmt.Fprintf(&b, "%s %s\t%s\t%s\t%s\n", mark, n, v.Factory, v.Tenant, v.Project)
		}
		return Result{Data: map[string]any{"current": set.Current, "contexts": views}, Text: b.String()}, nil
	}
}
