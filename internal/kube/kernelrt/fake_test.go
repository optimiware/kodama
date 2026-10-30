package kernelrt

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// fakeCluster is an in-memory Kubernetes API double. It proves the mapping of
// kernel intent to API objects and back: names, labels, annotations, UID and
// resourceVersion preconditions, foreground deletion and lost responses. It is
// not a CSI driver, a CNI, a quota controller or a kubelet, and nothing here
// may be read as evidence of their behavior.
type fakeCluster struct {
	mu    sync.Mutex
	seq   int
	clock *fakeClock

	namespaces map[string]kubernetes.Namespace
	accounts   map[string]bool
	netpols    map[string][]kubernetes.NetworkPolicy
	classes    map[string]kubernetes.StorageClass
	pvs        map[string]kubernetes.PersistentVolume
	attached   []kubernetes.VolumeAttachment

	jobs, pods, secrets, pvcs, services map[string]any
	logs                                map[string]string

	// foreground keeps a deleted object terminating until finalize.
	foreground bool
	// inject decides, per operation name, whether to fail and whether the
	// mutation is applied anyway (a lost response).
	inject map[string][]injected
	hooks  map[string][]func()
	// tampers rewrite the server document of one object, as a hostile or
	// misconfigured actor could: typed fields stay as stored, the raw JSON the
	// adapter verifies changes.
	tampers map[string][]func(map[string]any)
	deletes []string // "<kind> <name> uid=<uid>" for every accepted delete
	calls   map[string]int
}

type injected struct {
	err   error
	apply bool
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newFake(clock *fakeClock) *fakeCluster {
	return &fakeCluster{
		clock:      clock,
		namespaces: map[string]kubernetes.Namespace{},
		accounts:   map[string]bool{},
		netpols:    map[string][]kubernetes.NetworkPolicy{},
		classes:    map[string]kubernetes.StorageClass{},
		pvs:        map[string]kubernetes.PersistentVolume{},
		jobs:       map[string]any{}, pods: map[string]any{}, secrets: map[string]any{}, pvcs: map[string]any{}, services: map[string]any{},
		logs:   map[string]string{},
		inject: map[string][]injected{}, hooks: map[string][]func(){}, tampers: map[string][]func(map[string]any){}, calls: map[string]int{},
	}
}

func key(ns, name string) string { return ns + "/" + name }

func (f *fakeCluster) uid() string {
	f.seq++
	return "uid-" + strconv.Itoa(f.seq)
}

// stamp assigns server-owned metadata.
func (f *fakeCluster) stamp(m *kubernetes.ObjectMeta, ns string) {
	m.Namespace = ns
	m.UID = f.uid()
	m.ResourceVersion = "1"
	m.CreationTimestamp = f.clock.Now().UTC().Format(time.RFC3339)
}

// next returns the injected fault for op, if any.
func (f *fakeCluster) next(op string) (injected, bool) {
	f.calls[op]++
	q := f.inject[op]
	if len(q) == 0 {
		return injected{}, false
	}
	f.inject[op] = q[1:]
	return q[0], true
}

func (f *fakeCluster) failNext(op string, err error, apply bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inject[op] = append(f.inject[op], injected{err: err, apply: apply})
}

func (f *fakeCluster) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func notFound(what string) error {
	return &kubernetes.Error{Op: "get " + what, Kind: kubernetes.ErrNotFound}
}

// ---- reads of the namespace and policy ----

func (f *fakeCluster) GetNamespace(_ context.Context, name string) (kubernetes.Namespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.namespaces[name]
	if !ok {
		return n, notFound("namespace")
	}
	return n, nil
}

func (f *fakeCluster) GetServiceAccount(_ context.Context, ns, name string) (kubernetes.ServiceAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.accounts[key(ns, name)] {
		return kubernetes.ServiceAccount{}, notFound("serviceaccount")
	}
	return kubernetes.ServiceAccount{Metadata: kubernetes.ObjectMeta{Name: name, Namespace: ns}}, nil
}

func (f *fakeCluster) ListNetworkPolicies(_ context.Context, ns string) ([]kubernetes.NetworkPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]kubernetes.NetworkPolicy(nil), f.netpols[ns]...), nil
}

func (f *fakeCluster) GetStorageClass(_ context.Context, name string) (kubernetes.StorageClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.classes[name]
	if !ok {
		return c, notFound("storageclass")
	}
	return c, nil
}

