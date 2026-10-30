package gitworker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// benignFinalizers are the finalizers Kubernetes itself adds to objects of this
// kind; any other finalizer was put there by someone else.
var benignFinalizers = []string{"foregroundDeletion", "orphan", "kubernetes.io/pvc-protection"}

// chain is the ownership evidence an object is verified against: the tenant, the
// namespace and the namespace's instance UID. Labels and annotations are copyable
// evidence, so an object verifies only when all of them agree together with its
// own metadata; none of them alone is authority.
type chain struct {
	tenant domain.TenantID
	key    string
	ns     string
	// nsUID is the live namespace's UID ("" while verifying the namespace itself).
	nsUID string
}

// verifyMeta checks an object's metadata. A nil result means the object is
// provably this tenant's (labels, tenant annotation, namespace UID, no owner, no
// foreign finalizer, a recorded fingerprint); ErrForeign means it is not and must
// not be touched; ErrUnavailable means it is going away.
func (c chain) verifyMeta(m kubernetes.ObjectMeta, name, role string, namespaced bool) error {
	foreign := func(why string) error { return wrap(ErrForeign, "%s %s: %s", role, name, why) }
	if m.Name != name {
		return foreign("the name differs")
	}
	if namespaced && m.Namespace != c.ns {
		return foreign("the namespace differs")
	}
	if m.UID == "" {
		return foreign("it has no UID")
	}
	if m.Labels[LabelOwner] != ValueOwner || m.Labels[LabelTenantKey] != c.key || m.Labels[LabelObject] != role {
		return foreign("the ownership labels differ")
	}
	if m.Annotations[AnnTenant] != string(c.tenant) {
		return foreign("the tenant annotation differs")
	}
	if namespaced && (c.nsUID == "" || m.Annotations[AnnNamespaceUID] != c.nsUID) {
		return foreign("it does not belong to this instance of the namespace")
	}
	if len(m.OwnerReferences) != 0 {
		return foreign("it has an owner")
	}
	for _, f := range m.Finalizers {
		if !slices.Contains(benignFinalizers, f) {
			return foreign("it has a foreign finalizer")
		}
	}
	if !strings.HasPrefix(m.Annotations[AnnFingerprint], "sha256:") {
		return foreign("it records no fingerprint")
	}
	if m.DeletionTimestamp != "" {
		return wrap(ErrUnavailable, "%s %s is being deleted", role, name)
	}
	return nil
}

// stamp returns the metadata with the namespace UID recorded.
func stamp(m kubernetes.ObjectMeta, nsUID string) kubernetes.ObjectMeta {
	m.Annotations = merge(m.Annotations)
	if nsUID != "" {
		m.Annotations[AnnNamespaceUID] = nsUID
	}
	m.Labels = merge(m.Labels)
	return m
}

// current reports that an object's recorded fingerprint is the rendered one.
func current(live kubernetes.ObjectMeta, want kubernetes.ObjectMeta) bool {
	return live.Annotations[AnnFingerprint] == want.Annotations[AnnFingerprint]
}

// canonical is the normalized JSON form two documents are compared by.
func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "(unencodable)"
	}
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		return "(undecodable)"
	}
	out, _ := json.Marshal(x)
	return string(out)
}

// ---- document comparison ----

// benignFn decides whether a member the API server added (one this package did
// not render), at a normalized path, is a known default.
type benignFn func(path string, v any) bool

var indexRe = regexp.MustCompile(`\[[0-9]+\]`)

func normalPath(p string) string { return indexRe.ReplaceAllString(p, "[]") }

// isQuantityPath: quantities the server may spell differently (1000m is 1).
func isQuantityPath(p string) bool {
	n := normalPath(p)
	return strings.HasSuffix(n, ".resources.requests."+lastKey(n)) || strings.HasSuffix(n, ".resources.limits."+lastKey(n)) || strings.HasSuffix(n, ".emptyDir.sizeLimit")
}

func lastKey(p string) string {
	if i := strings.LastIndex(p, "."); i >= 0 {
		return p[i+1:]
	}
	return p
}

