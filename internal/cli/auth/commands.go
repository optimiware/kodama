package auth

import (
	"context"
	"flag"
	"fmt"

	"github.com/optimiware/kodama/internal/cli/root"
)

// NewCommands contributes `auth login` and `auth logout`. Central registration
// belongs to the composition root.
func NewCommands(s *Service) root.Contribution {
	return root.Contribution{
		Group:   "auth",
		Summary: "log in to and out of a factory",
		Commands: []root.Command{
			{Name: "login", Summary: "log in with OAuth authorization code and PKCE", Need: root.NeedFactory,
				Bind: func(*flag.FlagSet) root.RunFunc {
					return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
						if len(args) > 0 {
							return root.Result{}, root.Usagef("unexpected argument %q", args[0])
						}
						r, err := s.Login(ctx, inv.Context)
						if err != nil {
							return root.Result{}, err
						}
						return root.Result{Data: r, Text: fmt.Sprintf("logged in to %s", r.Factory)}, nil
					}
				}},
			{Name: "logout", Summary: "delete the local credential (does not revoke the token)", Need: root.NeedFactory,
				Bind: func(*flag.FlagSet) root.RunFunc {
					return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
						if len(args) > 0 {
							return root.Result{}, root.Usagef("unexpected argument %q", args[0])
						}
						if err := s.Logout(ctx, inv.Context); err != nil {
							return root.Result{}, err
						}
						return root.Result{
							Data: map[string]any{"factory": inv.Context.Factory, "loggedOut": true, "remoteRevocation": false},
							Text: fmt.Sprintf("logged out of %s (local credential removed; the token was not revoked at the provider)", inv.Context.Factory),
						}, nil
					}
				}},
		},
	}
}
