package kubetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// The run-resource side of the in-memory cluster: claims and their volumes,
// volume attachments, Services, created Pods and NetworkPolicies, with the same
// fault points, preconditions and server-side defaults as the rest of the Fake.
// Like the rest it is a double of the API server, not evidence about a CSI driver
// or a CNI.

// doc encodes an object and lets the server add its defaults to the spec, then
// returns the raw document.
func doc(v any, edit func(spec map[string]any)) json.RawMessage {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	spec, _ := m["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		m["spec"] = spec
	}
	if edit != nil {
		edit(spec)
	}
	out, _ := json.Marshal(m)
	return out
}

// ---- PersistentVolumeClaims and volumes ----

func (f *Fake) claimRaw(c kubernetes.PersistentVolumeClaim) json.RawMessage {
	return doc(c, func(spec map[string]any) {
		if c.Spec.VolumeName != "" {
			spec["volumeName"] = c.Spec.VolumeName
		}
		spec["volumeMode"] = "Filesystem"
	})
}

func (f *Fake) refreshClaimLocked(c *kubernetes.PersistentVolumeClaim) {
	c.Raw = f.claimRaw(*c)
}

// finalizeClaimsLocked completes the deletion of terminating claims no Pod uses
// (the pvc-protection finalizer) and removes their volumes unless held.
func (f *Fake) finalizeClaimsLocked() {
	for k, c := range f.pvcs {
		if c.Metadata.DeletionTimestamp == "" || f.claimInUseLocked(c.Metadata.Namespace, c.Metadata.Name) {
			continue
		}
		delete(f.pvcs, k)
		if c.Spec.VolumeName != "" && !f.HoldPVs {
			delete(f.pvs, c.Spec.VolumeName)
		}
	}
}

func (f *Fake) claimInUseLocked(ns, name string) bool {
	for _, p := range f.pods {
		if p.Metadata.Namespace != ns {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == name {
				return true
			}
		}
	}
	return false
}

// CreatePVC implements kube.RunCluster.
func (f *Fake) CreatePVC(_ context.Context, ns string, p kubernetes.PersistentVolumeClaim) (kubernetes.PersistentVolumeClaim, error) {
	flt := f.fault("create pvc")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return kubernetes.PersistentVolumeClaim{}, flt.Err
	}
	k := key(ns, p.Metadata.Name)
	if _, ok := f.pvcs[k]; ok {
		return kubernetes.PersistentVolumeClaim{}, apiErr("create pvc", kubernetes.ErrAlreadyExists, 409)
	}
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.uid(), "1"
	p.Metadata.CreationTimestamp = f.Now().UTC().Format(time.RFC3339)
	p.Metadata.Finalizers = []string{"kubernetes.io/pvc-protection"}
	if f.PendingClaims {
		p.Status.Phase = "Pending"
	} else {
		p.Status.Phase = "Bound"
		p.Spec.VolumeName = "pvc-" + p.Metadata.UID
		var pv kubernetes.PersistentVolume
		pv.Metadata = kubernetes.ObjectMeta{Name: p.Spec.VolumeName, UID: f.uid()}
		pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: ns, Name: p.Metadata.Name, UID: p.Metadata.UID}
		pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
		pv.Status.Phase = "Bound"
		f.pvs[pv.Metadata.Name] = &pv
	}
	st := p
	f.refreshClaimLocked(&st)
	f.pvcs[k] = &st
	if flt != nil {
		return kubernetes.PersistentVolumeClaim{}, flt.Err
	}
	return st, nil
}

// GetPVC implements kube.RunCluster.
func (f *Fake) GetPVC(_ context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, error) {
	flt0 := f.fault("get pvc")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.PersistentVolumeClaim{}, flt.Err
	}
	f.finalizeClaimsLocked()
	c, ok := f.pvcs[key(ns, name)]
	if !ok {
		return kubernetes.PersistentVolumeClaim{}, apiErr("get pvc", kubernetes.ErrNotFound, 404)
	}
	return *c, nil
}

