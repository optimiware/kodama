package gitworker_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/gitworker"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// fakeCluster is an in-memory Kubernetes API for the reconciler: namespaces and
// their objects with UIDs and resource versions, UID preconditions, the
// Deployment, ReplicaSet and Pod controllers reduced to what readiness needs, a
// call log and fault injection.
type fakeCluster struct {
	t  *testing.T
	mu sync.Mutex

	uid, rv int
	calls   []string

	classes map[string]kubernetes.StorageClass
	pvs     map[string]kubernetes.PersistentVolume
	nss     map[string]kubernetes.Namespace
	sas     map[string]kubernetes.ServiceAccount
	pvcs    map[string]kubernetes.PersistentVolumeClaim
	secrets map[string]kubernetes.Secret
	svcs    map[string][]byte
	pols    map[string]kubernetes.NetworkPolicy
	deps    map[string][]byte
	rss     map[string]kubernetes.ReplicaSet
	pods    map[string]kubernetes.Pod

	// ready makes the Deployment controller start a ready Pod; false leaves the
	// rollout unobserved.
	ready bool
	// fail returns an error for an operation before it applies; lose applies it and
	// then reports an unknown outcome.
	fail map[string]error
	lose map[string]bool
	// failNth fails only the nth call (1-based, counted from the last ResetCalls)
	// of an operation.
	failNth map[string]int
	// retainPV is the reclaim policy of the volumes the fake provisions.
	retainPV bool
}

func newFake(t *testing.T) *fakeCluster {
	return &fakeCluster{
		t: t, ready: true, retainPV: true,
		classes: map[string]kubernetes.StorageClass{"fast": {Metadata: kubernetes.ObjectMeta{Name: "fast"}, Provisioner: "csi.test", ReclaimPolicy: "Retain", VolumeBindingMode: "WaitForFirstConsumer"}},
		pvs:     map[string]kubernetes.PersistentVolume{}, nss: map[string]kubernetes.Namespace{}, sas: map[string]kubernetes.ServiceAccount{},
		pvcs: map[string]kubernetes.PersistentVolumeClaim{}, secrets: map[string]kubernetes.Secret{}, svcs: map[string][]byte{},
		pols: map[string]kubernetes.NetworkPolicy{}, deps: map[string][]byte{}, rss: map[string]kubernetes.ReplicaSet{}, pods: map[string]kubernetes.Pod{},
		fail: map[string]error{}, lose: map[string]bool{}, failNth: map[string]int{},
	}
}

func key(ns, name string) string { return ns + "/" + name }

func (f *fakeCluster) nextUID() string { f.uid++; return fmt.Sprintf("uid-%d", f.uid) }
func (f *fakeCluster) nextRV() string  { f.rv++; return fmt.Sprintf("%d", f.rv) }

// Calls returns how many times op was called; Log returns every call in order.
func (f *fakeCluster) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

func (f *fakeCluster) Mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "create ") || strings.HasPrefix(c, "replace ") || strings.HasPrefix(c, "update ") || strings.HasPrefix(c, "delete ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeCluster) ResetCalls() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

// enter records the call and applies fault injection; it returns a non-nil error
// when the call must not apply.
func (f *fakeCluster) enter(op string) error {
	f.calls = append(f.calls, op)
	if err := f.fail[op]; err != nil {
		return err
	}
	if n := f.failNth[op]; n > 0 {
		c := 0
		for _, x := range f.calls {
			if x == op {
				c++
			}
		}
		if c == n {
			return &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}
		}
	}
	return nil
}

func (f *fakeCluster) leave(op string) error {
	if f.lose[op] {
		return &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}
	}
	return nil
}

func nf(op string) error { return &kubernetes.Error{Op: op, Kind: kubernetes.ErrNotFound, Status: 404} }
func exists(op string) error {
	return &kubernetes.Error{Op: op, Kind: kubernetes.ErrAlreadyExists, Status: 409}
}
func conflict(op string) error {
	return &kubernetes.Error{Op: op, Kind: kubernetes.ErrConflict, Status: 409}
}

