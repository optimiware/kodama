package kubetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

var _ kube.Cluster = (*Fake)(nil)
var _ kube.RunCluster = (*Fake)(nil)

// Fake is an in-memory Kubernetes API with the semantics the executor depends on
// and fault injection at every call: not-found, already-exists, UID and
// resource-version preconditions, defaulting of a created Job, foreground
// deletion that can be held open, and mutations whose response is lost after
// they were applied. It is a test double of the API, not production evidence;
// the kind tests exercise the real server.
type Fake struct {
	mu      sync.Mutex
	seq     int
	Now     func() time.Time
	jobs    map[string]*kubernetes.Job
	pods    map[string]*kubernetes.Pod
	secrets map[string]*kubernetes.Secret
	pvcs    map[string]*kubernetes.PersistentVolumeClaim
	pvs     map[string]*kubernetes.PersistentVolume
	svcs    map[string]*kubernetes.Service

	// VolumeAttachments are the cluster's attachments, listed by
	// ListVolumeAttachments; tests add and remove them.
	VolumeAttachments []kubernetes.VolumeAttachment
	// HoldPods keeps deleted Pods (with a deletion timestamp) until ReleasePods is
	// called, as a graceful termination does. HoldPVs keeps the volume of a
	// deleted claim until ReleasePVs (reclaim pending). PendingClaims leaves new
	// claims unbound (volume binding waits for a consumer).
	// StorageClasses are the cluster's classes by name (GetStorageClass).
	StorageClasses map[string]kubernetes.StorageClass
	HoldPods       bool
	HoldPVs        bool
	PendingClaims  bool

	Namespaces      map[string]kubernetes.Namespace
	ServiceAccounts map[string]kubernetes.ServiceAccount // "ns/name"
	Quotas          map[string][]kubernetes.ResourceQuota
	NetPols         map[string][]kubernetes.NetworkPolicy
	// Permitted lists "ns/verb/resource[/sub]" the service accounts hold.
	Permitted map[string]bool

	// Hold keeps foreground-deleted Jobs (with a deletion timestamp) until
	// Release is called, as finalizers do.
	Hold bool

	// Fault is consulted before every call; op is "create job", "get job", ....
	Fault func(op string, n int) *Fault
	calls map[string]int
	Log   []string
}

// Fault decides what an injected failure does.
type Fault struct {
	Err error
	// After applies the mutation before failing (a lost response).
	After bool
}

// NewFake builds an empty cluster.
func NewFake() *Fake {
	return &Fake{
		Now: time.Now, jobs: map[string]*kubernetes.Job{}, pods: map[string]*kubernetes.Pod{}, secrets: map[string]*kubernetes.Secret{},
		StorageClasses: map[string]kubernetes.StorageClass{}, pvcs: map[string]*kubernetes.PersistentVolumeClaim{}, pvs: map[string]*kubernetes.PersistentVolume{}, svcs: map[string]*kubernetes.Service{},
		Namespaces: map[string]kubernetes.Namespace{}, ServiceAccounts: map[string]kubernetes.ServiceAccount{},
		Quotas: map[string][]kubernetes.ResourceQuota{}, NetPols: map[string][]kubernetes.NetworkPolicy{}, Permitted: map[string]bool{},
		calls: map[string]int{},
	}
}

func key(ns, name string) string { return ns + "/" + name }

var uidSeq atomic.Int64

func (f *Fake) uid() string {
	f.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", uidSeq.Add(1))
}

// fault counts a call and returns the injected fault. The hook runs outside the
// lock so it may use the Fake (to install a competing object, for example).
func (f *Fake) fault(op string) *Fault {
	f.mu.Lock()
	f.calls[op]++
	n, hook := f.calls[op], f.Fault
	f.Log = append(f.Log, op)
	f.mu.Unlock()
	if hook != nil {
		return hook(op, n)
	}
	return nil
}

// Calls returns how many times an operation ran.
func (f *Fake) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func apiErr(op string, kind error, status int) error {
	return &kubernetes.Error{Op: op, Kind: kind, Status: status}
}