func (f *fakeCluster) ListPVs(context.Context) ([]kubernetes.PersistentVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []kubernetes.PersistentVolume
	for name, pv := range f.pvs {
		pv.Metadata.Name = name
		out = append(out, pv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

func (f *fakeCluster) ListVolumeAttachments(context.Context) ([]kubernetes.VolumeAttachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]kubernetes.VolumeAttachment(nil), f.attached...), nil
}

// ---- generic object helpers ----

func get[T any](f *fakeCluster, m map[string]any, op, ns, name string) (T, error) {
	f.runHook(op)
	f.mu.Lock()
	defer f.mu.Unlock()
	var zero T
	if in, ok := f.next(op); ok && in.err != nil {
		return zero, in.err
	}
	v, ok := m[key(ns, name)]
	if !ok {
		return zero, notFound(op)
	}
	return f.attach(v).(T), nil
}

// runHook runs the next hook registered for op, outside the cluster lock, so a
// test can interleave another actor at an exact point of a call sequence.
func (f *fakeCluster) runHook(op string) {
	f.mu.Lock()
	q := f.hooks[op]
	var h func()
	if len(q) > 0 {
		h, f.hooks[op] = q[0], q[1:]
	}
	f.mu.Unlock()
	if h != nil {
		h()
	}
}

func (f *fakeCluster) onNext(op string, h func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[op] = append(f.hooks[op], h)
}

// tamper registers a rewrite of the server document of one object.
func (f *fakeCluster) tamper(kind, ns, name string, mutate func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := kind + "/" + key(ns, name)
	f.tampers[k] = append(f.tampers[k], mutate)
}

// attach returns v with the server document set: the typed object marshaled and
// completed with the defaults a real API server adds, then any tampering.
func (f *fakeCluster) attach(v any) any {
	switch o := v.(type) {
	case kubernetes.Job:
		o.Raw = f.document("job", o.Metadata, o, defaultJob)
		return o
	case kubernetes.Pod:
		o.Raw = f.document("pod", o.Metadata, o, defaultPod)
		return o
	case kubernetes.PersistentVolumeClaim:
		o.Raw = f.document("pvc", o.Metadata, o, nil)
		return o
	case kubernetes.Service:
		o.Raw = f.document("service", o.Metadata, o, defaultService)
		return o
	}
	return v
}

func (f *fakeCluster) document(kind string, m kubernetes.ObjectMeta, obj any, defaults func(map[string]any)) json.RawMessage {
	raw, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		panic(err)
	}
	if defaults != nil {
		defaults(d)
	}
	for _, t := range f.tampers[kind+"/"+key(m.Namespace, m.Name)] {
		t(d)
	}
	out, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return out
}

func sub(m map[string]any, k string) map[string]any {
	x, _ := m[k].(map[string]any)
	if x == nil {
		x = map[string]any{}
		m[k] = x
	}
	return x
}

// podDefaults adds what the API server and scheduler add to a Pod spec.
func podDefaults(spec map[string]any, scheduled bool) {
	spec["serviceAccount"] = spec["serviceAccountName"]
	spec["dnsPolicy"] = "ClusterFirst"
	spec["schedulerName"] = "default-scheduler"
	spec["priority"] = 0
	spec["preemptionPolicy"] = "PreemptLowerPriority"
	spec["tolerations"] = []any{
		map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
		map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
	}
	if scheduled {
		spec["nodeName"] = "node-1"
	}
	cs, _ := spec["containers"].([]any)
	for _, c := range cs {
		cm := c.(map[string]any)
		cm["terminationMessagePath"] = "/dev/termination-log"
		cm["terminationMessagePolicy"] = "File"
	}
}

func defaultPod(d map[string]any) {
	st, _ := d["status"].(map[string]any)
	phase, _ := st["phase"].(string)
	podDefaults(sub(d, "spec"), phase != "")
}

func defaultJob(d map[string]any) {
	spec := sub(d, "spec")
	spec["completionMode"] = "NonIndexed"
	spec["suspend"] = false
	spec["podReplacementPolicy"] = "TerminatingOrFailed"
	spec["selector"] = map[string]any{"matchLabels": map[string]any{"batch.kubernetes.io/controller-uid": "x"}}
	tpl := sub(spec, "template")
	tpl["metadata"].(map[string]any)["creationTimestamp"] = nil
	podDefaults(sub(tpl, "spec"), false)
}

