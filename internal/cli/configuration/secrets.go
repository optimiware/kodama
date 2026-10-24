package configuration

import (
	"context"
	"flag"
	"net/http"
	"strconv"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

const secretAdvice = "reread the secret metadata (`kodama secret get`) before changing it again; the CLI does not retry or refresh the version for you"

const revokeAdvice = "revocation is idempotent: repeat the identical command to read the same accounting; it keeps the original revocation time"

var secretKinds = map[string]bool{"generic": true, "git": true, "model": true, "mcp": true, "kubeconfig": true}

// NewSecretCommands returns the secret command group.
func NewSecretCommands() root.Contribution {
	return root.Contribution{
		Group:   "secret",
		Summary: "manage write-only project secrets",
		Commands: []root.Command{
			{Name: "create", Summary: "create a secret from --value-file (requires --idempotency-key)", Need: root.NeedFactory, Bind: bindSecretCreate},
			{Name: "get", Summary: "read secret metadata", Need: root.NeedFactory, Bind: bindSecretGet},
			{Name: "list", Summary: "list secret metadata (one page)", Need: root.NeedFactory, Bind: bindSecretList},
			{Name: "versions", Summary: "list a secret's value versions (metadata only)", Need: root.NeedFactory, Bind: bindSecretVersions},
			{Name: "rotate", Summary: "add the next value version from --value-file under --if-version", Need: root.NeedFactory, Bind: bindSecretRotate},
			{Name: "revoke", Summary: "revoke one value version", Need: root.NeedFactory, Bind: bindSecretRevoke},
			{Name: "delete", Summary: "logically delete a secret under --if-version", Need: root.NeedFactory, Bind: bindSecretDelete},
		},
	}
}

const valueFileHelp = "file holding the secret bytes (mode 0600 or stricter, regular file), or - for redirected standard input; never an argument"

func bindSecretCreate(fs *flag.FlagSet) root.RunFunc {
	kind := fs.String("kind", "", "generic, git, model, mcp or kubeconfig (required)")
	valueFile := fs.String("value-file", "", valueFileHelp)
	key := fs.String("idempotency-key", "", "idempotency key (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		name, err := oneArg(args, "the secret name")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		idem, err := requireKey(set, *key)
		if err != nil {
			return root.Result{}, err
		}
		if _, err := requireText(set, "kind", *kind); err != nil {
			return root.Result{}, err
		}
		if !secretKinds[*kind] {
			return root.Result{}, root.Usagef("--kind %q is not one of the accepted values", *kind)
		}
		value, err := secretValue(ctx, inv, set, *valueFile)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{
			Method: http.MethodPost, Scope: scope, Path: []string{"secrets"}, IdempotencyKey: idem,
			Body: map[string]any{"name": name, "kind": *kind, "value": value},
		})
		if err != nil {
			return root.Result{}, err
		}
		return secretResult(raw)
	}
}

// secretValue reads and encodes the secret only after every cheap validation
// passed, so a usage mistake never touches the secret source. The credential
// and scope are resolved by the caller after this point.
func secretValue(ctx context.Context, inv *root.Invocation, set map[string]bool, valueFile string) (string, error) {
	if !set["value-file"] {
		return "", root.Usagef("--value-file is required: a secret is read only from a protected file or redirected standard input (--value-file -)")
	}
	b, err := secretInput{ctx: ctx, stdin: inv.Streams.In}.read("value-file", valueFile)
	if err != nil {
		return "", err
	}
	return encodeSecret(b), nil
}

func bindSecretGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the secret ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"secrets", id}})
		if err != nil {
			return root.Result{}, err
		}
		return secretResult(raw)
	}
}

func bindSecretList(fs *flag.FlagSet) root.RunFunc {
	limit := fs.String("limit", "", "page size")
	cursor := fs.String("cursor", "", "opaque cursor from a previous page's nextCursor")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		return listPage(ctx, inv, args, explicit(fs), *limit, *cursor, "secrets", secretListText)
	}
}

func bindSecretVersions(fs *flag.FlagSet) root.RunFunc {
	limit := fs.String("limit", "", "page size")
	cursor := fs.String("cursor", "", "opaque cursor from a previous page's nextCursor")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the secret ID")
		if err != nil {
			return root.Result{}, err
		}
		q, err := pageQuery(explicit(fs), *limit, *cursor)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"secrets", id, "versions"}, Query: q})
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: versionsText(doc)}, nil
	}
}

func bindSecretRotate(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "secret resource version you observed (required)")
	valueFile := fs.String("value-file", "", valueFileHelp)
	key := fs.String("idempotency-key", "", "idempotency key (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the secret ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		version, err := requireVersion(set, "if-version", *ifVersion)
		if err != nil {
			return root.Result{}, err
		}
		idem, err := requireKey(set, *key)
		if err != nil {
			return root.Result{}, err
		}
		value, err := secretValue(ctx, inv, set, *valueFile)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{
			Method: http.MethodPost, Scope: scope, Path: []string{"secrets", id, "versions"}, IfVersion: version, IdempotencyKey: idem,
			Body: map[string]any{"value": value},
		})
		if err != nil {
			return root.Result{}, advise(err, secretAdvice)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: rotatedText(doc)}, nil
	}
}

func bindSecretRevoke(fs *flag.FlagSet) root.RunFunc {
	valueVersion := fs.String("version", "", "value version to revoke (not the resource version) (required)")
	reason := fs.String("reason", "", "reason recorded in the audit entry; never put secret values in it")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the secret ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		if !set["version"] {
			return root.Result{}, root.Usagef("--version is required: the value version to revoke")
		}
		n, err := parseVersion("version", *valueVersion)
		if err != nil {
			return root.Result{}, err
		}
		req := client.Request{Method: http.MethodPost, Scope: client.Scope{}, Path: []string{"secrets", id, "versions", strconv.FormatInt(int64(n), 10), "revoke"}}
		if set["reason"] {
			if *reason == "" {
				return root.Result{}, root.Usagef("--reason must not be empty")
			}
			req.Body = map[string]any{"reason": *reason}
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		req.Scope = scope
		raw, _, err := send(ctx, c, req)
		if err != nil {
			return root.Result{}, advise(err, revokeAdvice)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: revokedText(doc)}, nil
	}
}

func bindSecretDelete(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "secret resource version you observed (required)")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the secret ID")
		if err != nil {
			return root.Result{}, err
		}
		version, err := requireVersion(explicit(fs), "if-version", *ifVersion)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, client.Request{Method: http.MethodDelete, Scope: scope, Path: []string{"secrets", id}, IfVersion: version})
		if err != nil {
			return root.Result{}, advise(err, secretAdvice)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: deletedSecretText(doc)}, nil
	}
}

func secretResult(raw []byte) (root.Result, error) {
	doc, err := document(raw)
	if err != nil {
		return root.Result{}, err
	}
	return root.Result{Data: doc, Text: secretText(doc)}, nil
}