// ---- Jobs ----

// serverJob applies the defaults a real API server adds, producing the raw
// document verification reads.
func (f *Fake) serverJob(j kubernetes.Job) kubernetes.Job {
	j.Metadata.UID = f.uid()
	j.Metadata.ResourceVersion = "1"
	j.Metadata.CreationTimestamp = f.Now().UTC().Format(time.RFC3339)
	raw, _ := json.Marshal(j)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	spec := doc["spec"].(map[string]any)
	spec["completionMode"], spec["manualSelector"], spec["podReplacementPolicy"], spec["suspend"] = "NonIndexed", false, "TerminatingOrFailed", false
	spec["selector"] = map[string]any{"matchLabels": map[string]any{"batch.kubernetes.io/controller-uid": j.Metadata.UID}}
	tpl := spec["template"].(map[string]any)
	meta := tpl["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	for _, k := range []string{"batch.kubernetes.io/controller-uid", "controller-uid"} {
		labels[k] = j.Metadata.UID
	}
	for _, k := range []string{"batch.kubernetes.io/job-name", "job-name"} {
		labels[k] = j.Metadata.Name
	}
	meta["labels"], meta["creationTimestamp"] = labels, nil
	ps := tpl["spec"].(map[string]any)
	ps["dnsPolicy"], ps["schedulerName"], ps["serviceAccount"] = "ClusterFirst", "default-scheduler", ps["serviceAccountName"]
	for _, list := range []string{"containers", "initContainers"} {
		cs, _ := ps[list].([]any)
		for _, c := range cs {
			cm := c.(map[string]any)
			cm["terminationMessagePath"], cm["terminationMessagePolicy"] = "/dev/termination-log", "File"
		}
	}
	out, _ := json.Marshal(doc)
	j.Raw = out
	return j
}

// CreateJob implements kube.Cluster.
func (f *Fake) CreateJob(_ context.Context, ns string, j kubernetes.Job) (kubernetes.Job, error) {
	flt := f.fault("create job")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return kubernetes.Job{}, flt.Err
	}
	k := key(ns, j.Metadata.Name)
	if _, ok := f.jobs[k]; ok {
		return kubernetes.Job{}, apiErr("create job", kubernetes.ErrAlreadyExists, 409)
	}
	j.Metadata.Namespace = ns
	sj := f.serverJob(j)
	f.jobs[k] = &sj
	if flt != nil {
		return kubernetes.Job{}, flt.Err
	}
	return sj, nil
}

// GetJob implements kube.Cluster.
func (f *Fake) GetJob(_ context.Context, ns, name string) (kubernetes.Job, error) {
	flt0 := f.fault("get job")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.Job{}, flt.Err
	}
	j, ok := f.jobs[key(ns, name)]
	if !ok {
		return kubernetes.Job{}, apiErr("get job", kubernetes.ErrNotFound, 404)
	}
	return *j, nil
}