// DeletePVC implements kube.RunCluster. A claim a Pod uses stays terminating.
func (f *Fake) DeletePVC(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	flt := f.fault("delete pvc")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return flt.Err
	}
	c, ok := f.pvcs[key(ns, name)]
	if !ok {
		return apiErr("delete pvc", kubernetes.ErrNotFound, 404)
	}
	if o.UID == "" || c.Metadata.UID != o.UID {
		return apiErr("delete pvc", kubernetes.ErrConflict, 409)
	}
	c.Metadata.DeletionTimestamp = f.Now().UTC().Format(time.RFC3339)
	f.finalizeClaimsLocked()
	if flt != nil {
		return flt.Err
	}
	return nil
}

// GetPV implements kube.RunCluster.
func (f *Fake) GetPV(_ context.Context, name string) (kubernetes.PersistentVolume, error) {
	flt0 := f.fault("get pv")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.PersistentVolume{}, flt.Err
	}
	pv, ok := f.pvs[name]
	if !ok {
		return kubernetes.PersistentVolume{}, apiErr("get pv", kubernetes.ErrNotFound, 404)
	}
	return *pv, nil
}

// ListPVs implements kube.RunCluster.
func (f *Fake) ListPVs(context.Context) ([]kubernetes.PersistentVolume, error) {
	flt0 := f.fault("list persistentvolumes")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	var out []kubernetes.PersistentVolume
	for _, pv := range f.pvs {
		out = append(out, *pv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// ListVolumeAttachments implements kube.RunCluster.
func (f *Fake) ListVolumeAttachments(context.Context) ([]kubernetes.VolumeAttachment, error) {
	flt0 := f.fault("list volumeattachments")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	return append([]kubernetes.VolumeAttachment(nil), f.VolumeAttachments...), nil
}

// Attach adds a VolumeAttachment of the volume (a node may use it).
func (f *Fake) Attach(volume string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var va kubernetes.VolumeAttachment
	va.Metadata.Name = "csi-" + volume
	va.Spec.Source.PersistentVolumeName = &volume
	va.Status.Attached = true
	f.VolumeAttachments = append(f.VolumeAttachments, va)
}

// Detach removes the volume's attachments.
func (f *Fake) Detach(volume string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []kubernetes.VolumeAttachment
	for _, va := range f.VolumeAttachments {
		if va.Spec.Source.PersistentVolumeName == nil || *va.Spec.Source.PersistentVolumeName != volume {
			keep = append(keep, va)
		}
	}
	f.VolumeAttachments = keep
}

// ReleasePVs completes the deletion of volumes whose claim is gone.
func (f *Fake) ReleasePVs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, pv := range f.pvs {
		if pv.Spec.ClaimRef == nil {
			continue
		}
		if _, ok := f.pvcs[key(pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)]; !ok {
			delete(f.pvs, name)
		}
	}
}

// PVCOf returns a stored claim for assertions.
func (f *Fake) PVCOf(ns, name string) (kubernetes.PersistentVolumeClaim, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalizeClaimsLocked()
	c, ok := f.pvcs[key(ns, name)]
	if !ok {
		return kubernetes.PersistentVolumeClaim{}, false
	}
	return *c, true
}

// PVOf returns a stored volume.
func (f *Fake) PVOf(name string) (kubernetes.PersistentVolume, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pv, ok := f.pvs[name]
	if !ok {
		return kubernetes.PersistentVolume{}, false
	}
	return *pv, true
}

// PutPVC installs an arbitrary claim (a foreign object) and returns it as stored.
func (f *Fake) PutPVC(ns string, p kubernetes.PersistentVolumeClaim) kubernetes.PersistentVolumeClaim {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.uid(), "1"
	if p.Status.Phase == "" {
		p.Status.Phase = "Bound"
	}
	f.refreshClaimLocked(&p)
	f.pvcs[key(ns, p.Metadata.Name)] = &p
	return p
}

// DropPVC removes a claim without any precondition.
func (f *Fake) DropPVC(ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pvcs, key(ns, name))
}

// ---- Services ----

func (f *Fake) serviceRaw(s kubernetes.Service) json.RawMessage {
	return doc(s, func(spec map[string]any) {
		spec["clusterIP"], spec["clusterIPs"] = s.Spec.ClusterIP, []any{s.Spec.ClusterIP}
		spec["ipFamilies"], spec["ipFamilyPolicy"] = []any{"IPv4"}, "SingleStack"
		spec["internalTrafficPolicy"], spec["sessionAffinity"] = "Cluster", "None"
	})
}

// CreateService implements kube.RunCluster.
func (f *Fake) CreateService(_ context.Context, ns string, s kubernetes.Service) (kubernetes.Service, error) {
	flt := f.fault("create service")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return kubernetes.Service{}, flt.Err
	}
	k := key(ns, s.Metadata.Name)
	if _, ok := f.svcs[k]; ok {
		return kubernetes.Service{}, apiErr("create service", kubernetes.ErrAlreadyExists, 409)
	}
	s.Metadata.Namespace, s.Metadata.UID, s.Metadata.ResourceVersion = ns, f.uid(), "1"
	s.Spec.ClusterIP = fmt.Sprintf("10.96.%d.%d", len(f.svcs)/250, len(f.svcs)%250+1)
	s.Raw = f.serviceRaw(s)
	f.svcs[k] = &s
	if flt != nil {
		return kubernetes.Service{}, flt.Err
	}
	return s, nil
}