func clone[T any](v T) T {
	var out T
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *fakeCluster) nsExists(ns string) bool { _, ok := f.nss[ns]; return ok }

// ---- storage ----

func (f *fakeCluster) GetStorageClass(_ context.Context, name string) (kubernetes.StorageClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get storageclass"); err != nil {
		return kubernetes.StorageClass{}, err
	}
	sc, ok := f.classes[name]
	if !ok {
		return kubernetes.StorageClass{}, nf("get storageclass")
	}
	return sc, nil
}

func (f *fakeCluster) GetPV(_ context.Context, name string) (kubernetes.PersistentVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get pv"); err != nil {
		return kubernetes.PersistentVolume{}, err
	}
	pv, ok := f.pvs[name]
	if !ok {
		return kubernetes.PersistentVolume{}, nf("get pv")
	}
	return pv, nil
}

// ---- namespaces ----

func (f *fakeCluster) GetNamespace(_ context.Context, name string) (kubernetes.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get namespace"); err != nil {
		return kubernetes.Namespace{}, err
	}
	n, ok := f.nss[name]
	if !ok {
		return kubernetes.Namespace{}, nf("get namespace")
	}
	return clone(n), nil
}

func (f *fakeCluster) CreateNamespace(_ context.Context, n kubernetes.Namespace) (kubernetes.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create namespace"); err != nil {
		return kubernetes.Namespace{}, err
	}
	if f.nsExists(n.Metadata.Name) {
		return kubernetes.Namespace{}, exists("create namespace")
	}
	n = clone(n)
	n.Metadata.UID, n.Metadata.ResourceVersion = f.nextUID(), f.nextRV()
	n.Metadata.Labels = merged(n.Metadata.Labels, map[string]string{"kubernetes.io/metadata.name": n.Metadata.Name})
	n.Status.Phase = "Active"
	f.nss[n.Metadata.Name] = n
	return clone(n), f.leave("create namespace")
}

func (f *fakeCluster) ReplaceNamespace(_ context.Context, n kubernetes.Namespace) (kubernetes.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("replace namespace"); err != nil {
		return kubernetes.Namespace{}, err
	}
	cur, ok := f.nss[n.Metadata.Name]
	if !ok {
		return kubernetes.Namespace{}, nf("replace namespace")
	}
	if cur.Metadata.UID != n.Metadata.UID || cur.Metadata.ResourceVersion != n.Metadata.ResourceVersion {
		return kubernetes.Namespace{}, conflict("replace namespace")
	}
	n = clone(n)
	n.Metadata.ResourceVersion, n.Status = f.nextRV(), cur.Status
	f.nss[n.Metadata.Name] = n
	return clone(n), nil
}

func (f *fakeCluster) DeleteNamespace(_ context.Context, name string, o kubernetes.DeleteOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("delete namespace"); err != nil {
		return err
	}
	cur, ok := f.nss[name]
	if !ok {
		return nf("delete namespace")
	}
	if o.UID == "" || cur.Metadata.UID != o.UID {
		return conflict("delete namespace")
	}
	delete(f.nss, name)
	for _, k := range f.allKeys() {
		if strings.HasPrefix(k, name+"/") {
			delete(f.sas, k)
			delete(f.pvcs, k)
			delete(f.secrets, k)
			delete(f.svcs, k)
			delete(f.pols, k)
			delete(f.deps, k)
			delete(f.rss, k)
			delete(f.pods, k)
		}
	}
	return f.leave("delete namespace")
}