func selects(labels map[string]string, selector string) bool {
	if selector == "" {
		return true
	}
	for _, part := range strings.Split(selector, ",") {
		k, v, _ := strings.Cut(part, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

// ListJobs implements kube.Cluster.
func (f *Fake) ListJobs(_ context.Context, ns, selector string) ([]kubernetes.Job, error) {
	flt0 := f.fault("list jobs")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	var out []kubernetes.Job
	for k, j := range f.jobs {
		if strings.HasPrefix(k, ns+"/") && selects(j.Metadata.Labels, selector) {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// DeleteJob implements kube.Cluster.
func (f *Fake) DeleteJob(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	flt := f.fault("delete job")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return flt.Err
	}
	j, ok := f.jobs[key(ns, name)]
	if !ok {
		return apiErr("delete job", kubernetes.ErrNotFound, 404)
	}
	if o.UID == "" || j.Metadata.UID != o.UID {
		return apiErr("delete job", kubernetes.ErrConflict, 409)
	}
	f.removeJobLocked(ns, name, j)
	if flt != nil {
		return flt.Err
	}
	return nil
}

func (f *Fake) removeJobLocked(ns, name string, j *kubernetes.Job) {
	if f.Hold {
		j.Metadata.DeletionTimestamp = f.Now().UTC().Format(time.RFC3339)
		j.Metadata.Finalizers = []string{"foregroundDeletion"}
		return
	}
	delete(f.jobs, key(ns, name))
	for k, p := range f.pods {
		for _, o := range p.Metadata.OwnerReferences {
			if o.UID == j.Metadata.UID {
				delete(f.pods, k)
			}
		}
	}
}

// Release completes every held deletion.
func (f *Fake) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, j := range f.jobs {
		if j.Metadata.DeletionTimestamp != "" {
			ns, name, _ := strings.Cut(k, "/")
			hold := f.Hold
			f.Hold = false
			f.removeJobLocked(ns, name, j)
			f.Hold = hold
		}
	}
}

// PutJob installs an arbitrary Job (a foreign or hostile object) and returns it
// as the server would.
func (f *Fake) PutJob(ns string, j kubernetes.Job) kubernetes.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	j.Metadata.Namespace = ns
	sj := f.serverJob(j)
	f.jobs[key(ns, j.Metadata.Name)] = &sj
	return sj
}

// MutateJobRaw edits the raw document of a stored Job (a foreign change).
func (f *Fake) MutateJobRaw(ns, name string, edit func(doc map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[key(ns, name)]
	var doc map[string]any
	_ = json.Unmarshal(j.Raw, &doc)
	edit(doc)
	raw, _ := json.Marshal(doc)
	// Typed fields follow the document, as they do for an object decoded from
	// the API.
	var typed kubernetes.Job
	_ = json.Unmarshal(raw, &typed)
	typed.Raw = raw
	*j = typed
}

// DropJob removes a Job without any precondition (an external deletion).
func (f *Fake) DropJob(ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, key(ns, name))
}

// SetJobStatus edits a stored Job's status.
func (f *Fake) SetJobStatus(ns, name string, edit func(*kubernetes.JobStatus)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(&f.jobs[key(ns, name)].Status)
}

// ---- Pods ----

// AddPod installs a Pod controlled by the Job.
func (f *Fake) AddPod(ns string, job kubernetes.Job, edit func(*kubernetes.Pod)) kubernetes.Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	name := job.Metadata.Name + "-" + strconv.Itoa(f.seq)
	yes := true
	p := kubernetes.Pod{
		Metadata: kubernetes.ObjectMeta{
			Name: name, Namespace: ns, UID: f.uid(), CreationTimestamp: f.Now().UTC().Format(time.RFC3339),
			Labels: map[string]string{
				"batch.kubernetes.io/controller-uid": job.Metadata.UID, "batch.kubernetes.io/job-name": job.Metadata.Name,
			},
			OwnerReferences: []kubernetes.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Metadata.Name, UID: job.Metadata.UID, Controller: &yes}},
		},
		Spec:   job.Spec.Template.Spec,
		Status: kubernetes.PodStatus{Phase: "Pending"},
	}
	for k, v := range job.Spec.Template.Metadata.Labels {
		p.Metadata.Labels[k] = v
	}
	if edit != nil {
		edit(&p)
	}
	f.pods[key(ns, name)] = &p
	return p
}

// EditPod edits a stored Pod.
func (f *Fake) EditPod(ns, name string, edit func(*kubernetes.Pod)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.pods[key(ns, name)])
}