// GetService implements kube.RunCluster.
func (f *Fake) GetService(_ context.Context, ns, name string) (kubernetes.Service, error) {
	flt0 := f.fault("get service")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.Service{}, flt.Err
	}
	s, ok := f.svcs[key(ns, name)]
	if !ok {
		return kubernetes.Service{}, apiErr("get service", kubernetes.ErrNotFound, 404)
	}
	return *s, nil
}

// DeleteService implements kube.RunCluster.
func (f *Fake) DeleteService(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	flt := f.fault("delete service")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return flt.Err
	}
	s, ok := f.svcs[key(ns, name)]
	if !ok {
		return apiErr("delete service", kubernetes.ErrNotFound, 404)
	}
	if o.UID == "" || s.Metadata.UID != o.UID {
		return apiErr("delete service", kubernetes.ErrConflict, 409)
	}
	delete(f.svcs, key(ns, name))
	if flt != nil {
		return flt.Err
	}
	return nil
}

// ServiceOf returns a stored Service.
func (f *Fake) ServiceOf(ns, name string) (kubernetes.Service, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.svcs[key(ns, name)]
	if !ok {
		return kubernetes.Service{}, false
	}
	return *s, true
}

// PutService installs an arbitrary Service.
func (f *Fake) PutService(ns string, s kubernetes.Service) kubernetes.Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.Metadata.Namespace, s.Metadata.UID, s.Metadata.ResourceVersion = ns, f.uid(), "1"
	s.Spec.ClusterIP = "10.96.250.1"
	s.Raw = f.serviceRaw(s)
	f.svcs[key(ns, s.Metadata.Name)] = &s
	return s
}

// ---- Pods (create) ----

func podRaw(p kubernetes.Pod) json.RawMessage {
	return doc(p, func(spec map[string]any) {
		spec["dnsPolicy"], spec["schedulerName"], spec["priority"], spec["preemptionPolicy"] = "ClusterFirst", "default-scheduler", 0, "PreemptLowerPriority"
		spec["serviceAccount"] = spec["serviceAccountName"]
		spec["tolerations"] = []any{
			map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
			map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
		}
		for _, list := range []string{"containers", "initContainers"} {
			cs, _ := spec[list].([]any)
			for _, c := range cs {
				cm := c.(map[string]any)
				cm["terminationMessagePath"], cm["terminationMessagePolicy"] = "/dev/termination-log", "File"
			}
		}
	})
}

// CreatePod implements kube.RunCluster. The Pod is Pending until a test edits its
// status.
func (f *Fake) CreatePod(_ context.Context, ns string, p kubernetes.Pod) (kubernetes.Pod, error) {
	flt := f.fault("create pod")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return kubernetes.Pod{}, flt.Err
	}
	k := key(ns, p.Metadata.Name)
	if _, ok := f.pods[k]; ok {
		return kubernetes.Pod{}, apiErr("create pod", kubernetes.ErrAlreadyExists, 409)
	}
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.uid(), "1"
	p.Metadata.CreationTimestamp = f.Now().UTC().Format(time.RFC3339)
	p.Status = kubernetes.PodStatus{Phase: "Pending"}
	p.Raw = podRaw(p)
	f.pods[k] = &p
	if flt != nil {
		return kubernetes.Pod{}, flt.Err
	}
	return p, nil
}

