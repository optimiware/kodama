package kube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// ownedWant is what the committed handle says an object must be.
type ownedWant struct {
	scope domain.Scope
	run   domain.RunID
	kind  execution.ResourceKind
	key   string
	// name is the resource's deterministic name (the annotation value); the
	// object's own name may differ (a Service is named by the endpoint host).
	name string
	// uid is the bound instance UID ("" while none is bound).
	uid string
	// fingerprint, when set, must equal the object's fingerprint annotation.
	fingerprint string
	// labels are further labels the object must carry.
	labels map[string]string
	// owner, when set, is the controlling owner a Pod must have; otherwise the
	// object must have none.
	owner *kubernetes.OwnerReference
}

// finalizers the platform adds to our objects; any other finalizer is foreign.
var benignFinalizers = []string{"foregroundDeletion", "orphan", "kubernetes.io/pvc-protection", "batch.kubernetes.io/job-tracking"}

// checkOwned verifies an object's metadata against the committed handle: its
// name and namespace, an instance UID (equal to the bound one), the ownership
// labels and annotations, no foreign owner or finalizer. Labels and annotations
// are copyable evidence: this check gates adoption together with the spec
// comparison, never alone. The message lists a path, never a value.
func checkOwned(m kubernetes.ObjectMeta, ns, objName string, w ownedWant) error {
	bad := func(why string) error { return wrap(ErrForeignWorkload, "%s", why) }
	if m.Name != objName || m.Namespace != ns {
		return bad("name or namespace differs")
	}
	if m.UID == "" {
		return bad("no UID")
	}
	if w.uid != "" && m.UID != w.uid {
		return bad("the instance UID differs from the bound one")
	}
	switch {
	case w.owner == nil && len(m.OwnerReferences) != 0:
		return bad("the object has an owner")
	case w.owner != nil:
		if len(m.OwnerReferences) != 1 || !m.OwnerReferences[0].Controlled() || m.OwnerReferences[0].Kind != w.owner.Kind ||
			m.OwnerReferences[0].Name != w.owner.Name || m.OwnerReferences[0].UID != w.owner.UID {
			return bad("the object's owner differs")
		}
	}
	for _, f := range m.Finalizers {
		if !slices.Contains(benignFinalizers, f) {
			return bad("unexpected finalizer")
		}
	}
	for k, v := range ownerLabels(w.scope, w.run, w.kind) {
		if m.Labels[k] != v {
			return bad("label " + k + " differs")
		}
	}
	for k, v := range w.labels {
		if m.Labels[k] != v {
			return bad("label " + k + " differs")
		}
	}
	want := map[string]string{
		AnnTenant: string(w.scope.TenantID), AnnProject: string(w.scope.ProjectID), AnnRun: string(w.run),
		AnnResourceKind: string(w.kind), AnnResourceKey: w.key,
	}
	if w.name != "" {
		want[AnnResourceName] = w.name
	}
	if w.fingerprint != "" {
		want[AnnFingerprint] = w.fingerprint
	}
	for k, v := range want {
		if m.Annotations[k] != v {
			return bad("annotation " + k + " differs")
		}
	}
	return nil
}

// benignFn decides whether a field the server added, at a normalized path, is a
// known default.
type benignFn func(path string, v any) bool

// compareDoc compares the controlled document of an object with the one the
// server returned. Every expected field must be present and equal; every extra
// field must be a known server default (benign). Anything else, a new field
// included, is a difference. It returns the sorted paths of the differences,
// at most five, never values.
func compareDoc(want any, raw []byte, benign benignFn) ([]string, error) {
	if len(raw) == 0 {
		return []string{"(no object document)"}, nil
	}
	wantRaw, err := json.Marshal(want)
	if err != nil {
		return nil, err
	}
	var w, g map[string]any
	if err := json.Unmarshal(wantRaw, &w); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&g); err != nil {
		return []string{"(undecodable object)"}, nil
	}
	gs, _ := g["spec"].(map[string]any)
	if gs == nil {
		return []string{"spec(missing)"}, nil
	}
	diffs := docDiff(w["spec"], gs, "spec", benign)
	sort.Strings(diffs)
	if len(diffs) > 5 {
		diffs = diffs[:5]
	}
	return diffs, nil
}