func (f *fakeCluster) allKeys() []string {
	var ks []string
	for k := range f.sas {
		ks = append(ks, k)
	}
	for k := range f.pvcs {
		ks = append(ks, k)
	}
	for k := range f.secrets {
		ks = append(ks, k)
	}
	for k := range f.svcs {
		ks = append(ks, k)
	}
	for k := range f.pols {
		ks = append(ks, k)
	}
	for k := range f.deps {
		ks = append(ks, k)
	}
	for k := range f.rss {
		ks = append(ks, k)
	}
	for k := range f.pods {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func merged(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// ---- service accounts ----

func (f *fakeCluster) GetServiceAccount(_ context.Context, ns, name string) (kubernetes.ServiceAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get serviceaccount"); err != nil {
		return kubernetes.ServiceAccount{}, err
	}
	s, ok := f.sas[key(ns, name)]
	if !ok {
		return kubernetes.ServiceAccount{}, nf("get serviceaccount")
	}
	return clone(s), nil
}

func (f *fakeCluster) CreateServiceAccount(_ context.Context, ns string, s kubernetes.ServiceAccount) (kubernetes.ServiceAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create serviceaccount"); err != nil {
		return kubernetes.ServiceAccount{}, err
	}
	if !f.nsExists(ns) {
		return kubernetes.ServiceAccount{}, nf("create serviceaccount")
	}
	k := key(ns, s.Metadata.Name)
	if _, ok := f.sas[k]; ok {
		return kubernetes.ServiceAccount{}, exists("create serviceaccount")
	}
	s = clone(s)
	s.Metadata.Namespace, s.Metadata.UID, s.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	f.sas[k] = s
	return clone(s), f.leave("create serviceaccount")
}

// ---- claims ----

func (f *fakeCluster) GetPVC(_ context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get pvc"); err != nil {
		return kubernetes.PersistentVolumeClaim{}, err
	}
	p, ok := f.pvcs[key(ns, name)]
	if !ok {
		return kubernetes.PersistentVolumeClaim{}, nf("get pvc")
	}
	p = clone(p)
	p.Raw, _ = json.Marshal(p)
	return p, nil
}

func (f *fakeCluster) CreatePVC(_ context.Context, ns string, p kubernetes.PersistentVolumeClaim) (kubernetes.PersistentVolumeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create pvc"); err != nil {
		return kubernetes.PersistentVolumeClaim{}, err
	}
	if !f.nsExists(ns) {
		return kubernetes.PersistentVolumeClaim{}, nf("create pvc")
	}
	k := key(ns, p.Metadata.Name)
	if _, ok := f.pvcs[k]; ok {
		return kubernetes.PersistentVolumeClaim{}, exists("create pvc")
	}
	p = clone(p)
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	p.Metadata.Finalizers = []string{"kubernetes.io/pvc-protection"}
	// Bind at once: a volume that keeps its claim's identity and the class's policy.
	pv := kubernetes.PersistentVolume{Metadata: kubernetes.ObjectMeta{Name: "pv-" + p.Metadata.UID}}
	pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: ns, Name: p.Metadata.Name, UID: p.Metadata.UID}
	pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
	if f.retainPV {
		pv.Spec.PersistentVolumeReclaimPolicy = "Retain"
	}
	f.pvs[pv.Metadata.Name] = pv
	p.Spec.VolumeName, p.Status.Phase = pv.Metadata.Name, "Bound"
	f.pvcs[k] = p
	return clone(p), f.leave("create pvc")
}

// ---- secrets ----

func (f *fakeCluster) GetSecret(_ context.Context, ns, name string) (kubernetes.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get secret"); err != nil {
		return kubernetes.Secret{}, err
	}
	s, ok := f.secrets[key(ns, name)]
	if !ok {
		return kubernetes.Secret{}, nf("get secret")
	}
	return clone(s), nil
}

func (f *fakeCluster) CreateSecret(_ context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create secret"); err != nil {
		return kubernetes.Secret{}, err
	}
	if !f.nsExists(ns) {
		return kubernetes.Secret{}, nf("create secret")
	}
	k := key(ns, s.Metadata.Name)
	if _, ok := f.secrets[k]; ok {
		return kubernetes.Secret{}, exists("create secret")
	}
	s = clone(s)
	s.Metadata.Namespace, s.Metadata.UID, s.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	f.secrets[k] = s
	return clone(s), f.leave("create secret")
}

