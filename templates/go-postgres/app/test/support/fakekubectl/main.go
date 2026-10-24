// Command fakekubectl is a stateful stand-in for the kubectl subset the
// delivery helper uses (apply -f - and get KIND[.GROUP]/NAME -o json). It exists
// to test the kubectl adapter's command lines and JSON handling; it is not a
// Kubernetes API server and proves nothing about a real cluster.
//
// State lives in $FAKEKUBECTL_STATE. The kubeconfig file must read
// "namespace=<ns>"; a command for any other namespace is Forbidden.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func die(code int, f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(code)
}

func main() {
	args := os.Args[1:]
	var kubeconfig, namespace string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--kubeconfig":
			i++
			kubeconfig = args[i]
		case "--namespace":
			i++
			namespace = args[i]
		case "--request-timeout":
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	state := os.Getenv("FAKEKUBECTL_STATE")
	if state == "" || len(rest) == 0 {
		die(2, "usage")
	}
	cfg, err := os.ReadFile(kubeconfig)
	if err != nil || strings.TrimSpace(string(cfg)) != "namespace="+namespace {
		die(1, "Error from server (Forbidden): the credential cannot access namespace %q", namespace)
	}
	switch rest[0] {
	case "apply":
		raw, _ := io.ReadAll(os.Stdin)
		var l struct {
			Items []map[string]any `json:"items"`
		}
		if json.Unmarshal(raw, &l) != nil {
			die(1, "error: unable to decode the manifests")
		}
		for _, it := range l.Items {
			meta, _ := it["metadata"].(map[string]any)
			if meta["namespace"] != namespace {
				die(1, "error: the namespace from the provided object does not match the namespace %q", namespace)
			}
		}
		for _, it := range l.Items {
			meta := it["metadata"].(map[string]any)
			path := filepath.Join(state, fmt.Sprintf("%s.%s.json", it["kind"], meta["name"]))
			if old, err := os.ReadFile(path); err == nil {
				var prev map[string]any
				_ = json.Unmarshal(old, &prev)
				meta["uid"] = prev["metadata"].(map[string]any)["uid"]
			} else {
				entries, _ := os.ReadDir(state)
				meta["uid"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(entries)+1)
			}
			meta["generation"] = 1
			it["status"] = status(it)
			b, _ := json.Marshal(it)
			_ = os.WriteFile(path, b, 0o600)
		}
		if _, err := os.Stat(filepath.Join(state, "DROP_APPLY_RESPONSE")); err == nil {
			_ = os.Remove(filepath.Join(state, "DROP_APPLY_RESPONSE"))
			die(1, "error: connection reset by peer")
		}
	case "get":
		kind, name, _ := strings.Cut(rest[1], "/")
		kind, _, _ = strings.Cut(kind, ".")
		raw, err := os.ReadFile(filepath.Join(state, kind+"."+name+".json"))
		if err != nil {
			die(1, "Error from server (NotFound): %s %q not found", kind, name)
		}
		os.Stdout.Write(raw)
	default:
		die(2, "unsupported")
	}
}

func status(it map[string]any) map[string]any {
	spec, _ := it["spec"].(map[string]any)
	switch it["kind"] {
	case "Deployment":
		n := 1.0
		if r, ok := spec["replicas"].(float64); ok {
			n = r
		}
		if _, err := os.Stat(os.Getenv("FAKEKUBECTL_STATE") + "/NEVER_READY"); err == nil {
			return map[string]any{"observedGeneration": 1, "updatedReplicas": 0, "availableReplicas": 0, "replicas": n}
		}
		return map[string]any{"observedGeneration": 1, "updatedReplicas": n, "availableReplicas": n, "replicas": n}
	case "Job":
		return map[string]any{"succeeded": 1}
	}
	return nil
}