// ListPods implements kube.Cluster.
func (f *Fake) ListPods(_ context.Context, ns, selector string) ([]kubernetes.Pod, error) {
	flt0 := f.fault("list pods")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	var out []kubernetes.Pod
	for k, p := range f.pods {
		if strings.HasPrefix(k, ns+"/") && selects(p.Metadata.Labels, selector) {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// GetPod implements kube.Cluster.
func (f *Fake) GetPod(_ context.Context, ns, name string) (kubernetes.Pod, error) {
	flt0 := f.fault("get pod")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.Pod{}, flt.Err
	}
	p, ok := f.pods[key(ns, name)]
	if !ok {
		return kubernetes.Pod{}, apiErr("get pod", kubernetes.ErrNotFound, 404)
	}
	return *p, nil
}

// DeletePod implements kube.Cluster.
func (f *Fake) DeletePod(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	flt0 := f.fault("delete pod")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return flt.Err
	}
	p, ok := f.pods[key(ns, name)]
	if !ok {
		return apiErr("delete pod", kubernetes.ErrNotFound, 404)
	}
	if o.UID == "" || p.Metadata.UID != o.UID {
		return apiErr("delete pod", kubernetes.ErrConflict, 409)
	}
	if f.HoldPods {
		p.Metadata.DeletionTimestamp = f.Now().UTC().Format(time.RFC3339)
		return nil
	}
	delete(f.pods, key(ns, name))
	return nil
}

// ---- Secrets ----

// GetSecret implements kube.Cluster.
func (f *Fake) GetSecret(_ context.Context, ns, name string) (kubernetes.Secret, error) {
	flt0 := f.fault("get secret")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.Secret{}, flt.Err
	}
	s, ok := f.secrets[key(ns, name)]
	if !ok {
		return kubernetes.Secret{}, apiErr("get secret", kubernetes.ErrNotFound, 404)
	}
	return cloneSecret(*s), nil
}

func cloneSecret(s kubernetes.Secret) kubernetes.Secret {
	out := s
	out.Metadata.Annotations = map[string]string{}
	for k, v := range s.Metadata.Annotations {
		out.Metadata.Annotations[k] = v
	}
	out.Data = map[string][]byte{}
	for k, v := range s.Data {
		out.Data[k] = append([]byte(nil), v...)
	}
	return out
}

// CreateSecret implements kube.Cluster.
func (f *Fake) CreateSecret(_ context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	flt := f.fault("create secret")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return kubernetes.Secret{}, flt.Err
	}
	if _, ok := f.secrets[key(ns, s.Metadata.Name)]; ok {
		return kubernetes.Secret{}, apiErr("create secret", kubernetes.ErrAlreadyExists, 409)
	}
	s.Metadata.Namespace, s.Metadata.UID, s.Metadata.ResourceVersion = ns, f.uid(), "1"
	s.Metadata.CreationTimestamp = f.Now().UTC().Format(time.RFC3339)
	st := cloneSecret(s)
	f.secrets[key(ns, s.Metadata.Name)] = &st
	if flt != nil {
		return kubernetes.Secret{}, flt.Err
	}
	return cloneSecret(st), nil
}

// UpdateSecret implements kube.Cluster.
func (f *Fake) UpdateSecret(_ context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	flt := f.fault("update secret")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt != nil && !flt.After {
		return kubernetes.Secret{}, flt.Err
	}
	cur, ok := f.secrets[key(ns, s.Metadata.Name)]
	if !ok {
		return kubernetes.Secret{}, apiErr("update secret", kubernetes.ErrNotFound, 404)
	}
	if s.Metadata.ResourceVersion != cur.Metadata.ResourceVersion {
		return kubernetes.Secret{}, apiErr("update secret", kubernetes.ErrConflict, 409)
	}
	n, _ := strconv.Atoi(cur.Metadata.ResourceVersion)
	s.Metadata.UID, s.Metadata.Namespace, s.Metadata.ResourceVersion = cur.Metadata.UID, ns, strconv.Itoa(n+1)
	s.Metadata.CreationTimestamp = cur.Metadata.CreationTimestamp
	st := cloneSecret(s)
	f.secrets[key(ns, s.Metadata.Name)] = &st
	if flt != nil {
		return kubernetes.Secret{}, flt.Err
	}
	return cloneSecret(st), nil
}