func docDiff(want, got any, path string, benign benignFn) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{path}
		}
		var out []string
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				out = append(out, path+"."+k+"(missing)")
				continue
			}
			out = append(out, docDiff(wv, gv, path+"."+k, benign)...)
		}
		for k, gv := range g {
			if _, exp := w[k]; exp {
				continue
			}
			if !benign(path+"."+k, gv) {
				out = append(out, path+"."+k+"(unexpected)")
			}
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{path + "(length)"}
		}
		var out []string
		for i := range w {
			out = append(out, docDiff(w[i], g[i], fmt.Sprintf("%s[%d]", path, i), benign)...)
		}
		return out
	case string:
		g, ok := got.(string)
		if !ok {
			return []string{path}
		}
		if g == w || (isQuantityPath(path) && equalQuantity(g, w)) {
			return nil
		}
		return []string{path}
	case float64:
		switch g := got.(type) {
		case json.Number:
			if g.String() == fmt.Sprint(w) || fmt.Sprintf("%v", w) == g.String() {
				return nil
			}
			if f, err := g.Float64(); err == nil && f == w {
				return nil
			}
		case float64:
			if g == w {
				return nil
			}
		}
		return []string{path}
	case bool:
		if g, ok := got.(bool); ok && g == w {
			return nil
		}
		return []string{path}
	case nil:
		if got == nil {
			return nil
		}
		return []string{path}
	}
	return []string{path + "(type)"}
}

// A path with list indexes normalized, e.g. spec.containers[].name.
func normalPath(p string) string { return indexRe.ReplaceAllString(p, "[]") }

var defaultTolerationKeys = []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"}

// benignPod allows exactly the fields the API server and scheduler add to an
// unchanged Pod. A field not listed is a difference.
func benignPod(path string, v any) bool {
	norm := normalPath(path)
	str, _ := v.(string)
	switch norm {
	case "spec.dnsPolicy":
		return str == "ClusterFirst"
	case "spec.schedulerName":
		return str == "default-scheduler"
	case "spec.serviceAccount", "spec.nodeName":
		return str != ""
	case "spec.priority":
		n, ok := v.(json.Number)
		return ok && n.String() == "0"
	case "spec.preemptionPolicy":
		return str == "PreemptLowerPriority"
	case "spec.tolerations":
		list, _ := v.([]any)
		if len(list) == 0 {
			return false
		}
		for _, e := range list {
			m, _ := e.(map[string]any)
			k, _ := m["key"].(string)
			if !slices.Contains(defaultTolerationKeys, k) || m["operator"] != "Exists" || m["effect"] != "NoExecute" || len(m) != 4 {
				return false
			}
		}
		return true
	case "spec.containers[].terminationMessagePath", "spec.initContainers[].terminationMessagePath":
		return str == "/dev/termination-log"
	case "spec.containers[].terminationMessagePolicy", "spec.initContainers[].terminationMessagePolicy":
		return str == "File"
	}
	return false
}

// benignClaim allows the fields the control plane adds to a bound claim.
func benignClaim(path string, v any) bool {
	str, _ := v.(string)
	switch path {
	case "spec.volumeName":
		return str != ""
	case "spec.volumeMode":
		return str == "Filesystem"
	}
	return false
}

// benignService allows the defaults a ClusterIP Service gets. A headless Service
// ("None") is never ours.
func benignService(path string, v any) bool {
	norm := normalPath(path)
	str, _ := v.(string)
	switch norm {
	case "spec.clusterIP":
		return str != "" && str != "None"
	case "spec.clusterIPs", "spec.ipFamilies":
		_, ok := v.([]any)
		return ok
	case "spec.ipFamilyPolicy":
		return str == "SingleStack" || str == "PreferDualStack" || str == "RequireDualStack"
	case "spec.internalTrafficPolicy":
		return str == "Cluster"
	case "spec.sessionAffinity":
		return str == "None"
	}
	return false
}

// benignNone is for objects the server does not default (NetworkPolicy).
func benignNone(string, any) bool { return false }

// benignJob reuses the attempt Job's defaults.
func benignJob(jobUID, jobName string) benignFn {
	return func(path string, v any) bool { return benignDefault(path, v, jobUID, jobName) }
}

// verifyDoc compares an expected typed object's spec with the actual raw one.
func verifyDoc(what string, want any, raw []byte, benign benignFn) error {
	diffs, err := compareDoc(want, raw, benign)
	if err != nil {
		return err
	}
	if len(diffs) > 0 {
		return wrap(ErrForeignWorkload, "%s spec differs at %s", what, strings.Join(diffs, ", "))
	}
	return nil
}

// ianaPortName reports a valid IANA service name: 1-15 characters, lowercase
// letters, digits and single interior hyphens, with at least one letter.
var ianaPortRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validPortName(s string) bool {
	return len(s) >= 1 && len(s) <= 15 && ianaPortRe.MatchString(s) && strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz")
}