func defaultService(d map[string]any) {
	spec := sub(d, "spec")
	spec["clusterIP"] = "10.96.0.7"
	spec["clusterIPs"] = []any{"10.96.0.7"}
	spec["ipFamilies"] = []any{"IPv4"}
	spec["ipFamilyPolicy"] = "SingleStack"
	spec["internalTrafficPolicy"] = "Cluster"
	spec["sessionAffinity"] = "None"
}

func meta(v any) *kubernetes.ObjectMeta {
	switch o := v.(type) {
	case *kubernetes.Job:
		return &o.Metadata
	case *kubernetes.Pod:
		return &o.Metadata
	case *kubernetes.Secret:
		return &o.Metadata
	case *kubernetes.PersistentVolumeClaim:
		return &o.Metadata
	case *kubernetes.Service:
		return &o.Metadata
	}
	panic("unknown object")
}

func create[T any](f *fakeCluster, m map[string]any, op, ns string, obj T) (T, error) {
	f.runHook(op)
	f.mu.Lock()
	defer f.mu.Unlock()
	in, injectedFault := f.next(op)
	name := meta(&obj).Name
	if injectedFault && in.err != nil && !in.apply {
		return obj, in.err
	}
	if _, ok := m[key(ns, name)]; ok {
		return obj, &kubernetes.Error{Op: op, Kind: kubernetes.ErrAlreadyExists}
	}
	f.stamp(meta(&obj), ns)
	m[key(ns, name)] = obj
	if injectedFault && in.err != nil {
		return obj, in.err // applied, but the answer is lost
	}
	return obj, nil
}

// remove deletes under a UID precondition. Foreground deletes of Jobs, Pods and
// claims stay visible, terminating, until finalize.
func (f *fakeCluster) remove(kind string, m map[string]any, ns, name string, o kubernetes.DeleteOptions) error {
	f.runHook("delete " + kind)
	f.mu.Lock()
	defer f.mu.Unlock()
	op := "delete " + kind
	in, injectedFault := f.next(op)
	if injectedFault && in.err != nil && !in.apply {
		return in.err
	}
	v, ok := m[key(ns, name)]
	if !ok {
		return notFound(kind)
	}
	md := meta(copyPtr(v))
	if o.UID == "" || md.UID != o.UID {
		return &kubernetes.Error{Op: op, Kind: kubernetes.ErrConflict}
	}
	f.deletes = append(f.deletes, fmt.Sprintf("%s %s uid=%s", kind, name, o.UID))
	if f.foreground && o.Propagation == "Foreground" && (kind == "job" || kind == "pod") {
		setTerminating(m, key(ns, name), v, f.clock.Now())
	} else {
		delete(m, key(ns, name))
		if kind == "job" {
			f.cascade(ns, name)
		}
	}
	if injectedFault && in.err != nil {
		return in.err
	}
	return nil
}

func copyPtr(v any) any {
	switch o := v.(type) {
	case kubernetes.Job:
		return &o
	case kubernetes.Pod:
		return &o
	case kubernetes.Secret:
		return &o
	case kubernetes.PersistentVolumeClaim:
		return &o
	case kubernetes.Service:
		return &o
	}
	panic("unknown object")
}

func setTerminating(m map[string]any, k string, v any, now time.Time) {
	p := copyPtr(v)
	meta(p).DeletionTimestamp = now.UTC().Format(time.RFC3339)
	switch o := p.(type) {
	case *kubernetes.Job:
		m[k] = *o
	case *kubernetes.Pod:
		m[k] = *o
	}
}

// cascade deletes the Pods controlled by a Job that is gone.
func (f *fakeCluster) cascade(ns, job string) {
	for k, v := range f.pods {
		p := v.(kubernetes.Pod)
		for _, o := range p.Metadata.OwnerReferences {
			if o.Kind == "Job" && o.Name == job && p.Metadata.Namespace == ns {
				delete(f.pods, k)
			}
		}
	}
}

// finalize completes every foreground deletion, as the garbage collector would.
func (f *fakeCluster) finalize() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.jobs {
		j := v.(kubernetes.Job)
		if j.Metadata.DeletionTimestamp != "" {
			delete(f.jobs, k)
			f.cascade(j.Metadata.Namespace, j.Metadata.Name)
		}
	}
	for k, v := range f.pods {
		if v.(kubernetes.Pod).Metadata.DeletionTimestamp != "" {
			delete(f.pods, k)
		}
	}
}

