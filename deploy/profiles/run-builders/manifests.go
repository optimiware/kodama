package runbuilders

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

// AllowedHostPaths are the only host directories the installer may mount.
var AllowedHostPaths = []string{HostSeccompDir}

type object = map[string]any

func child(parent object, key string) object {
	value, _ := parent[key].(object)
	return value
}

func list(parent object, key string) []any {
	value, _ := parent[key].([]any)
	return value
}

func text(parent object, key string) string {
	value, _ := parent[key].(string)
	return value
}

// ValidateManifests checks a rendered installer YAML stream (install or uninstall) against the
// profiles it must carry. It fails when the installer could reach a tenant namespace, use an
// unpinned image, mount unexpected host paths or carry profile content that differs from the
// shipped files.
func ValidateManifests(stream []byte, p Profiles) error {
	decoder := yaml.NewDecoder(bytes.NewReader(stream))
	kinds := map[string]int{}
	for {
		var doc object
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
		if doc == nil {
			continue
		}
		kind := text(doc, "kind")
		kinds[kind]++
		metadata := child(doc, "metadata")
		name := text(metadata, "name")
		switch kind {
		case "Namespace":
			if name != Namespace {
				return fmt.Errorf("manifest: namespace %q is not the installation-owned %q", name, Namespace)
			}
			if got := text(child(metadata, "labels"), "pod-security.kubernetes.io/enforce"); got != "privileged" {
				return fmt.Errorf("manifest: namespace %q must declare its privileged Pod Security level, got %q", name, got)
			}
		case "ServiceAccount", "ConfigMap", "DaemonSet":
			if got := text(metadata, "namespace"); got != Namespace {
				return fmt.Errorf("manifest: %s %q is in namespace %q, want %q", kind, name, got, Namespace)
			}
			if strings.HasPrefix(text(metadata, "namespace"), "kodama-tenant") {
				return fmt.Errorf("manifest: %s %q is in a tenant namespace", kind, name)
			}
		default:
			return fmt.Errorf("manifest: kind %q is not allowed (no RBAC, cluster-scoped or other objects)", kind)
		}
		switch kind {
		case "ServiceAccount":
			if value, ok := doc["automountServiceAccountToken"].(bool); !ok || value {
				return errors.New("manifest: the installer ServiceAccount must not mount a token")
			}
		case "ConfigMap":
			data := child(doc, "data")
			if text(data, SeccompFile) != string(p.Seccomp) || text(data, ProfileVersion) != string(p.AppArmor) {
				return errors.New("manifest: ConfigMap profile content differs from the shipped files")
			}
			if text(data, "install.sh") != string(installScript) {
				return errors.New("manifest: ConfigMap installer script differs from the shipped script")
			}
		case "DaemonSet":
			if err := validateDaemonSet(doc); err != nil {
				return err
			}
		}
	}
	if kinds["DaemonSet"] == 0 {
		return errors.New("manifest: no DaemonSet")
	}
	return nil
}

func validateDaemonSet(doc object) error {
	spec := child(child(child(doc, "spec"), "template"), "spec")
	for _, field := range []string{"hostNetwork", "hostIPC"} {
		if value, _ := spec[field].(bool); value {
			return fmt.Errorf("manifest: DaemonSet sets %s", field)
		}
	}
	if value, ok := spec["automountServiceAccountToken"].(bool); !ok || value {
		return errors.New("manifest: DaemonSet must not mount a ServiceAccount token")
	}
	containers := list(spec, "containers")
	if len(containers) != 1 || list(spec, "initContainers") != nil {
		return errors.New("manifest: DaemonSet must have exactly one container and no init containers")
	}
	container, _ := containers[0].(object)
	if err := ValidateImage(text(container, "image")); err != nil {
		return err
	}
	for _, volume := range list(spec, "volumes") {
		volume, _ := volume.(object)
		for key := range volume {
			switch key {
			case "name", "configMap":
			case "hostPath":
				path := text(child(volume, "hostPath"), "path")
				ok := false
				for _, allowed := range AllowedHostPaths {
					ok = ok || path == allowed
				}
				if !ok {
					return fmt.Errorf("manifest: hostPath %q is not one of %v", path, AllowedHostPaths)
				}
			default:
				return fmt.Errorf("manifest: volume type %q is not allowed", key)
			}
		}
	}
	return nil
}