// DeleteSecret implements kube.Cluster.
func (f *Fake) DeleteSecret(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	flt0 := f.fault("delete secret")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return flt.Err
	}
	s, ok := f.secrets[key(ns, name)]
	if !ok {
		return apiErr("delete secret", kubernetes.ErrNotFound, 404)
	}
	if o.UID == "" || s.Metadata.UID != o.UID {
		return apiErr("delete secret", kubernetes.ErrConflict, 409)
	}
	delete(f.secrets, key(ns, name))
	return nil
}

// ListSecrets implements kube.Cluster.
func (f *Fake) ListSecrets(_ context.Context, ns, selector string) ([]kubernetes.Secret, error) {
	flt0 := f.fault("list secrets")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	var out []kubernetes.Secret
	for k, s := range f.secrets {
		if strings.HasPrefix(k, ns+"/") && selects(s.Metadata.Labels, selector) {
			out = append(out, cloneSecret(*s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// PutSecret installs an arbitrary Secret.
func (f *Fake) PutSecret(ns string, s kubernetes.Secret) kubernetes.Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.Metadata.Namespace, s.Metadata.UID, s.Metadata.ResourceVersion = ns, f.uid(), "1"
	s.Metadata.CreationTimestamp = f.Now().UTC().Format(time.RFC3339)
	st := cloneSecret(s)
	f.secrets[key(ns, s.Metadata.Name)] = &st
	return cloneSecret(st)
}

// SecretOf returns a stored Secret for assertions.
func (f *Fake) SecretOf(ns, name string) (kubernetes.Secret, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.secrets[key(ns, name)]
	if !ok {
		return kubernetes.Secret{}, false
	}
	return cloneSecret(*s), true
}

// JobOf returns a stored Job for assertions.
func (f *Fake) JobOf(ns, name string) (kubernetes.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[key(ns, name)]
	if !ok {
		return kubernetes.Job{}, false
	}
	return *j, true
}

// ---- policy objects ----

// GetNamespace implements kube.Cluster.
func (f *Fake) GetNamespace(_ context.Context, name string) (kubernetes.Namespace, error) {
	flt0 := f.fault("get namespace")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.Namespace{}, flt.Err
	}
	n, ok := f.Namespaces[name]
	if !ok {
		return kubernetes.Namespace{}, apiErr("get namespace", kubernetes.ErrNotFound, 404)
	}
	return n, nil
}

// GetServiceAccount implements kube.Cluster.
func (f *Fake) GetServiceAccount(_ context.Context, ns, name string) (kubernetes.ServiceAccount, error) {
	flt0 := f.fault("get serviceaccount")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return kubernetes.ServiceAccount{}, flt.Err
	}
	s, ok := f.ServiceAccounts[key(ns, name)]
	if !ok {
		return kubernetes.ServiceAccount{}, apiErr("get serviceaccount", kubernetes.ErrNotFound, 404)
	}
	return s, nil
}

// ListResourceQuotas implements kube.Cluster.
func (f *Fake) ListResourceQuotas(_ context.Context, ns string) ([]kubernetes.ResourceQuota, error) {
	flt0 := f.fault("list resourcequotas")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	return f.Quotas[ns], nil
}

// ListNetworkPolicies implements kube.Cluster.
func (f *Fake) ListNetworkPolicies(_ context.Context, ns string) ([]kubernetes.NetworkPolicy, error) {
	flt0 := f.fault("list networkpolicies")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return nil, flt.Err
	}
	return f.NetPols[ns], nil
}

// Allowed implements kube.Cluster.
func (f *Fake) Allowed(_ context.Context, user string, _ []string, a kubernetes.ResourceAttributes) (bool, error) {
	flt0 := f.fault("access review")
	f.mu.Lock()
	defer f.mu.Unlock()
	if flt := flt0; flt != nil {
		return false, flt.Err
	}
	k := a.Namespace + "/" + a.Verb + "/" + a.Resource
	if a.Subresource != "" {
		k += "/" + a.Subresource
	}
	return f.Permitted[k], nil
}