func (f *fakeCluster) UpdateSecret(_ context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("update secret"); err != nil {
		return kubernetes.Secret{}, err
	}
	k := key(ns, s.Metadata.Name)
	cur, ok := f.secrets[k]
	if !ok {
		return kubernetes.Secret{}, nf("update secret")
	}
	if cur.Metadata.ResourceVersion != s.Metadata.ResourceVersion || (s.Metadata.UID != "" && cur.Metadata.UID != s.Metadata.UID) {
		return kubernetes.Secret{}, conflict("update secret")
	}
	s = clone(s)
	s.Metadata.ResourceVersion = f.nextRV()
	f.secrets[k] = s
	return clone(s), f.leave("update secret")
}

// ---- services ----

func (f *fakeCluster) GetService(_ context.Context, ns, name string) (kubernetes.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get service"); err != nil {
		return kubernetes.Service{}, err
	}
	raw, ok := f.svcs[key(ns, name)]
	if !ok {
		return kubernetes.Service{}, nf("get service")
	}
	var s kubernetes.Service
	_ = json.Unmarshal(raw, &s)
	s.Raw = raw
	return s, nil
}

func (f *fakeCluster) CreateService(_ context.Context, ns string, s kubernetes.Service) (kubernetes.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create service"); err != nil {
		return kubernetes.Service{}, err
	}
	if !f.nsExists(ns) {
		return kubernetes.Service{}, nf("create service")
	}
	k := key(ns, s.Metadata.Name)
	if _, ok := f.svcs[k]; ok {
		return kubernetes.Service{}, exists("create service")
	}
	s = clone(s)
	s.Metadata.Namespace, s.Metadata.UID, s.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	s.Spec.ClusterIP = "10.0.0.7"
	raw, _ := json.Marshal(s)
	f.svcs[k] = raw
	s.Raw = raw
	return s, f.leave("create service")
}

// ---- network policies ----

func (f *fakeCluster) GetNetworkPolicy(_ context.Context, ns, name string) (kubernetes.NetworkPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get networkpolicy"); err != nil {
		return kubernetes.NetworkPolicy{}, err
	}
	p, ok := f.pols[key(ns, name)]
	if !ok {
		return kubernetes.NetworkPolicy{}, nf("get networkpolicy")
	}
	p = clone(p)
	p.Raw, _ = json.Marshal(p)
	return p, nil
}

func (f *fakeCluster) CreateNetworkPolicy(_ context.Context, ns string, p kubernetes.NetworkPolicy) (kubernetes.NetworkPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create networkpolicy"); err != nil {
		return kubernetes.NetworkPolicy{}, err
	}
	if !f.nsExists(ns) {
		return kubernetes.NetworkPolicy{}, nf("create networkpolicy")
	}
	k := key(ns, p.Metadata.Name)
	if _, ok := f.pols[k]; ok {
		return kubernetes.NetworkPolicy{}, exists("create networkpolicy")
	}
	p = clone(p)
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	f.pols[k] = p
	return clone(p), f.leave("create networkpolicy")
}

func (f *fakeCluster) ReplaceNetworkPolicy(_ context.Context, ns string, p kubernetes.NetworkPolicy) (kubernetes.NetworkPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("replace networkpolicy"); err != nil {
		return kubernetes.NetworkPolicy{}, err
	}
	k := key(ns, p.Metadata.Name)
	cur, ok := f.pols[k]
	if !ok {
		return kubernetes.NetworkPolicy{}, nf("replace networkpolicy")
	}
	if cur.Metadata.UID != p.Metadata.UID || cur.Metadata.ResourceVersion != p.Metadata.ResourceVersion {
		return kubernetes.NetworkPolicy{}, conflict("replace networkpolicy")
	}
	p = clone(p)
	p.Metadata.ResourceVersion = f.nextRV()
	f.pols[k] = p
	return clone(p), f.leave("replace networkpolicy")
}

func (f *fakeCluster) ListNetworkPolicies(_ context.Context, ns string) ([]kubernetes.NetworkPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("list networkpolicies"); err != nil {
		return nil, err
	}
	var out []kubernetes.NetworkPolicy
	for _, k := range f.allKeys() {
		if p, ok := f.pols[k]; ok && strings.HasPrefix(k, ns+"/") {
			out = append(out, clone(p))
		}
	}
	return out, nil
}

