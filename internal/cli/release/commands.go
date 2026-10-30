// Package release contributes the ADR-0012 `artifact`, `release`, and
// `publication` CLI command groups and release-input binding helpers.
package release

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
)

// ParseReleaseFlag parses "<name>@<version>" and repeatable "input=entry"
// bindings into a validated ReleaseSelection.
func ParseReleaseFlag(coord string, binds []string) (artifactsv1.ReleaseSelection, error) {
	parts := strings.SplitN(coord, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return artifactsv1.ReleaseSelection{}, fmt.Errorf("%w: --release must be <name>@<version>", domain.ErrInvalidValue)
	}
	m := make(map[string]string, len(binds))
	for _, b := range binds {
		kv := strings.SplitN(b, "=", 2)
		if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
			return artifactsv1.ReleaseSelection{}, fmt.Errorf("%w: --bind must be <input>=<entry>", domain.ErrInvalidValue)
		}
		if _, dup := m[kv[0]]; dup {
			return artifactsv1.ReleaseSelection{}, fmt.Errorf("%w: duplicate --bind for input %q", domain.ErrInvalidValue, kv[0])
		}
		m[kv[0]] = kv[1]
	}
	sel := artifactsv1.ReleaseSelection{
		Name:         parts[0],
		VersionLabel: parts[1],
		Bindings:     m,
	}
	return sel, sel.Validate()
}

// NewArtifactCommands returns the `kodama artifact` command group.
func NewArtifactCommands() root.Contribution {
	return root.Contribution{
		Group:   "artifact",
		Summary: "inspect and download retained pipeline artifacts",
		Commands: []root.Command{
			{Name: "list", Summary: "list retained artifacts in the project", Need: root.NeedFactory, Bind: bindArtifactList},
			{Name: "get", Summary: "read artifact metadata and provenance", Need: root.NeedFactory, Bind: bindArtifactGet},
			{Name: "download", Summary: "issue a short-lived download instruction for a verified file artifact", Need: root.NeedFactory, Bind: bindArtifactDownload},
		},
	}
}

// NewReleaseCommands returns the `kodama release` command group.
func NewReleaseCommands() root.Contribution {
	return root.Contribution{
		Group:   "release",
		Summary: "create, resolve, inspect and retire immutable artifact releases",
		Commands: []root.Command{
			{Name: "create", Summary: "create an immutable named release manifest from verified artifacts", Need: root.NeedFactory, Bind: bindReleaseCreate},
			{Name: "list", Summary: "list releases in the project", Need: root.NeedFactory, Bind: bindReleaseList},
			{Name: "get", Summary: "read a release manifest by ID", Need: root.NeedFactory, Bind: bindReleaseGet},
			{Name: "resolve", Summary: "resolve an active release by name and version label", Need: root.NeedFactory, Bind: bindReleaseResolve},
			{Name: "retire", Summary: "retire a release version under --if-version", Need: root.NeedFactory, Bind: bindReleaseRetire},
		},
	}
}

// NewPublicationCommands returns the `kodama publication` command group.
func NewPublicationCommands() root.Contribution {
	return root.Contribution{
		Group:   "publication",
		Summary: "inspect verified artifact publication receipts",
		Commands: []root.Command{
			{Name: "list", Summary: "list verified publication receipts for an artifact", Need: root.NeedFactory, Bind: bindPublicationList},
		},
	}
}

func bindArtifactList(_ *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 0 {
			return root.Result{}, root.Usagef("artifact list takes no positional arguments")
		}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"artifacts"}}
		})
	}
}

func bindArtifactGet(_ *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 1 || args[0] == "" {
			return root.Result{}, root.Usagef("usage: kodama artifact get <artifact-id>")
		}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"artifacts", args[0]}}
		})
	}
}

func bindArtifactDownload(_ *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 1 || args[0] == "" {
			return root.Result{}, root.Usagef("usage: kodama artifact download <artifact-id>")
		}
		q := url.Values{"download": []string{"true"}}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"artifacts", args[0]}, Query: q}
		})
	}
}

func bindReleaseCreate(fs *flag.FlagSet) root.RunFunc {
	file := fs.String("file", "", "path to release manifest JSON file (required)")
	key := fs.String("idempotency-key", "", "idempotency key (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 0 || *file == "" || *key == "" {
			return root.Result{}, root.Usagef("usage: kodama release create --file <manifest.json> --idempotency-key <key>")
		}
		raw, err := os.ReadFile(*file)
		if err != nil {
			return root.Result{}, root.Usagef("read --file: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return root.Result{}, root.Usagef("invalid release manifest JSON: %v", err)
		}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"releases"}, IdempotencyKey: *key, Body: body}
		})
	}
}

func bindReleaseList(fs *flag.FlagSet) root.RunFunc {
	name := fs.String("name", "", "optional release name filter")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 0 {
			return root.Result{}, root.Usagef("release list takes no positional arguments")
		}
		q := url.Values{}
		if *name != "" {
			q.Set("name", *name)
		}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"releases"}, Query: q}
		})
	}
}

func bindReleaseGet(_ *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 1 || args[0] == "" {
			return root.Result{}, root.Usagef("usage: kodama release get <release-id>")
		}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"releases", args[0]}}
		})
	}
}

func bindReleaseResolve(fs *flag.FlagSet) root.RunFunc {
	name := fs.String("name", "", "release name (required)")
	ver := fs.String("version", "", "release version label (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 0 || *name == "" || *ver == "" {
			return root.Result{}, root.Usagef("usage: kodama release resolve --name <name> --version <label>")
		}
		q := url.Values{"name": []string{*name}, "version": []string{*ver}}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"releases", "resolve"}, Query: q}
		})
	}
}

func bindReleaseRetire(fs *flag.FlagSet) root.RunFunc {
	ifVer := fs.String("if-version", "", "expected release version for optimistic concurrency (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 1 || args[0] == "" || *ifVer == "" {
			return root.Result{}, root.Usagef("usage: kodama release retire --if-version <n> <release-id>")
		}
		v, err := strconv.ParseInt(*ifVer, 10, 64)
		if err != nil || domain.Version(v).Validate() != nil {
			return root.Result{}, root.Usagef("--if-version must be a positive integer")
		}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{
				Method:    http.MethodPost,
				Scope:     scope,
				Path:      []string{"releases", args[0], "retire"},
				IfVersion: domain.Version(v),
				Body:      map[string]any{},
			}
		})
	}
}

func bindPublicationList(fs *flag.FlagSet) root.RunFunc {
	artifactID := fs.String("artifact-id", "", "artifact ID (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if len(args) != 0 || *artifactID == "" {
			return root.Result{}, root.Usagef("usage: kodama publication list --artifact-id <artifact-id>")
		}
		return doRequest(ctx, inv, func(scope client.Scope) client.Request {
			return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"artifacts", *artifactID, "publications"}}
		})
	}
}

func doRequest(ctx context.Context, inv *root.Invocation, build func(scope client.Scope) client.Request) (root.Result, error) {
	scope, err := inv.Context.RequireProject()
	if err != nil {
		return root.Result{}, err
	}
	c, err := inv.Client()
	if err != nil {
		return root.Result{}, err
	}
	var raw json.RawMessage
	if _, err := c.Do(ctx, build(scope), &raw); err != nil {
		return root.Result{}, err
	}
	doc, err := root.NormalizeJSON(raw)
	if err != nil {
		return root.Result{}, root.NewError(root.ExitFailure, "malformed_response", "the factory returned a response that is not a JSON document")
	}
	return root.Result{Data: doc, Text: strings.TrimSpace(string(raw))}, nil
}
