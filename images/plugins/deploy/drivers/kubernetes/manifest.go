package kubernetes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/optimiware/kodama/images/plugins/deploy/bundle"
	"github.com/optimiware/kodama/images/plugins/deploy/driver"
	"go.yaml.in/yaml/v3"
)

// object is one bounded manifest document after validation. The document is
// retained so ownership annotations can be added without a second shell pass.
type object struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	Doc        map[string]any
}

var clusterScopedKinds = map[string]bool{
	"APIService": true, "CSIDriver": true, "CSINode": true,
	"ClusterRole": true, "ClusterRoleBinding": true, "CustomResourceDefinition": true,
	"IngressClass": true, "MutatingWebhookConfiguration": true, "Namespace": true,
	"Node": true, "PersistentVolume": true, "PriorityClass": true, "RuntimeClass": true,
	"StorageClass": true, "ValidatingWebhookConfiguration": true, "VolumeAttachment": true,
}

var allowedKinds = map[string]bool{
	"ConfigMap": true, "CronJob": true, "DaemonSet": true, "Deployment": true,
	"HorizontalPodAutoscaler": true, "Ingress": true, "Job": true, "NetworkPolicy": true,
	"PersistentVolumeClaim": true, "Pod": true, "PodDisruptionBudget": true, "Role": true, "RoleBinding": true,
	"Secret": true, "Service": true, "ServiceAccount": true, "StatefulSet": true,
}

func loadManifest(cfg Config) ([]byte, []object, error) {
	var data []byte
	var err error
	if cfg.ManifestPath != "" {
		data, err = os.ReadFile(cfg.ManifestPath)
	} else {
		var files bundle.Files
		files, err = bundle.Read(cfg.BundlePath)
		if err == nil {
			var ok bool
			data, ok = files.Manifest()
			if !ok {
				return nil, nil, fmt.Errorf("kubernetes driver: bundle has no manifest.yaml")
			}
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes driver: read manifest: %w", err)
	}
	objects, err := parseObjects(data, cfg.Namespace)
	if err != nil {
		return nil, nil, err
	}
	return data, objects, nil
}

func parseObjects(data []byte, namespace string) ([]object, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []object
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("kubernetes driver: parse manifest: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		obj, err := validateObject(doc, namespace)
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("kubernetes driver: manifest is empty")
	}
	if len(out) > 64 {
		return nil, fmt.Errorf("kubernetes driver: manifest has too many objects")
	}
	return out, nil
}

func validateObject(doc map[string]any, namespace string) (object, error) {
	apiVersion := text(doc["apiVersion"])
	kind := text(doc["kind"])
	if apiVersion == "" || kind == "" {
		return object{}, fmt.Errorf("kubernetes driver: every object needs apiVersion and kind")
	}
	if clusterScopedKinds[kind] {
		return object{}, fmt.Errorf("kubernetes driver: cluster-scoped kind %s is not allowed", kind)
	}
	if !allowedKinds[kind] {
		return object{}, fmt.Errorf("kubernetes driver: unsupported kind %s", kind)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		return object{}, fmt.Errorf("kubernetes driver: %s has no metadata", kind)
	}
	if text(meta["generateName"]) != "" {
		return object{}, fmt.Errorf("kubernetes driver: %s uses generateName; a stable name is required", kind)
	}
	name := text(meta["name"])
	if name == "" {
		return object{}, fmt.Errorf("kubernetes driver: %s has no metadata.name", kind)
	}
	ns := text(meta["namespace"])
	if ns != namespace {
		return object{}, fmt.Errorf("kubernetes driver: %s %s targets namespace %q, not %q", kind, name, ns, namespace)
	}
	return object{APIVersion: apiVersion, Kind: kind, Name: name, Namespace: ns, Doc: doc}, nil
}

// render annotates every object with the stable ownership marker and returns a
// single bounded YAML stream. The annotations are applied as part of the same
// request, so there is no window where an object exists without its owner.
func render(objects []object, identity, release, operation string) ([]byte, error) {
	var out bytes.Buffer
	for i := range objects {
		meta, _ := objects[i].Doc["metadata"].(map[string]any)
		annotations, _ := meta["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
		}
		annotations["kodama.optimi.com/resource-key"] = identity
		annotations["kodama.optimi.com/release"] = release
		annotations["kodama.optimi.com/operation"] = operation
		meta["annotations"] = annotations
		objects[i].Doc["metadata"] = meta
		raw, err := yaml.Marshal(objects[i].Doc)
		if err != nil {
			return nil, fmt.Errorf("kubernetes driver: render manifest: %w", err)
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(raw)
	}
	return out.Bytes(), nil
}

func manifestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateImages(objects []object, cfg Config) error {
	if cfg.AllowAnyImage {
		return nil
	}
	if cfg.Image == "" {
		return fmt.Errorf("kubernetes driver: the expected primary image is required")
	}
	if !cfg.AllowMutableImage && !strings.Contains(cfg.Image, "@sha256:") {
		return fmt.Errorf("kubernetes driver: the primary image must be digest-pinned")
	}
	allowed := map[string]bool{cfg.Image: true}
	for _, image := range cfg.AuxiliaryImages {
		allowed[image] = true
	}
	foundMain := false
	for _, obj := range objects {
		for _, image := range objectImages(obj.Doc) {
			if image == cfg.Image {
				foundMain = true
			}
			if !allowed[image] {
				return fmt.Errorf("kubernetes driver: %s %s references unapproved image %s", obj.Kind, obj.Name, image)
			}
		}
	}
	if !foundMain {
		return fmt.Errorf("kubernetes driver: the primary image is not deployed by the manifest")
	}
	return nil
}

func objectImages(doc map[string]any) []string {
	var images []string
	var walk func(any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			for key, value := range node {
				if key == "containers" || key == "initContainers" {
					if list, ok := value.([]any); ok {
						for _, item := range list {
							if container, ok := item.(map[string]any); ok {
								if image := text(container["image"]); image != "" {
									images = append(images, image)
								}
							}
						}
					}
				}
				walk(value)
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(doc)
	sort.Strings(images)
	return images
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

func objectRefs(objects []object) []driver.Resource {
	out := make([]driver.Resource, 0, len(objects))
	for _, obj := range objects {
		out = append(out, driver.Resource{APIVersion: obj.APIVersion, Kind: obj.Kind, Namespace: obj.Namespace, Name: obj.Name})
	}
	return out
}