// PutPod installs an arbitrary Pod (a foreign or hostile object).
func (f *Fake) PutPod(ns string, p kubernetes.Pod) kubernetes.Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.uid(), "1"
	if p.Status.Phase == "" {
		p.Status.Phase = "Pending"
	}
	p.Raw = podRaw(p)
	f.pods[key(ns, p.Metadata.Name)] = &p
	return p
}

// PodOf returns a stored Pod.
func (f *Fake) PodOf(ns, name string) (kubernetes.Pod, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[key(ns, name)]
	if !ok {
		return kubernetes.Pod{}, false
	}
	return *p, true
}

// ReleasePods completes every held Pod deletion.
func (f *Fake) ReleasePods() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, p := range f.pods {
		if p.Metadata.DeletionTimestamp != "" {
			delete(f.pods, k)
		}
	}
	f.finalizeClaimsLocked()
}

// Ready marks a Pod running and ready.
func (f *Fake) Ready(ns, name string) {
	f.EditPod(ns, name, func(p *kubernetes.Pod) {
		yes := true
		p.Status.Phase = "Running"
		p.Status.Conditions = []kubernetes.PodCondition{{Type: "Ready", Status: "True"}}
		p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: "workload", Started: &yes, Ready: true, State: kubernetes.ContainerState{Running: &kubernetes.ContainerRunning{}}}}
	})
}

// MutateRaw edits the raw document of a stored claim, Service, Pod or policy
// ("pvc", "service", "pod", "policy") the way a foreign change would, keeping
// the typed fields in step with it.
func (f *Fake) MutateRaw(kind, ns, name string, edit func(doc map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply := func(raw json.RawMessage, into any) json.RawMessage {
		var d map[string]any
		_ = json.Unmarshal(raw, &d)
		edit(d)
		out, _ := json.Marshal(d)
		_ = json.Unmarshal(out, into)
		return out
	}
	switch kind {
	case "pvc":
		c := f.pvcs[key(ns, name)]
		raw := apply(c.Raw, c)
		c.Raw = raw
	case "service":
		s := f.svcs[key(ns, name)]
		s.Raw = apply(s.Raw, s)
	case "pod":
		p := f.pods[key(ns, name)]
		p.Raw = apply(p.Raw, p)
	case "policy":
		pols := f.NetPols[ns]
		for i := range pols {
			if pols[i].Metadata.Name == name {
				raw, _ := json.Marshal(pols[i])
				pols[i].Raw = apply(raw, &pols[i])
			}
		}
	}
}

// ---- NetworkPolicies ----

// GetNetworkPolicy implements kube.RunCluster.
func (f *Fake) GetNetworkPolicy(_ context.Context, ns, name string) (kubernetes.NetworkPolicy, error) {
	flt0 := f.fault("get networkpolicy")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.NetworkPolicy{}, flt.Err
	}
	for _, p := range f.NetPols[ns] {
		if p.Metadata.Name == name {
			return withRaw(p), nil
		}
	}
	return kubernetes.NetworkPolicy{}, apiErr("get networkpolicy", kubernetes.ErrNotFound, 404)
}

func withRaw(p kubernetes.NetworkPolicy) kubernetes.NetworkPolicy {
	if len(p.Raw) == 0 {
		p.Raw = doc(p, nil)
	}
	return p
}