func selectorMatches(sel string, labels map[string]string) bool {
	if sel == "" {
		return true
	}
	for _, term := range strings.Split(sel, ",") {
		k, v, _ := strings.Cut(term, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

// ---- Jobs ----

func (f *fakeCluster) GetJob(_ context.Context, ns, name string) (kubernetes.Job, error) {
	return get[kubernetes.Job](f, f.jobs, "get job", ns, name)
}

func (f *fakeCluster) CreateJob(_ context.Context, ns string, j kubernetes.Job) (kubernetes.Job, error) {
	return create(f, f.jobs, "create job", ns, j)
}

func (f *fakeCluster) DeleteJob(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	return f.remove("job", f.jobs, ns, name, o)
}

// ---- Pods ----

func (f *fakeCluster) ListPods(_ context.Context, ns, selector string) ([]kubernetes.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []kubernetes.Pod
	for _, v := range f.pods {
		p := v.(kubernetes.Pod)
		if p.Metadata.Namespace == ns && selectorMatches(selector, p.Metadata.Labels) {
			out = append(out, f.attach(p).(kubernetes.Pod))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

func (f *fakeCluster) GetPod(_ context.Context, ns, name string) (kubernetes.Pod, error) {
	return get[kubernetes.Pod](f, f.pods, "get pod", ns, name)
}

func (f *fakeCluster) CreatePod(_ context.Context, ns string, p kubernetes.Pod) (kubernetes.Pod, error) {
	return create(f, f.pods, "create pod", ns, p)
}

func (f *fakeCluster) DeletePod(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	return f.remove("pod", f.pods, ns, name, o)
}

func (f *fakeCluster) PodLog(_ context.Context, ns, pod, container string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.logs[key(ns, pod)+"/"+container]
	if !ok {
		return nil, notFound("log")
	}
	return []byte(l), nil
}

// ---- Secrets ----

func (f *fakeCluster) GetSecret(_ context.Context, ns, name string) (kubernetes.Secret, error) {
	return get[kubernetes.Secret](f, f.secrets, "get secret", ns, name)
}

func (f *fakeCluster) CreateSecret(_ context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	return create(f, f.secrets, "create secret", ns, s)
}

func (f *fakeCluster) UpdateSecret(_ context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	f.runHook("update secret")
	f.mu.Lock()
	defer f.mu.Unlock()
	in, injectedFault := f.next("update secret")
	if injectedFault && in.err != nil && !in.apply {
		return s, in.err
	}
	cur, ok := f.secrets[key(ns, s.Metadata.Name)]
	if !ok {
		return s, notFound("secret")
	}
	c := cur.(kubernetes.Secret)
	if s.Metadata.ResourceVersion != c.Metadata.ResourceVersion {
		return s, &kubernetes.Error{Op: "update secret", Kind: kubernetes.ErrConflict}
	}
	n, _ := strconv.Atoi(c.Metadata.ResourceVersion)
	s.Metadata.ResourceVersion = strconv.Itoa(n + 1)
	s.Metadata.UID, s.Metadata.CreationTimestamp, s.Metadata.Namespace = c.Metadata.UID, c.Metadata.CreationTimestamp, ns
	f.secrets[key(ns, s.Metadata.Name)] = s
	if injectedFault && in.err != nil {
		return s, in.err
	}
	return s, nil
}

func (f *fakeCluster) DeleteSecret(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	return f.remove("secret", f.secrets, ns, name, o)
}

func (f *fakeCluster) ListSecrets(_ context.Context, ns, selector string) ([]kubernetes.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []kubernetes.Secret
	for _, v := range f.secrets {
		s := v.(kubernetes.Secret)
		if s.Metadata.Namespace == ns && selectorMatches(selector, s.Metadata.Labels) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// ---- Claims and Services ----

func (f *fakeCluster) GetPVC(_ context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, error) {
	return get[kubernetes.PersistentVolumeClaim](f, f.pvcs, "get pvc", ns, name)
}

func (f *fakeCluster) CreatePVC(_ context.Context, ns string, p kubernetes.PersistentVolumeClaim) (kubernetes.PersistentVolumeClaim, error) {
	return create(f, f.pvcs, "create pvc", ns, p)
}

func (f *fakeCluster) DeletePVC(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	return f.remove("pvc", f.pvcs, ns, name, o)
}

func (f *fakeCluster) GetService(_ context.Context, ns, name string) (kubernetes.Service, error) {
	return get[kubernetes.Service](f, f.services, "get service", ns, name)
}

func (f *fakeCluster) CreateService(_ context.Context, ns string, s kubernetes.Service) (kubernetes.Service, error) {
	return create(f, f.services, "create service", ns, s)
}

func (f *fakeCluster) DeleteService(_ context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	return f.remove("service", f.services, ns, name, o)
}

// ---- the controllers the cluster would run ----

func (f *fakeCluster) job(ns, name string) (kubernetes.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.jobs[key(ns, name)]
	if !ok {
		return kubernetes.Job{}, false
	}
	return f.attach(v).(kubernetes.Job), true
}

func (f *fakeCluster) names(m map[string]any) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ctrl() *bool { b := true; return &b }

// startPod is the Job controller creating the Job's Pod.
func (f *fakeCluster) startPod(ns, job string) kubernetes.Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[key(ns, job)].(kubernetes.Job)
	p := kubernetes.Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: kubernetes.ObjectMeta{
			Name: job + "-x1", Labels: j.Spec.Template.Metadata.Labels, Annotations: j.Spec.Template.Metadata.Annotations,
			OwnerReferences: []kubernetes.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job, UID: j.Metadata.UID, Controller: ctrl()}},
		},
		Spec:   j.Spec.Template.Spec,
		Status: kubernetes.PodStatus{Phase: "Running", ContainerStatuses: []kubernetes.ContainerStatus{{Name: "step", State: kubernetes.ContainerState{Running: &kubernetes.ContainerRunning{}}}}},
	}
	f.stamp(&p.Metadata, ns)
	f.pods[key(ns, p.Metadata.Name)] = p
	f.logs[key(ns, p.Metadata.Name)+"/step"] = "hello from the step\n"
	j.Status.Active = 1
	f.jobs[key(ns, job)] = j
	return p
}

// finishJob completes the Job with the container's exit code.
func (f *fakeCluster) finishJob(ns, job string, exit int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[key(ns, job)].(kubernetes.Job)
	j.Status.Active = 0
	if exit == 0 {
		j.Status.Succeeded = 1
		j.Status.Conditions = []kubernetes.JobCondition{{Type: "Complete", Status: "True"}}
	} else {
		j.Status.Failed = 1
		j.Status.Conditions = []kubernetes.JobCondition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}}
	}
	f.jobs[key(ns, job)] = j
	for k, v := range f.pods {
		p := v.(kubernetes.Pod)
		if len(p.Metadata.OwnerReferences) > 0 && p.Metadata.OwnerReferences[0].Name == job {
			p.Status.Phase = "Succeeded"
			if exit != 0 {
				p.Status.Phase = "Failed"
			}
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: "step", State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: exit}}}}
			f.pods[k] = p
		}
	}
}