// ---- deployments and the controllers behind them ----

func (f *fakeCluster) GetDeployment(_ context.Context, ns, name string) (kubernetes.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("get deployment"); err != nil {
		return kubernetes.Deployment{}, err
	}
	raw, ok := f.deps[key(ns, name)]
	if !ok {
		return kubernetes.Deployment{}, nf("get deployment")
	}
	return decodeDeployment(raw), nil
}

func decodeDeployment(raw []byte) kubernetes.Deployment {
	var d kubernetes.Deployment
	_ = json.Unmarshal(raw, &d)
	var g struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &g)
	d.Generation, d.Raw = g.Metadata.Generation, raw
	return d
}

func (f *fakeCluster) CreateDeployment(_ context.Context, ns string, d kubernetes.Deployment) (kubernetes.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("create deployment"); err != nil {
		return kubernetes.Deployment{}, err
	}
	if !f.nsExists(ns) {
		return kubernetes.Deployment{}, nf("create deployment")
	}
	k := key(ns, d.Metadata.Name)
	if _, ok := f.deps[k]; ok {
		return kubernetes.Deployment{}, exists("create deployment")
	}
	d = clone(d)
	d.Metadata.Namespace, d.Metadata.UID, d.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	f.storeDeployment(k, d, 1)
	return decodeDeployment(f.deps[k]), f.leave("create deployment")
}

func (f *fakeCluster) ReplaceDeployment(_ context.Context, ns string, d kubernetes.Deployment) (kubernetes.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("replace deployment"); err != nil {
		return kubernetes.Deployment{}, err
	}
	k := key(ns, d.Metadata.Name)
	raw, ok := f.deps[k]
	if !ok {
		return kubernetes.Deployment{}, nf("replace deployment")
	}
	cur := decodeDeployment(raw)
	if cur.Metadata.UID != d.Metadata.UID || cur.Metadata.ResourceVersion != d.Metadata.ResourceVersion {
		return kubernetes.Deployment{}, conflict("replace deployment")
	}
	d = clone(d)
	d.Metadata.Namespace, d.Metadata.ResourceVersion = ns, f.nextRV()
	f.storeDeployment(k, d, cur.Generation+1)
	return decodeDeployment(f.deps[k]), f.leave("replace deployment")
}

// storeDeployment records the Deployment and runs the controller: the old
// ReplicaSet and Pod go (Recreate), a new ReplicaSet and, when ready, a Pod come.
func (f *fakeCluster) storeDeployment(k string, d kubernetes.Deployment, gen int64) {
	ns := d.Metadata.Namespace
	for rk, rs := range f.rss {
		if strings.HasPrefix(rk, ns+"/") && len(rs.Metadata.OwnerReferences) == 1 && rs.Metadata.OwnerReferences[0].UID == d.Metadata.UID {
			delete(f.rss, rk)
			for pk, p := range f.pods {
				if len(p.Metadata.OwnerReferences) == 1 && p.Metadata.OwnerReferences[0].UID == rs.Metadata.UID {
					delete(f.pods, pk)
				}
			}
		}
	}
	rsUID := f.nextUID()
	yes := true
	rs := kubernetes.ReplicaSet{Metadata: kubernetes.ObjectMeta{Name: d.Metadata.Name + "-" + rsUID, Namespace: ns, UID: rsUID, OwnerReferences: []kubernetes.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Metadata.Name, UID: d.Metadata.UID, Controller: &yes}}}}
	f.rss[key(ns, rs.Metadata.Name)] = rs
	doc := map[string]any{}
	b, _ := json.Marshal(d)
	_ = json.Unmarshal(b, &doc)
	md := doc["metadata"].(map[string]any)
	md["generation"] = gen
	// What the API server adds to a Deployment that renders everything explicitly.
	tpl := doc["spec"].(map[string]any)["template"].(map[string]any)
	tpl["metadata"].(map[string]any)["creationTimestamp"] = nil
	tspec := tpl["spec"].(map[string]any)
	tspec["serviceAccount"] = tspec["serviceAccountName"]
	for _, c := range tspec["containers"].([]any) {
		c.(map[string]any)["terminationMessagePath"] = "/dev/termination-log"
	}
	status := map[string]any{}
	if f.ready {
		pod := kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: rs.Metadata.Name + "-pod", Namespace: ns, UID: f.nextUID(), OwnerReferences: []kubernetes.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Metadata.Name, UID: rsUID, Controller: &yes}}}}
		pod.Status.Phase = "Running"
		pod.Status.Conditions = []kubernetes.PodCondition{{Type: "Ready", Status: "True"}}
		f.pods[key(ns, pod.Metadata.Name)] = pod
		status = map[string]any{"observedGeneration": gen, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1}
	}
	doc["status"] = status
	raw, _ := json.Marshal(doc)
	f.deps[k] = raw
}