// SelectNetworkPolicies implements kube.RunCluster.
func (f *Fake) SelectNetworkPolicies(_ context.Context, ns, selector string) ([]kubernetes.NetworkPolicy, error) {
	flt0 := f.fault("select networkpolicies")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	var out []kubernetes.NetworkPolicy
	for _, p := range f.NetPols[ns] {
		if selects(p.Metadata.Labels, selector) {
			out = append(out, withRaw(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// CreateNetworkPolicy implements kube.RunCluster.
func (f *Fake) CreateNetworkPolicy(_ context.Context, ns string, p kubernetes.NetworkPolicy) (kubernetes.NetworkPolicy, error) {
	flt := f.fault("create networkpolicy")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return kubernetes.NetworkPolicy{}, flt.Err
	}
	for _, e := range f.NetPols[ns] {
		if e.Metadata.Name == p.Metadata.Name {
			return kubernetes.NetworkPolicy{}, apiErr("create networkpolicy", kubernetes.ErrAlreadyExists, 409)
		}
	}
	p.Metadata.Namespace, p.Metadata.UID, p.Metadata.ResourceVersion = ns, f.uid(), "1"
	p.Raw = doc(p, nil)
	f.NetPols[ns] = append(f.NetPols[ns], p)
	if flt != nil {
		return kubernetes.NetworkPolicy{}, flt.Err
	}
	return p, nil
}

// DeleteNetworkPolicy implements kube.RunCluster.
func (f *Fake) DeleteNetworkPolicy(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	flt := f.fault("delete networkpolicy")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return flt.Err
	}
	pols := f.NetPols[ns]
	for i, p := range pols {
		if p.Metadata.Name != name {
			continue
		}
		if o.UID == "" || p.Metadata.UID != o.UID {
			return apiErr("delete networkpolicy", kubernetes.ErrConflict, 409)
		}
		f.NetPols[ns] = append(append([]kubernetes.NetworkPolicy(nil), pols[:i]...), pols[i+1:]...)
		if flt != nil {
			return flt.Err
		}
		return nil
	}
	return apiErr("delete networkpolicy", kubernetes.ErrNotFound, 404)
}

// PoliciesWith lists the names of the namespace's policies carrying a label.
func (f *Fake) PoliciesWith(ns, label, value string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.NetPols[ns] {
		if p.Metadata.Labels[label] == value {
			out = append(out, p.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Names lists the names of every stored object of a kind ("pod", "pvc", "service",
// "job", "secret") of a namespace, sorted: the inventory a teardown test checks.
func (f *Fake) Names(kind, ns string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	add := func(k string) {
		if strings.HasPrefix(k, ns+"/") {
			out = append(out, strings.TrimPrefix(k, ns+"/"))
		}
	}
	switch kind {
	case "pod":
		for k := range f.pods {
			add(k)
		}
	case "pvc":
		for k := range f.pvcs {
			add(k)
		}
	case "service":
		for k := range f.svcs {
			add(k)
		}
	case "job":
		for k := range f.jobs {
			add(k)
		}
	case "secret":
		for k := range f.secrets {
			add(k)
		}
	case "policy":
		for _, p := range f.NetPols[ns] {
			out = append(out, p.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out
}

// GetStorageClass implements kube.RunCluster.
func (f *Fake) GetStorageClass(_ context.Context, name string) (kubernetes.StorageClass, error) {
	flt0 := f.fault("get storageclass")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.StorageClass{}, flt.Err
	}
	sc, ok := f.StorageClasses[name]
	if !ok {
		return kubernetes.StorageClass{}, apiErr("get storageclass", kubernetes.ErrNotFound, 404)
	}
	return sc, nil
}

// PutStorageClass installs a class.
func (f *Fake) PutStorageClass(name, bindingMode, reclaim string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StorageClasses[name] = kubernetes.StorageClass{
		Metadata: kubernetes.ObjectMeta{Name: name}, Provisioner: "csi.test.invalid", ReclaimPolicy: reclaim, VolumeBindingMode: bindingMode,
	}
}

// BindClaim binds a pending claim to a new volume, as the provisioner does after
// the claim's first consumer was scheduled.
func (f *Fake) BindClaim(ns, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.pvcs[key(ns, name)]
	c.Status.Phase = "Bound"
	c.Spec.VolumeName = "pvc-" + c.Metadata.UID
	var pv kubernetes.PersistentVolume
	pv.Metadata = kubernetes.ObjectMeta{Name: c.Spec.VolumeName, UID: f.uid()}
	pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: ns, Name: name, UID: c.Metadata.UID}
	pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
	pv.Status.Phase = "Bound"
	f.pvs[pv.Metadata.Name] = &pv
	f.refreshClaimLocked(c)
	return c.Spec.VolumeName
}

// PutPV installs a volume bound to the given claim (a provisioning that finished
// after its claim was deleted).
func (f *Fake) PutPV(name, ns, claim, claimUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pv kubernetes.PersistentVolume
	pv.Metadata = kubernetes.ObjectMeta{Name: name, UID: f.uid()}
	pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: ns, Name: claim, UID: claimUID}
	pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
	f.pvs[name] = &pv
}

// DropPV removes a volume without any precondition (its reclaim finished).
func (f *Fake) DropPV(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pvs, name)
}