func (f *fakeCluster) failJob(ns, job, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[key(ns, job)].(kubernetes.Job)
	j.Status.Failed = 1
	j.Status.Conditions = []kubernetes.JobCondition{{Type: "Failed", Status: "True", Reason: reason}}
	f.jobs[key(ns, job)] = j
}

func (f *fakeCluster) setPVC(ns, name string, mutate func(*kubernetes.PersistentVolumeClaim)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pvcs[key(ns, name)].(kubernetes.PersistentVolumeClaim)
	mutate(&p)
	f.pvcs[key(ns, name)] = p
}

func (f *fakeCluster) setPod(ns, name string, mutate func(*kubernetes.Pod)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[key(ns, name)].(kubernetes.Pod)
	mutate(&p)
	f.pods[key(ns, name)] = p
}

// replace swaps an object for another with a new UID, as a delete and
// recreate between a read and a delete would.
func (f *fakeCluster) replaceJob(ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[key(ns, name)].(kubernetes.Job)
	j.Metadata.UID = f.uid()
	f.jobs[key(ns, name)] = j
}

func (f *fakeCluster) putJob(ns string, j kubernetes.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stamp(&j.Metadata, ns)
	f.jobs[key(ns, j.Metadata.Name)] = j
}

func (f *fakeCluster) deleteCount(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, d := range f.deletes {
		if strings.HasPrefix(d, kind+" ") {
			n++
		}
	}
	return n
}