func (f *fakeCluster) ListReplicaSets(_ context.Context, ns, _ string) ([]kubernetes.ReplicaSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("list replicasets"); err != nil {
		return nil, err
	}
	var out []kubernetes.ReplicaSet
	for _, k := range f.allKeys() {
		if r, ok := f.rss[k]; ok && strings.HasPrefix(k, ns+"/") {
			out = append(out, clone(r))
		}
	}
	return out, nil
}

func (f *fakeCluster) ListPods(_ context.Context, ns, _ string) ([]kubernetes.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("list pods"); err != nil {
		return nil, err
	}
	var out []kubernetes.Pod
	for _, k := range f.allKeys() {
		if p, ok := f.pods[k]; ok && strings.HasPrefix(k, ns+"/") {
			out = append(out, clone(p))
		}
	}
	return out, nil
}

// ---- test access to the stored state ----

func (f *fakeCluster) deployment(ns string) (kubernetes.Deployment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.deps[key(ns, gitworker.DeploymentName)]
	if !ok {
		return kubernetes.Deployment{}, false
	}
	return decodeDeployment(raw), true
}

// editDeployment changes the stored Deployment document (a tamper by someone
// with access to the namespace) without touching UID or resource version.
func (f *fakeCluster) editDeployment(ns string, edit func(doc map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(ns, gitworker.DeploymentName)
	doc := map[string]any{}
	_ = json.Unmarshal(f.deps[k], &doc)
	edit(doc)
	f.deps[k], _ = json.Marshal(doc)
}

func (f *fakeCluster) putPod(ns string, p kubernetes.Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.Metadata.Namespace = ns
	if p.Metadata.UID == "" {
		p.Metadata.UID = f.nextUID()
	}
	f.pods[key(ns, p.Metadata.Name)] = p
}

func (f *fakeCluster) putNamespace(n kubernetes.Namespace) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n.Metadata.UID, n.Metadata.ResourceVersion, n.Status.Phase = f.nextUID(), f.nextRV(), "Active"
	f.nss[n.Metadata.Name] = n
}

func (f *fakeCluster) putPolicy(ns string, p kubernetes.NetworkPolicy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	f.pols[key(ns, p.Metadata.Name)] = p
}

func (f *fakeCluster) putDeployment(ns string, d kubernetes.Deployment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.Metadata.Namespace, d.Metadata.UID, d.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	b, _ := json.Marshal(d)
	doc := map[string]any{}
	_ = json.Unmarshal(b, &doc)
	doc["metadata"].(map[string]any)["generation"] = 1
	f.deps[key(ns, d.Metadata.Name)], _ = json.Marshal(doc)
}

func (f *fakeCluster) putPVC(ns string, p kubernetes.PersistentVolumeClaim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.nextUID(), f.nextRV()
	f.pvcs[key(ns, p.Metadata.Name)] = p
}

func (f *fakeCluster) pvc(ns string) (kubernetes.PersistentVolumeClaim, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pvcs[key(ns, gitworker.ClaimName)]
	return p, ok
}

