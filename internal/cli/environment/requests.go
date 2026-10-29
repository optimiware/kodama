package environment

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
)

var ttlPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// Request bodies are the EnvironmentCreate and EnvironmentEdit of
// api/delivery.yaml and nothing else. They are maps so an edit sends exactly
// the members the caller named: an omitted member is unchanged on the server.

func listRequest(scope client.Scope, q url.Values) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"environments"}, Query: q}
}

func getRequest(scope client.Scope, id string) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"environments", id}}
}

func createRequest(scope client.Scope, body map[string]any) client.Request {
	return client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"environments"}, Body: body}
}

func updateRequest(scope client.Scope, id string, v domain.Version, body map[string]any) client.Request {
	return client.Request{Method: http.MethodPatch, Scope: scope, Path: []string{"environments", id}, IfVersion: v, Body: body}
}

func addOptionalTarget(set map[string]bool, body map[string]any, s settings) error {
	if set["target-profile"] {
		profile, err := requireText(set, "target-profile", *s.targetProfile)
		if err != nil {
			return err
		}
		body["targetProfile"] = profile
	}
	if set["target-config"] {
		cfg := map[string]any{}
		for _, pair := range *s.targetConfigs {
			k, v, ok := strings.Cut(pair, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return root.Usagef("--target-config expects KEY=VALUE")
			}
			cfg[k] = v
		}
		body["targetConfig"] = cfg
	}
	return nil
}

func createBody(set map[string]bool, name, kind string, s settings) (map[string]any, error) {
	if strings.TrimSpace(name) == "" {
		return nil, root.Usagef("the environment name must not be empty")
	}
	if !set["kind"] {
		return nil, root.Usagef("--kind is required: preview, stage or production")
	}
	if kind != "preview" && kind != "stage" && kind != "production" {
		return nil, root.Usagef("--kind must be preview, stage or production")
	}
	body := map[string]any{"name": name, "kind": kind}
	if set["target-profile"] {
		profile, err := requireText(set, "target-profile", *s.targetProfile)
		if err != nil {
			return nil, err
		}
		body["targetProfile"] = profile
		if len(*s.secrets) == 0 {
			return nil, root.Usagef("a profile environment needs at least one --secret credential")
		}
	} else {
		target, err := requireText(set, "target", *s.target)
		if err != nil {
			return nil, err
		}
		namespace, err := requireText(set, "namespace", *s.namespace)
		if err != nil {
			return nil, err
		}
		if !set["kubeconfig"] {
			return nil, root.Usagef("--kubeconfig is required for a legacy Kubernetes environment: NAME=secret://project/REF@file|env")
		}
		kc, err := parseBinding("kubeconfig", *s.kubeconfig)
		if err != nil {
			return nil, err
		}
		body["target"], body["namespace"], body["kubeconfig"] = target, namespace, kc
	}
	if err := addOptionalTarget(set, body, s); err != nil {
		return nil, err
	}
	if err := addOptional(set, body, s); err != nil {
		return nil, err
	}
	if (kind == "stage" || kind == "production") && set["preview-ttl-seconds"] {
		return nil, root.Usagef("--preview-ttl-seconds applies to a preview environment only; a stage environment has no expiry")
	}
	return body, nil
}

func editBody(set map[string]bool, s settings) (map[string]any, error) {
	body := map[string]any{}
	if set["target"] {
		v, err := requireText(set, "target", *s.target)
		if err != nil {
			return nil, err
		}
		body["target"] = v
	}
	if set["namespace"] {
		v, err := requireText(set, "namespace", *s.namespace)
		if err != nil {
			return nil, err
		}
		body["namespace"] = v
	}
	if set["kubeconfig"] {
		kc, err := parseBinding("kubeconfig", *s.kubeconfig)
		if err != nil {
			return nil, err
		}
		body["kubeconfig"] = kc
	}
	if err := addOptionalTarget(set, body, s); err != nil {
		return nil, err
	}
	if set["clear-secrets"] && *s.clearSecrets && set["secret"] {
		return nil, root.Usagef("--clear-secrets and --secret conflict: name the complete list or clear it")
	}
	if set["clear-config"] && *s.clearConfig && set["config"] {
		return nil, root.Usagef("--clear-config and --config conflict: name the complete configuration or clear it")
	}
	if err := addOptional(set, body, s); err != nil {
		return nil, err
	}
	if set["clear-secrets"] && *s.clearSecrets {
		body["secrets"] = []any{}
	}
	if set["clear-config"] && *s.clearConfig {
		body["config"] = map[string]any{}
	}
	if len(body) == 0 {
		return nil, root.Usagef("nothing to change: pass --target, --namespace, --kubeconfig, --secret, --config, --preview-ttl-seconds, --clear-secrets or --clear-config")
	}
	return body, nil
}

// addOptional adds the secrets, configuration and TTL the caller named. A
// named list replaces the stored one, as the server defines for these members.
func addOptional(set map[string]bool, body map[string]any, s settings) error {
	if set["secret"] {
		list := make([]any, 0, len(*s.secrets))
		for _, raw := range *s.secrets {
			b, err := parseBinding("secret", raw)
			if err != nil {
				return err
			}
			list = append(list, b)
		}
		body["secrets"] = list
	}
	if set["config"] {
		cfg := make(map[string]any, len(*s.configs))
		for _, raw := range *s.configs {
			k, v, ok := strings.Cut(raw, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return root.Usagef("--config must be KEY=VALUE")
			}
			if _, dup := cfg[k]; dup {
				return root.Usagef("--config %q is given twice: a configuration key has one value", k)
			}
			cfg[k] = v
		}
		body["config"] = cfg
	}
	if set["preview-ttl-seconds"] {
		if !ttlPattern.MatchString(*s.ttl) {
			return root.Usagef("--preview-ttl-seconds must be a positive integer number of seconds")
		}
		body["previewTtlSeconds"] = json.Number(*s.ttl)
	}
	return nil
}

// parseBinding reads NAME=REF@DELIVERY exactly as written. Only the shape is
// checked here; the reference's project, kind and existence belong to the
// server. A value can never be expressed: a binding carries a reference only.
func parseBinding(flagName, raw string) (map[string]any, error) {
	name, rest, ok := strings.Cut(raw, "=")
	i := strings.LastIndex(rest, "@")
	if !ok || i < 0 {
		return nil, root.Usagef("--%s must be NAME=secret://project/REF@file or @env", flagName)
	}
	ref, delivery := rest[:i], rest[i+1:]
	if strings.TrimSpace(name) == "" || strings.TrimSpace(ref) == "" {
		return nil, root.Usagef("--%s must name both a file or variable name and a secret reference", flagName)
	}
	if delivery != "file" && delivery != "env" {
		return nil, root.Usagef("--%s delivery must be file or env", flagName)
	}
	return map[string]any{"name": name, "ref": ref, "delivery": delivery}, nil
}
