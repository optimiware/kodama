// Package environment contributes the `kodama environment` commands: list,
// get, create and update the project's delivery environments and their
// explicit nonsecret configuration. They translate user input into the public
// requests of api/delivery.yaml and render the answers.
//
// An environment names a namespace, a kubeconfig credential reference and
// explicit script credential references, never a value: no secret, kubeconfig
// content or manifest is accepted or shown. Every rule - permissions
// (deployment:manage), the kubeconfig secret kind, namespace scoping, name
// uniqueness, TTL bounds and configuration pinning for runs admitted before an
// edit - stays in the server. The CLI never infers a version, refreshes one or
// retries a mutation.
package environment

import (
	"context"
	"flag"

	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	editAdvice = "reread the environment (`kodama environment get`) before editing again; the CLI does not retry or refresh the version for you. An edit affects only runs admitted afterwards: runs already admitted keep the configuration they pinned"

	createAdvice = "the environment name is already used in this project (or the definition conflicts): list the environments (`kodama environment list`) and read the existing one before choosing another name"

	createUncertain = "the environment may have been created: list the environments (`kodama environment list`) or read it (`kodama environment get`); creating the same name again is refused as a conflict, never duplicated"

	editUncertain = "the edit may have been applied: reread the environment (`kodama environment get`) and compare its version before editing again; repeating the edit with the old --if-version is refused if it was applied"
)

// NewCommands returns the environment command group for registration by the
// composition root.
func NewCommands() root.Contribution {
	return root.Contribution{
		Group:   "environment",
		Summary: "manage delivery environments and their explicit configuration",
		Commands: []root.Command{
			{Name: "list", Summary: "list the project's environments (one page)", Need: root.NeedFactory, Bind: bindList},
			{Name: "get", Summary: "read an environment: namespace, credential references, configuration and version", Need: root.NeedFactory, Bind: bindGet},
			{Name: "create", Summary: "define a preview or stage environment (references only, never values)", Need: root.NeedFactory, Bind: bindCreate},
			{Name: "update", Summary: "edit an environment under --if-version", Need: root.NeedFactory, Bind: bindUpdate},
		},
	}
}

func bindList(fs *flag.FlagSet) root.RunFunc {
	limit := fs.String("limit", "", "page size")
	cursor := fs.String("cursor", "", "opaque cursor from a previous page's nextCursor")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		if err := noArgs(args); err != nil {
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
		raw, _, err := send(ctx, c, listRequest(scope, q))
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: listText(doc)}, nil
	}
}

func bindGet(fs *flag.FlagSet) root.RunFunc {
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the environment ID")
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, getRequest(scope, id))
		if err != nil {
			return root.Result{}, err
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: environmentText(doc)}, nil
	}
}

// settings are the definition flags shared by create and update.
type settings struct {
	target       *string
	namespace    *string
	kubeconfig   *string
	secrets      *stringList
	configs      *stringList
	ttl          *string
	clearSecrets *bool
	clearConfig  *bool
}

func bindSettings(fs *flag.FlagSet, update bool) settings {
	s := settings{
		target:     fs.String("target", "", "label of the cluster or target"),
		namespace:  fs.String("namespace", "", "the single Kubernetes namespace a deployment may touch"),
		kubeconfig: fs.String("kubeconfig", "", "kubeconfig credential binding NAME=secret://project/REF@file|env (a reference, never a value)"),
		secrets:    &stringList{},
		configs:    &stringList{},
		ttl:        fs.String("preview-ttl-seconds", "", "preview environments only: seconds before a preview expires"),
	}
	fs.Var(s.secrets, "secret", "script credential binding NAME=secret://project/REF@file|env (repeatable; references only)")
	fs.Var(s.configs, "config", "explicit nonsecret configuration KEY=VALUE (repeatable)")
	if update {
		s.clearSecrets = fs.Bool("clear-secrets", false, "remove every script credential binding")
		s.clearConfig = fs.Bool("clear-config", false, "remove every configuration entry")
	}
	return s
}

func bindCreate(fs *flag.FlagSet) root.RunFunc {
	kind := fs.String("kind", "", "preview or stage (required)")
	s := bindSettings(fs, false)
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		name, err := oneArg(args, "the environment name")
		if err != nil {
			return root.Result{}, err
		}
		body, err := createBody(explicit(fs), name, *kind, s)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, createRequest(scope, body))
		if err != nil {
			return root.Result{}, advise(err, createAdvice, createUncertain)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: environmentText(doc)}, nil
	}
}

func bindUpdate(fs *flag.FlagSet) root.RunFunc {
	ifVersion := fs.String("if-version", "", "environment version you observed (required)")
	s := bindSettings(fs, true)
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		id, err := oneArg(args, "the environment ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		version, err := requireVersion(set, "if-version", *ifVersion)
		if err != nil {
			return root.Result{}, err
		}
		body, err := editBody(set, s)
		if err != nil {
			return root.Result{}, err
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		raw, _, err := send(ctx, c, updateRequest(scope, id, version, body))
		if err != nil {
			return root.Result{}, advise(err, editAdvice, editUncertain)
		}
		doc, err := document(raw)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: environmentText(doc)}, nil
	}
}
