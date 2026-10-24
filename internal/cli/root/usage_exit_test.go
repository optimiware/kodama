package root_test

import (
	"context"
	"flag"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
)

func Test_invalid_request_is_usage_exit(t *testing.T) {
	var hits int
	srv := clienttest.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	bad := root.Contribution{Group: "probe", Summary: "bad request", Commands: []root.Command{{
		Name: "bad", Summary: "invalid local request", Need: root.NeedFactory,
		Bind: func(fs *flag.FlagSet) root.RunFunc {
			kind := fs.String("kind", "version", "")
			return func(ctx context.Context, inv *root.Invocation, _ []string) (root.Result, error) {
				c, err := inv.Client()
				if err != nil {
					return root.Result{}, err
				}
				req := client.Request{Method: http.MethodPost, Path: []string{"probe"}}
				if *kind == "key" {
					req.IdempotencyKey = "bad key"
				} else {
					req.IfVersion = domain.Version(-1)
				}
				_, err = c.Do(ctx, req, nil)
				return root.Result{}, err
			}
		},
	}}}
	for _, kind := range []string{"version", "key"} {
		hits = 0
		r, out, errb := newRunner(t, root.RunnerConfig{
			Contexts: newMem(), Tokens: &tokenResolver{src: clienttest.Tokens("tok")}, AllowInsecureLoopback: true,
		}, bad)
		code := r.Run(context.Background(), []string{"probe", "bad", "--factory", srv.URL, "--output", "json", "--kind", kind})
		if code != root.ExitUsage || out.Len() != 0 {
			t.Fatalf("%s exit %d stdout %q stderr %s", kind, code, out, errb)
		}
		if hits != 0 {
			t.Fatalf("%s sent %d HTTP requests", kind, hits)
		}
	}
}