// diffDoc compares the spec this package renders with the one the server holds,
// as documents: every rendered member must be present and equal, and every member
// of the live document that was not rendered must be a default the server adds
// (benign). Anything else, subPath, mountPropagation, envFrom, a further volume
// or container included, is a difference. It returns at most five paths, never
// values.
func diffDoc(want any, raw []byte, benign benignFn) []string {
	if len(raw) == 0 {
		return []string{"(no object document)"}
	}
	wb, err := json.Marshal(want)
	if err != nil {
		return []string{"(unencodable)"}
	}
	var w, g any
	if err := json.Unmarshal(wb, &w); err != nil {
		return []string{"(unencodable)"}
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []string{"(undecodable object)"}
	}
	spec, ok := doc["spec"]
	if !ok {
		return []string{"spec(missing)"}
	}
	g = spec
	diffs := cmpDoc(w, g, "spec", benign)
	sort.Strings(diffs)
	if len(diffs) > 5 {
		diffs = diffs[:5]
	}
	return diffs
}

func cmpDoc(want, got any, path string, benign benignFn) []string {
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
			out = append(out, cmpDoc(wv, gv, path+"."+k, benign)...)
		}
		for k, gv := range g {
			if _, rendered := w[k]; !rendered && !benign(path+"."+k, gv) {
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
			out = append(out, cmpDoc(w[i], g[i], fmt.Sprintf("%s[%d]", path, i), benign)...)
		}
		return out
	case string:
		g, ok := got.(string)
		if !ok {
			return []string{path}
		}
		if g == w || (isQuantityPath(path) && canonicalQuantity(g) == canonicalQuantity(w)) {
			return nil
		}
		return []string{path}
	case float64:
		if g, ok := got.(float64); ok && g == w {
			return nil
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

// benignDeployment lists the members the API server adds to a Deployment that
// carries everything rendered here explicitly. Everything else is drift.
func benignDeployment(path string, v any) bool {
	str, _ := v.(string)
	switch normalPath(path) {
	case "spec.template.metadata.creationTimestamp":
		return v == nil
	case "spec.template.spec.serviceAccount": // the deprecated alias of serviceAccountName
		return str == ServiceAccountName
	case "spec.template.spec.containers[].terminationMessagePath":
		return str == "/dev/termination-log"
	}
	return false
}

// benignService lists the defaults of a ClusterIP Service.
func benignService(path string, v any) bool {
	str, _ := v.(string)
	switch normalPath(path) {
	case "spec.clusterIP":
		return str != "" && str != "None"
	case "spec.clusterIPs", "spec.ipFamilies":
		list, ok := v.([]any)
		return ok && len(list) > 0
	case "spec.ipFamilyPolicy":
		return str == "SingleStack" || str == "PreferDualStack" || str == "RequireDualStack"
	case "spec.internalTrafficPolicy":
		return str == "Cluster"
	case "spec.sessionAffinity":
		return str == "None"
	}
	return false
}

func benignNone(string, any) bool { return false }

// deploymentDiff returns the paths at which a live Deployment differs from the
// rendered one; none means it is the rendered one.
func deploymentDiff(want, live kubernetes.Deployment) []string {
	return diffDoc(want.Spec, live.Raw, benignDeployment)
}

func serviceDiff(want, live kubernetes.Service) []string {
	w := want.Spec
	w.ClusterIP = ""
	return diffDoc(w, live.Raw, benignService)
}

func policyDiff(want, live kubernetes.NetworkPolicy) []string {
	return diffDoc(want.Spec, live.Raw, benignNone)
}

func claimMatches(want, live kubernetes.PersistentVolumeClaim) bool {
	if len(live.Spec.AccessModes) != 1 || live.Spec.AccessModes[0] != "ReadWriteOncePod" {
		return false
	}
	if live.Spec.StorageClassName == nil || want.Spec.StorageClassName == nil || *live.Spec.StorageClassName != *want.Spec.StorageClassName {
		return false
	}
	return live.Spec.VolumeMode == "" || live.Spec.VolumeMode == "Filesystem"
}

// secretDataMatches compares the identity Secret's data to the issued material.
func secretDataMatches(live map[string][]byte, want map[string][]byte) bool {
	if len(live) != len(want) {
		return false
	}
	for k, v := range want {
		if !bytes.Equal(live[k], v) {
			return false
		}
	}
	return true
}