func (f *fakeCluster) secret(ns string) (kubernetes.Secret, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.secrets[key(ns, gitworker.SecretName)]
	return s, ok
}

func (f *fakeCluster) namespace(ns string) (kubernetes.Namespace, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nss[ns]
	return clone(n), ok
}

func (f *fakeCluster) deletePolicy(ns, name string) {
	f.mu.Lock()
	delete(f.pols, key(ns, name))
	f.mu.Unlock()
}

func (f *fakeCluster) deletePod(ns, name string) {
	f.mu.Lock()
	delete(f.pods, key(ns, name))
	f.mu.Unlock()
}

func (f *fakeCluster) setReady(v bool) { f.mu.Lock(); f.ready = v; f.mu.Unlock() }

// ---- the rest of the fixtures ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// identity is a fake IdentityProvider.
type identity struct {
	mu    sync.Mutex
	clk   *clock
	token string
	calls int
	fn    func(domain.TenantID, *gitworker.Material)
	err   error
}

func (i *identity) WorkerIdentity(_ context.Context, t domain.TenantID) (gitworker.Material, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	if i.err != nil {
		return gitworker.Material{}, i.err
	}
	m := gitworker.Material{
		TenantID: t, Token: []byte(i.token + "." + string(t)), ExpiresAt: i.clk.Now().Add(30 * time.Minute),
		PublicKeys: keyMaterial.publicPEM,
		CA:         keyMaterial.certPEM,
	}
	if i.fn != nil {
		i.fn(t, &m)
	}
	return m, nil
}

type retention struct {
	allow bool
	err   error
	calls int
}

func (r *retention) MayDeleteVolume(context.Context, domain.TenantID) (bool, error) {
	r.calls++
	return r.allow, r.err
}

type env struct {
	t    *testing.T
	f    *fakeCluster
	clk  *clock
	id   *identity
	ret  *retention
	rec  *gitworker.Reconciler
	spec gitworker.WorkerSpec
}

const testImage = "registry.example/kodama/kodama@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const testImage2 = "registry.example/kodama/kodama@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func testSpec() gitworker.WorkerSpec {
	return gitworker.WorkerSpec{
		Image: testImage,
		Resources: gitworker.Resources{
			CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi",
			EphemeralRequest: "64Mi", EphemeralLimit: "256Mi", TmpSize: "128Mi",
		},
		Storage:    gitworker.Storage{Class: "fast", Size: "20Gi"},
		ListenPort: 8443,
		ControlPlane: gitworker.ControlPlane{
			Namespace: "kodama", PodLabels: map[string]string{"app.kubernetes.io/name": "kodama-server"}, PodPort: 8080,
			URL: "https://kodama-server.kodama.svc:8080",
		},
		PodSecurityVersion: "v1.32",
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := newFake(t)
	clk := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	id := &identity{clk: clk, token: "worker-token"}
	ret := &retention{}
	rec, err := gitworker.New(gitworker.Dependencies{Cluster: f, Identity: id, Retention: ret},
		gitworker.Options{Clock: clk.Now, RWOPProvisioners: []string{"csi.test"}, StatusTTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, f: f, clk: clk, id: id, ret: ret, rec: rec, spec: testSpec()}
}

func (e *env) reconcile(tenant domain.TenantID) gitworker.Status {
	e.t.Helper()
	st, err := e.rec.Reconcile(context.Background(), tenant, e.spec)
	if err != nil {
		e.t.Fatalf("reconcile %s: %v", tenant, err)
	}
	return st
}

// keyMaterial is real key material: a public key as the key ring distributes it,
// a private key (never delivered), and a CA certificate.
var keyMaterial = func() (m struct{ publicPEM, privatePEM, pkcs8PEM, certPEM []byte }) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	m.publicPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
	ec, _ := x509.MarshalECPrivateKey(k)
	m.privatePEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ec})
	p8, _ := x509.MarshalPKCS8PrivateKey(k)
	m.pkcs8PEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		panic(err)
	}
	m.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return m
}()
