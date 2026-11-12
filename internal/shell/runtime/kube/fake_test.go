// Deterministic in-memory Kubernetes fake and shared fixtures.
//
// The fake is the only "cluster" these tests ever touch: no process, container,
// network, API server or wall-clock sleep is involved. It records every call,
// supports explicit fault injection before and after an effect, and exposes
// gates so a test can hold a request at a chosen barrier and cancel it.
package kube

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

const (
	testNamespace = "tenant-acme"
	testKodamaURL = "https://kodama.example.com"
	// testImage is a digest-pinned image reference.
	testImage = "registry.example.com/tools/build@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func testTenant() id.TenantID {
	return "tenant-acme"
}

// fakeGate holds one fake operation at an explicit barrier.
type fakeGate struct {
	entered chan struct{}
	release chan struct{}
}

// fakeCluster is a deterministic in-memory Kubernetes API. All state is guarded
// by one mutex; tests inspect it through the helpers below.
type fakeCluster struct {
	mu sync.Mutex

	namespaces  map[string]Namespace
	jobs        map[string]Job
	configMaps  map[string]ConfigMap
	secrets     map[string]Secret
	claims      map[string]Claim
	pods        []Pod
	logs        map[string]map[string][]byte
	logStreams  []string
	logRequests []PodLogRequest

	version   int64
	uidSeq    int64
	calls     []string
	selectors []map[string]string

	before map[string]error
	after  map[string]error
	gates  map[string]*fakeGate

	// suspendHook models the asynchronous Job controller reaction to a
	// suspension PATCH. It runs after the patched object is stored and after the
	// PATCH answer snapshot is taken, so a caller that trusts the answer cannot
	// see the acknowledgment.
	suspendHook func()

	// keepDeletedClaims models StorageObjectInUseProtection: the delete
	// request is accepted but the claim survives, carrying a deletion
	// timestamp until its finalizers are removed.
	keepDeletedClaims   bool
	claimDeleteVersions []string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{
		namespaces: map[string]Namespace{},
		jobs:       map[string]Job{},
		configMaps: map[string]ConfigMap{},
		secrets:    map[string]Secret{},
		claims:     map[string]Claim{},
		logs:       map[string]map[string][]byte{},
		before:     map[string]error{},
		after:      map[string]error{},
		gates:      map[string]*fakeGate{},
	}
}

func fakeKey(namespace, name string) string {
	return namespace + "/" + name
}

func (f *fakeCluster) nextVersionLocked() string {
	f.version++
	return fmt.Sprintf("%d", f.version)
}

// nextUIDLocked returns a fresh immutable object UID. A real API server assigns
// a new UID to every created object and never reuses one, even after the name is
// deleted and recreated.
func (f *fakeCluster) nextUIDLocked() string {
	f.uidSeq++
	return fmt.Sprintf("uid-%d", f.uidSeq)
}

func (f *fakeCluster) addNamespace(namespace Namespace) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces[namespace.Name] = namespace
}

func (f *fakeCluster) addClaim(claim Claim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if claim.ResourceVersion == "" {
		claim.ResourceVersion = f.nextVersionLocked()
	}
	f.claims[fakeKey(claim.Namespace, claim.Name)] = cloneClaim(claim)
}

func (f *fakeCluster) addJob(job Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if job.UID == "" {
		job.UID = f.nextUIDLocked()
	}
	if job.ResourceVersion == "" {
		job.ResourceVersion = f.nextVersionLocked()
	}
	f.jobs[fakeKey(job.Namespace, job.Name)] = cloneJob(job)
}

// mutateJob changes a stored Job under the fake lock and advances its resource
// version, as an independent controller would. Only the test may acknowledge a
// suspension, move the counters or remove pods; SuspendJob itself never does.
func (f *fakeCluster) mutateJob(namespace, name string, mutate func(*Job)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeKey(namespace, name)
	job, ok := f.jobs[key]
	if !ok {
		return
	}
	mutate(&job)
	job.ResourceVersion = f.nextVersionLocked()
	f.jobs[key] = cloneJob(job)
}

func (f *fakeCluster) jobSnapshot(namespace, name string) (Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[fakeKey(namespace, name)]
	if !ok {
		return Job{}, false
	}
	return cloneJob(job), true
}

// jobMustGet returns one stored Job for a test assertion.
func (f *fakeCluster) jobMustGet(t *testing.T, namespace, name string) Job {
	t.Helper()
	job, ok := f.jobSnapshot(namespace, name)
	if !ok {
		t.Fatalf("fake kubernetes: job %q is absent", name)
	}
	return job
}

func (f *fakeCluster) removeJob(namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, fakeKey(namespace, name))
}

// cloneJob deep-copies every pointer, condition and mutable map of a Job, so a
// caller (or a test) that mutates a returned value cannot change the stored
// object.
func cloneJob(job Job) Job {
	copied := job
	copied.Labels = cloneStringMap(job.Labels)
	copied.Annotations = cloneStringMap(job.Annotations)
	copied.DeletionTimestamp = cloneTime(job.DeletionTimestamp)
	copied.Spec.Command = append([]string(nil), job.Spec.Command...)
	copied.Spec.Env = append([]EnvVar(nil), job.Spec.Env...)
	if job.Spec.Volumes != nil {
		copied.Spec.Volumes = make([]Volume, len(job.Spec.Volumes))
		for index, volume := range job.Spec.Volumes {
			volume.Items = append([]VolumeItem(nil), volume.Items...)
			copied.Spec.Volumes[index] = volume
		}
	}
	copied.Spec.PodLabels = cloneStringMap(job.Spec.PodLabels)
	copied.Spec.ImagePullSecrets = append([]string(nil), job.Spec.ImagePullSecrets...)
	if job.Spec.TTLSecondsAfterFinished != nil {
		value := *job.Spec.TTLSecondsAfterFinished
		copied.Spec.TTLSecondsAfterFinished = &value
	}
	if job.Spec.Suspend != nil {
		value := *job.Spec.Suspend
		copied.Spec.Suspend = &value
	}
	copied.Status.StartTime = cloneTime(job.Status.StartTime)
	copied.Status.CompletionTime = cloneTime(job.Status.CompletionTime)
	if job.Status.Terminating != nil {
		value := *job.Status.Terminating
		copied.Status.Terminating = &value
	}
	copied.Status.Conditions = append([]JobCondition(nil), job.Status.Conditions...)
	return copied
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// mutateClaim changes a stored claim under the fake lock and advances its
// resource version, as an independent controller would. Tests use it to model
// a concurrent Retain or a deletion request.
func (f *fakeCluster) mutateClaim(namespace, name string, mutate func(*Claim)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeKey(namespace, name)
	claim, ok := f.claims[key]
	if !ok {
		return
	}
	mutate(&claim)
	claim.ResourceVersion = f.nextVersionLocked()
	f.claims[key] = claim
}

func (f *fakeCluster) claimSnapshot(namespace, name string) (Claim, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claim, ok := f.claims[fakeKey(namespace, name)]
	if !ok {
		return Claim{}, false
	}
	return cloneClaim(claim), true
}

func cloneClaim(claim Claim) Claim {
	copied := claim
	copied.Labels = cloneStringMap(claim.Labels)
	copied.Annotations = cloneStringMap(claim.Annotations)
	copied.Finalizers = append([]string(nil), claim.Finalizers...)
	if claim.DeletionTimestamp != nil {
		deleted := *claim.DeletionTimestamp
		copied.DeletionTimestamp = &deleted
	}
	return copied
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	maps.Copy(copied, values)
	return copied
}

func (f *fakeCluster) addPod(pod Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods = append(f.pods, pod)
}

func (f *fakeCluster) setPods(pods []Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods = append([]Pod(nil), pods...)
}

// logStreamsSnapshot returns every pod-log stream the adapter requested.
func (f *fakeCluster) logStreamsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logStreams...)
}

// logRequestsSnapshot returns every pod-log read the adapter issued.
func (f *fakeCluster) logRequestsSnapshot() []PodLogRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PodLogRequest(nil), f.logRequests...)
}

// resetCallLog forgets recorded operations and log reads, so a long table test
// can assert per-fixture behavior without unbounded memory.
func (f *fakeCluster) resetCallLog() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.logStreams = nil
	f.logRequests = nil
}

func (f *fakeCluster) setPodLogs(pod, stream string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs[pod] == nil {
		f.logs[pod] = map[string][]byte{}
	}
	f.logs[pod][stream] = append([]byte(nil), data...)
}

func (f *fakeCluster) injectBefore(operation string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before[operation] = err
}

func (f *fakeCluster) injectAfter(operation string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.after[operation] = err
}

func (f *fakeCluster) clearInjection(operation string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.before, operation)
	delete(f.after, operation)
}

func (f *fakeCluster) newGate(operation string) *fakeGate {
	gate := &fakeGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates[operation] = gate
	return gate
}

// setSuspendHook installs the controller reaction a test wants after the next
// suspension PATCH. The hook runs outside the fake lock, so it may mutate the
// Job and the pod list through the ordinary helpers.
func (f *fakeCluster) setSuspendHook(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspendHook = hook
}

func (f *fakeCluster) takeSuspendHook() func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	hook := f.suspendHook
	f.suspendHook = nil
	return hook
}

// callsSnapshot returns the recorded operation names in call order.
func (f *fakeCluster) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCluster) callCount(operation string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == operation {
			count++
		}
	}
	return count
}

func (f *fakeCluster) callTotal() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeCluster) jobCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

func (f *fakeCluster) jobsSnapshot() []Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	jobs := make([]Job, 0, len(f.jobs))
	for _, job := range f.jobs {
		jobs = append(jobs, cloneJob(job))
	}
	return jobs
}

func (f *fakeCluster) podsSnapshot() []Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Pod(nil), f.pods...)
}

func (f *fakeCluster) configMapsSnapshot() []ConfigMap {
	f.mu.Lock()
	defer f.mu.Unlock()
	configMaps := make([]ConfigMap, 0, len(f.configMaps))
	for _, configMap := range f.configMaps {
		configMaps = append(configMaps, cloneConfigMap(configMap))
	}
	return configMaps
}

func cloneConfigMap(configMap ConfigMap) ConfigMap {
	copied := configMap
	copied.Labels = cloneStringMap(configMap.Labels)
	if configMap.Data != nil {
		copied.Data = make(map[string][]byte, len(configMap.Data))
		for key, value := range configMap.Data {
			copied.Data[key] = append([]byte(nil), value...)
		}
	}
	return copied
}

func (f *fakeCluster) secretsSnapshot() []Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	secrets := make([]Secret, 0, len(f.secrets))
	for _, secret := range f.secrets {
		secrets = append(secrets, secret)
	}
	return secrets
}

func (f *fakeCluster) claimExists(namespace, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.claims[fakeKey(namespace, name)]
	return ok
}

func (f *fakeCluster) removeClaim(namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, fakeKey(namespace, name))
}

// setKeepDeletedClaims makes every accepted claim delete leave the claim in
// place with a deletion timestamp, as a finalizer-protected PVC behaves.
func (f *fakeCluster) setKeepDeletedClaims(keep bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keepDeletedClaims = keep
}

// lastClaimDeleteVersion reports the conditional resource version of the most
// recent claim delete, so a test can prove the delete was version-checked.
func (f *fakeCluster) lastClaimDeleteVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.claimDeleteVersions) == 0 {
		return ""
	}
	return f.claimDeleteVersions[len(f.claimDeleteVersions)-1]
}

func (f *fakeCluster) lastSelector() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.selectors) == 0 {
		return nil
	}
	selector := f.selectors[len(f.selectors)-1]
	copied := make(map[string]string, len(selector))
	maps.Copy(copied, selector)
	return copied
}

// begin records the call, waits at an optional gate and applies a pre-effect
// fault. A cancelled context never reaches the effect.
func (f *fakeCluster) begin(ctx context.Context, operation string) error {
	f.mu.Lock()
	f.calls = append(f.calls, operation)
	beforeErr := f.before[operation]
	gate := f.gates[operation]
	f.mu.Unlock()

	if gate != nil {
		select {
		case gate.entered <- struct{}{}:
		default:
		}
		select {
		case <-gate.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return beforeErr
}

// end applies a post-effect fault: the object change is already visible, so the
// caller sees a lost response rather than a clean refusal.
func (f *fakeCluster) end(operation string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.after[operation]
}

func (f *fakeCluster) GetNamespace(ctx context.Context, name string) (Namespace, error) {
	if err := f.begin(ctx, "GetNamespace"); err != nil {
		return Namespace{}, err
	}
	f.mu.Lock()
	namespace, ok := f.namespaces[name]
	f.mu.Unlock()
	if !ok {
		return Namespace{}, fmt.Errorf("fake kubernetes: namespace %q: %w", name, ErrNotFound)
	}
	if err := f.end("GetNamespace"); err != nil {
		return Namespace{}, err
	}
	return namespace, nil
}

func (f *fakeCluster) CreateJob(ctx context.Context, job Job) (Job, error) {
	if err := f.begin(ctx, "CreateJob"); err != nil {
		return Job{}, err
	}
	f.mu.Lock()
	key := fakeKey(job.Namespace, job.Name)
	if existing, ok := f.jobs[key]; ok {
		f.mu.Unlock()
		if err := f.end("CreateJob"); err != nil {
			return Job{}, err
		}
		return cloneJob(existing), fmt.Errorf("fake kubernetes: job %q: %w", job.Name, ErrAlreadyExists)
	}
	if job.UID == "" {
		job.UID = f.nextUIDLocked()
	}
	job.ResourceVersion = f.nextVersionLocked()
	f.jobs[key] = cloneJob(job)
	created := cloneJob(job)
	f.mu.Unlock()
	if err := f.end("CreateJob"); err != nil {
		return Job{}, err
	}
	return created, nil
}

func (f *fakeCluster) GetJob(ctx context.Context, namespace, name string) (Job, error) {
	if err := f.begin(ctx, "GetJob"); err != nil {
		return Job{}, err
	}
	f.mu.Lock()
	job, ok := f.jobs[fakeKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		return Job{}, fmt.Errorf("fake kubernetes: job %q: %w", name, ErrNotFound)
	}
	if err := f.end("GetJob"); err != nil {
		return Job{}, err
	}
	return cloneJob(job), nil
}

func (f *fakeCluster) DeleteJob(ctx context.Context, namespace, name string) error {
	if err := f.begin(ctx, "DeleteJob"); err != nil {
		return err
	}
	f.mu.Lock()
	key := fakeKey(namespace, name)
	_, ok := f.jobs[key]
	delete(f.jobs, key)
	f.mu.Unlock()
	if err := f.end("DeleteJob"); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("fake kubernetes: job %q: %w", name, ErrNotFound)
	}
	return nil
}

// SuspendJob models the UID-guarded merge PATCH independently of deletion. It
// enforces the expected UID, records the suspension intent, clears any
// finished-Job TTL exactly as the server does for the driver's explicit null,
// and leaves the controller acknowledgment, counters and pod removal to explicit
// test action.
func (f *fakeCluster) SuspendJob(ctx context.Context, namespace, name, expectedUID string) (Job, error) {
	if err := f.begin(ctx, "SuspendJob"); err != nil {
		return Job{}, err
	}
	f.mu.Lock()
	key := fakeKey(namespace, name)
	job, ok := f.jobs[key]
	if !ok {
		f.mu.Unlock()
		return Job{}, fmt.Errorf("fake kubernetes: job %q: %w", name, ErrNotFound)
	}
	if expectedUID == "" || expectedUID != job.UID {
		f.mu.Unlock()
		return Job{}, fmt.Errorf("fake kubernetes: job %q UID mismatch: %w", name, ErrConflict)
	}
	suspend := true
	job.Spec.Suspend = &suspend
	// The production PATCH always sends an explicit ttlSecondsAfterFinished
	// null, which removes the field; mirror that so the fake cannot hide a Job
	// that would be TTL-deleted after it finishes.
	job.Spec.TTLSecondsAfterFinished = nil
	job.ResourceVersion = f.nextVersionLocked()
	f.jobs[key] = cloneJob(job)
	suspended := cloneJob(job)
	f.mu.Unlock()
	// The controller reaction is asynchronous: the PATCH answer above is only
	// the recorded intent. A test that wants an acknowledgement or a pod change
	// installs the hook explicitly.
	if hook := f.takeSuspendHook(); hook != nil {
		hook()
	}
	if err := f.end("SuspendJob"); err != nil {
		return Job{}, err
	}
	return suspended, nil
}

func (f *fakeCluster) ListPods(ctx context.Context, namespace string, selector map[string]string) ([]Pod, error) {
	if err := f.begin(ctx, "ListPods"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	copiedSelector := make(map[string]string, len(selector))
	maps.Copy(copiedSelector, selector)
	f.selectors = append(f.selectors, copiedSelector)
	pods := make([]Pod, 0)
	for _, pod := range f.pods {
		if pod.Namespace != namespace || !fakeMatches(pod.Labels, selector) {
			continue
		}
		pods = append(pods, pod)
	}
	f.mu.Unlock()
	if err := f.end("ListPods"); err != nil {
		return nil, err
	}
	return pods, nil
}

func (f *fakeCluster) ReadPodLogs(ctx context.Context, namespace, pod string, request PodLogRequest) (PodLogPage, error) {
	if err := f.begin(ctx, "ReadPodLogs"); err != nil {
		return PodLogPage{}, err
	}
	f.mu.Lock()
	found := false
	for _, candidate := range f.pods {
		if candidate.Namespace == namespace && candidate.Name == pod {
			found = true
			break
		}
	}
	data := f.logs[pod][request.Stream]
	f.logStreams = append(f.logStreams, request.Stream)
	f.logRequests = append(f.logRequests, request)
	f.mu.Unlock()
	if !found {
		return PodLogPage{}, fmt.Errorf("fake kubernetes: pod %q: %w", pod, ErrNotFound)
	}
	start := min(max(request.Offset, 0), int64(len(data)))
	end := min(start+int64(request.Limit), int64(len(data)))
	page := PodLogPage{
		Data:       append([]byte(nil), data[start:end]...),
		NextOffset: end,
		Truncated:  end < int64(len(data)),
	}
	if err := f.end("ReadPodLogs"); err != nil {
		return PodLogPage{}, err
	}
	return page, nil
}

func (f *fakeCluster) PutConfigMap(ctx context.Context, configMap ConfigMap) (ConfigMap, error) {
	if err := f.begin(ctx, "PutConfigMap"); err != nil {
		return ConfigMap{}, err
	}
	f.mu.Lock()
	key := fakeKey(configMap.Namespace, configMap.Name)
	if existing, ok := f.configMaps[key]; ok {
		if existing.Immutable && !fakeSameData(existing.Data, configMap.Data) {
			f.mu.Unlock()
			return ConfigMap{}, fmt.Errorf("fake kubernetes: configmap %q: %w", configMap.Name, ErrConflict)
		}
		stored := cloneConfigMap(existing)
		f.mu.Unlock()
		if err := f.end("PutConfigMap"); err != nil {
			return ConfigMap{}, err
		}
		return stored, nil
	}
	configMap.ResourceVersion = f.nextVersionLocked()
	stored := cloneConfigMap(configMap)
	f.configMaps[key] = stored
	f.mu.Unlock()
	if err := f.end("PutConfigMap"); err != nil {
		return ConfigMap{}, err
	}
	return cloneConfigMap(stored), nil
}

func (f *fakeCluster) GetConfigMap(ctx context.Context, namespace, name string) (ConfigMap, error) {
	if err := f.begin(ctx, "GetConfigMap"); err != nil {
		return ConfigMap{}, err
	}
	f.mu.Lock()
	configMap, ok := f.configMaps[fakeKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		return ConfigMap{}, fmt.Errorf("fake kubernetes: configmap %q: %w", name, ErrNotFound)
	}
	if err := f.end("GetConfigMap"); err != nil {
		return ConfigMap{}, err
	}
	return cloneConfigMap(configMap), nil
}

func (f *fakeCluster) DeleteConfigMap(ctx context.Context, namespace, name string) error {
	if err := f.begin(ctx, "DeleteConfigMap"); err != nil {
		return err
	}
	f.mu.Lock()
	key := fakeKey(namespace, name)
	_, ok := f.configMaps[key]
	delete(f.configMaps, key)
	f.mu.Unlock()
	if err := f.end("DeleteConfigMap"); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("fake kubernetes: configmap %q: %w", name, ErrNotFound)
	}
	return nil
}

func (f *fakeCluster) PutSecret(ctx context.Context, secret Secret) (Secret, error) {
	if err := f.begin(ctx, "PutSecret"); err != nil {
		return Secret{}, err
	}
	f.mu.Lock()
	key := fakeKey(secret.Namespace, secret.Name)
	if existing, ok := f.secrets[key]; ok {
		secret.ResourceVersion = existing.ResourceVersion
	}
	secret.ResourceVersion = f.nextVersionLocked()
	f.secrets[key] = secret
	stored := secret
	f.mu.Unlock()
	if err := f.end("PutSecret"); err != nil {
		return Secret{}, err
	}
	return stored, nil
}

func (f *fakeCluster) GetSecret(ctx context.Context, namespace, name string) (Secret, error) {
	if err := f.begin(ctx, "GetSecret"); err != nil {
		return Secret{}, err
	}
	f.mu.Lock()
	secret, ok := f.secrets[fakeKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		return Secret{}, fmt.Errorf("fake kubernetes: secret %q: %w", name, ErrNotFound)
	}
	if err := f.end("GetSecret"); err != nil {
		return Secret{}, err
	}
	return secret, nil
}

func (f *fakeCluster) DeleteSecret(ctx context.Context, namespace, name string) error {
	if err := f.begin(ctx, "DeleteSecret"); err != nil {
		return err
	}
	f.mu.Lock()
	key := fakeKey(namespace, name)
	_, ok := f.secrets[key]
	delete(f.secrets, key)
	f.mu.Unlock()
	if err := f.end("DeleteSecret"); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("fake kubernetes: secret %q: %w", name, ErrNotFound)
	}
	return nil
}

func (f *fakeCluster) GetClaim(ctx context.Context, namespace, name string) (Claim, error) {
	if err := f.begin(ctx, "GetClaim"); err != nil {
		return Claim{}, err
	}
	f.mu.Lock()
	claim, ok := f.claims[fakeKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		return Claim{}, fmt.Errorf("fake kubernetes: claim %q: %w", name, ErrNotFound)
	}
	if err := f.end("GetClaim"); err != nil {
		return Claim{}, err
	}
	return cloneClaim(claim), nil
}

// UpdateClaim applies a conditional claim update. A stale expected resource
// version is a conflict and never overwrites a newer Retain.
func (f *fakeCluster) UpdateClaim(ctx context.Context, claim Claim) (Claim, error) {
	if err := f.begin(ctx, "UpdateClaim"); err != nil {
		return Claim{}, err
	}
	f.mu.Lock()
	key := fakeKey(claim.Namespace, claim.Name)
	existing, ok := f.claims[key]
	if !ok {
		f.mu.Unlock()
		return Claim{}, fmt.Errorf("fake kubernetes: claim %q: %w", claim.Name, ErrNotFound)
	}
	if claim.ResourceVersion != "" && existing.ResourceVersion != claim.ResourceVersion {
		f.mu.Unlock()
		return Claim{}, fmt.Errorf("fake kubernetes: claim %q: %w", claim.Name, ErrConflict)
	}
	claim.ResourceVersion = f.nextVersionLocked()
	f.claims[key] = cloneClaim(claim)
	stored := cloneClaim(claim)
	f.mu.Unlock()
	if err := f.end("UpdateClaim"); err != nil {
		return Claim{}, err
	}
	return stored, nil
}

func (f *fakeCluster) DeleteClaim(ctx context.Context, namespace, name string) error {
	return f.DeleteClaimVersioned(ctx, namespace, name, "")
}

// DeleteClaimVersioned applies a conditional deletion. A stale expected
// resource version is a conflict, so a stale Release never deletes a claim
// retained meanwhile.
func (f *fakeCluster) DeleteClaimVersioned(ctx context.Context, namespace, name, expectedVersion string) error {
	if err := f.begin(ctx, "DeleteClaim"); err != nil {
		return err
	}
	f.mu.Lock()
	key := fakeKey(namespace, name)
	existing, ok := f.claims[key]
	f.claimDeleteVersions = append(f.claimDeleteVersions, expectedVersion)
	if ok && expectedVersion != "" && existing.ResourceVersion != expectedVersion {
		f.mu.Unlock()
		return fmt.Errorf("fake kubernetes: claim %q: %w", name, ErrConflict)
	}
	if ok && f.keepDeletedClaims {
		if existing.DeletionTimestamp == nil {
			deleted := testDeadline
			existing.DeletionTimestamp = &deleted
		}
		existing.ResourceVersion = f.nextVersionLocked()
		f.claims[key] = existing
	} else {
		delete(f.claims, key)
	}
	f.mu.Unlock()
	if err := f.end("DeleteClaim"); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("fake kubernetes: claim %q: %w", name, ErrNotFound)
	}
	return nil
}

func (f *fakeCluster) ListJobs(ctx context.Context, namespace string, selector map[string]string) ([]Job, error) {
	if err := f.begin(ctx, "ListJobs"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	copiedSelector := cloneStringMap(selector)
	f.selectors = append(f.selectors, copiedSelector)
	jobs := make([]Job, 0)
	for _, job := range f.jobs {
		if job.Namespace != namespace || !fakeMatches(job.Labels, selector) {
			continue
		}
		jobs = append(jobs, cloneJob(job))
	}
	f.mu.Unlock()
	if err := f.end("ListJobs"); err != nil {
		return nil, err
	}
	return jobs, nil
}

func fakeMatches(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func fakeSameData(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if string(right[key]) != string(value) {
			return false
		}
	}
	return true
}

// fakeClock is an injected deterministic clock. It never sleeps in real time.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	sleeps   []time.Duration
	sleepErr error
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: testDeadline}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, duration)
	c.now = c.now.Add(duration)
	sleepErr := c.sleepErr
	c.mu.Unlock()
	if sleepErr != nil {
		return sleepErr
	}
	return ctx.Err()
}

func (c *fakeClock) recordedSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func testConfig(fake *fakeCluster, clock *fakeClock) Config {
	return Config{
		Cluster:   fake,
		Namespace: testNamespace,
		TenantID:  testTenant(),
		KodamaURL: testKodamaURL,
		Policy: Policy{
			CPURequest:    "100m",
			CPULimit:      "1",
			MemoryRequest: "64Mi",
			MemoryLimit:   "256Mi",
			JobTTLSeconds: 600,
		},
		Cleanup: CleanupPolicy{Attempts: 3, Backoff: 2 * time.Second},
		Clock:   clock,
	}
}

func newTestAdapter(t *testing.T, fake *fakeCluster, clock *fakeClock) *Adapter {
	t.Helper()
	adapter, err := New(testConfig(fake, clock))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return adapter
}

func testClaimName(t *testing.T, workspace id.WorkspaceID) string {
	t.Helper()
	name, err := WorkspaceClaimName(workspace)
	if err != nil {
		t.Fatalf("WorkspaceClaimName(%q) returned error: %v", workspace, err)
	}
	return name
}

func addTestNamespace(fake *fakeCluster) {
	fake.addNamespace(Namespace{
		Name:   testNamespace,
		Labels: map[string]string{LabelTenant: string(testTenant())},
	})
}

func addTestClaim(t *testing.T, fake *fakeCluster, workspace id.WorkspaceID) {
	t.Helper()
	fake.addClaim(Claim{
		Name:      testClaimName(t, workspace),
		Namespace: testNamespace,
		Labels:    map[string]string{LabelTenant: string(testTenant()), LabelWorkspace: string(workspace)},
	})
}

func testContainer() definition.Container {
	return definition.Container{
		Image:    testImage,
		Commands: []string{"set -u", `echo "a b"`, "make build"},
		Env: []definition.EnvVar{
			{Name: "BUILD_MODE", Value: "release"},
		},
		Secrets: []id.SecretName{"API_KEY"},
		Outputs: []id.OutputKey{"out-a"},
	}
}

func testPrepareRequest() engine.PrepareRequest {
	return engine.PrepareRequest{
		Identity:    testIdentity(),
		WorkspaceID: testWorkspaceID(),
		ActionName:  "build",
		TaskID:      "task-1",
		Container:   testContainer(),
		Secrets: []engine.SecretValue{
			{Name: "API_KEY", Value: "s3cr3t"},
		},
		Inputs: map[id.InputKey]string{
			"input-a": "line1\nline2",
		},
		Outputs: map[id.ActionName]kernelOutputs{
			"build": {"out-a": "out"},
		},
		Task:       []byte(`{"id":"task-1"}`),
		Credential: "tok-abc",
	}
}

// kernelOutputs aliases the declared settled-output map for readable fixtures.
type kernelOutputs = kernel.Outputs

func testPod(identity engine.EffectIdentity, phase string) Pod {
	return Pod{
		Name:      "pod-1",
		Namespace: testNamespace,
		Labels:    map[string]string{LabelWorkload: string(identity.WorkloadID)},
		Phase:     phase,
		NodeName:  "node-1",
	}
}

func testTerminatedPod(identity engine.EffectIdentity, phase string, exitCode int) Pod {
	pod := testPod(identity, phase)
	pod.Containers = []ContainerStatus{
		{Name: "workload", Terminated: true, ExitCode: exitCode},
	}
	return pod
}

func testRunningPod(identity engine.EffectIdentity) Pod {
	return testPod(identity, "Running")
}

func testTerminatingPod(identity engine.EffectIdentity) Pod {
	pod := testRunningPod(identity)
	deleted := testDeadline.Add(time.Minute)
	pod.DeletionTimestamp = &deleted
	return pod
}

// testTerminatingTerminatedPod is a pod whose deletion was requested after its
// workload container affirmatively terminated, as TTL cleanup does.
func testTerminatingTerminatedPod(identity engine.EffectIdentity, phase string, exitCode int) Pod {
	pod := testTerminatedPod(identity, phase, exitCode)
	deleted := testDeadline.Add(time.Minute)
	pod.DeletionTimestamp = &deleted
	return pod
}

// TestJobSuspensionDriverPreservesUIDAndIsIdempotent proves the fake models a
// real suspension PATCH independently of deletion: every created Job carries an
// immutable distinct UID, the expected UID is enforced, only the suspension
// intent changes, controller counters/pods/conditions stay under test control,
// and a repeated PATCH is a no-op for the identity.
func TestJobSuspensionDriverPreservesUIDAndIsIdempotent(t *testing.T) {
	fake := newFakeCluster()
	ctx := context.Background()

	makeJob := func(name string) Job {
		created, err := fake.CreateJob(ctx, Job{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{LabelWorkload: "workload-1"},
			Spec:      JobSpec{Image: testImage},
		})
		if err != nil {
			t.Fatalf("CreateJob %q: %v", name, err)
		}
		return created
	}
	first := makeJob("job-1")
	second := makeJob("job-2")
	if first.UID == "" || second.UID == "" {
		t.Fatalf("created Jobs have no UID: %q, %q", first.UID, second.UID)
	}
	if first.UID == second.UID {
		t.Fatalf("created Jobs share UID %q", first.UID)
	}

	// A duplicate create never rewrites the immutable identity.
	reused, err := fake.CreateJob(ctx, Job{Name: "job-1", Namespace: testNamespace, Spec: JobSpec{Image: testImage}})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate create error = %v; want ErrAlreadyExists", err)
	}
	if reused.UID != first.UID {
		t.Fatalf("duplicate create returned UID %q; want the immutable %q", reused.UID, first.UID)
	}

	// Put controller evidence and a pod in place; a suspension PATCH must not
	// change either.
	one := 1
	fake.mutateJob(testNamespace, "job-1", func(job *Job) {
		job.Status.Active = 1
		job.Status.Terminating = &one
	})
	fake.addPod(testPod(testIdentity(), "Running"))
	if fake.jobCount() != 2 || len(fake.podsSnapshot()) != 1 {
		t.Fatalf("fixture = %d jobs/%d pods; want 2/1", fake.jobCount(), len(fake.podsSnapshot()))
	}

	suspended, err := fake.SuspendJob(ctx, testNamespace, "job-1", first.UID)
	if err != nil {
		t.Fatalf("SuspendJob: %v", err)
	}
	if suspended.UID != first.UID {
		t.Fatalf("suspended UID = %q; want %q", suspended.UID, first.UID)
	}
	if suspended.Spec.Suspend == nil || !*suspended.Spec.Suspend {
		t.Fatalf("spec.suspend = %v; want true", suspended.Spec.Suspend)
	}
	if suspended.Status.Active != 1 || suspended.Status.Terminating == nil || *suspended.Status.Terminating != 1 {
		t.Fatalf("suspension changed counters: %+v", suspended.Status)
	}
	if len(suspended.Status.Conditions) != 0 {
		t.Fatalf("suspension invented conditions: %+v", suspended.Status.Conditions)
	}
	if len(fake.podsSnapshot()) != 1 {
		t.Fatal("suspension removed a pod without controller action")
	}

	// Idempotent repeat: same UID, same logical state, no new Job.
	again, err := fake.SuspendJob(ctx, testNamespace, "job-1", first.UID)
	if err != nil {
		t.Fatalf("second SuspendJob: %v", err)
	}
	if again.UID != first.UID || again.Spec.Suspend == nil || !*again.Spec.Suspend {
		t.Fatalf("second suspension = %+v; want the same UID and intent", again)
	}
	if fake.jobCount() != 2 {
		t.Fatalf("repeated suspension changed the Job count to %d; want 2", fake.jobCount())
	}
	if fake.callCount("SuspendJob") != 2 {
		t.Fatalf("SuspendJob call count = %d; want 2", fake.callCount("SuspendJob"))
	}

	// A different (for example reused-name) UID is a conflict and changes
	// nothing.
	before := fake.jobMustGet(t, testNamespace, "job-1")
	if _, err := fake.SuspendJob(ctx, testNamespace, "job-1", "uid-someone-else"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong UID error = %v; want ErrConflict", err)
	}
	after := fake.jobMustGet(t, testNamespace, "job-1")
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("conflicting PATCH advanced the resource version to %q; want %q", after.ResourceVersion, before.ResourceVersion)
	}
	if _, err := fake.SuspendJob(ctx, testNamespace, "absent-job", first.UID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Job error = %v; want ErrNotFound", err)
	}
	if _, err := fake.SuspendJob(ctx, testNamespace, "job-1", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty expected UID error = %v; want ErrConflict", err)
	}
}

// TestFakeSuspensionInjectionAndDeepCopy proves the fake supports the same
// before-effect refusal and after-effect lost-response injection as every
// other operation, and that returned Jobs never alias stored pointers,
// conditions or maps.
func TestFakeSuspensionInjectionAndDeepCopy(t *testing.T) {
	fake := newFakeCluster()
	ctx := context.Background()
	created, err := fake.CreateJob(ctx, Job{
		Name:      "job-1",
		Namespace: testNamespace,
		Labels:    map[string]string{LabelWorkload: "workload-1"},
		Spec:      JobSpec{Image: testImage},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	fake.mutateJob(testNamespace, "job-1", func(job *Job) {
		zero := 0
		job.Status.Terminating = &zero
		job.Status.Conditions = []JobCondition{{Type: "Suspended", Status: "True"}}
	})

	fake.injectBefore("SuspendJob", errors.New("api server unreachable"))
	if _, err := fake.SuspendJob(ctx, testNamespace, "job-1", created.UID); err == nil {
		t.Fatal("before-effect injection did not fail the suspension")
	}
	if job := fake.jobMustGet(t, testNamespace, "job-1"); job.Spec.Suspend != nil {
		t.Fatalf("before-effect failure mutated spec.suspend: %v", job.Spec.Suspend)
	}
	fake.clearInjection("SuspendJob")

	fake.injectAfter("SuspendJob", errors.New("connection reset after write"))
	if _, err := fake.SuspendJob(ctx, testNamespace, "job-1", created.UID); err == nil {
		t.Fatal("after-effect injection did not surface a lost response")
	}
	if job := fake.jobMustGet(t, testNamespace, "job-1"); job.Spec.Suspend == nil || !*job.Spec.Suspend {
		t.Fatal("after-effect failure lost the applied suspension")
	}
	fake.clearInjection("SuspendJob")

	returned := fake.jobMustGet(t, testNamespace, "job-1")
	*returned.Spec.Suspend = false
	*returned.Status.Terminating = 7
	returned.Status.Conditions[0].Status = "False"
	returned.Labels["mutated"] = "yes"
	job := fake.jobMustGet(t, testNamespace, "job-1")
	if job.Spec.Suspend == nil || !*job.Spec.Suspend {
		t.Fatal("mutating a returned Job changed stored spec.suspend")
	}
	if job.Status.Terminating == nil || *job.Status.Terminating != 0 {
		t.Fatalf("mutating a returned Job changed stored terminating: %v", job.Status.Terminating)
	}
	if job.Status.Conditions[0].Status != "True" {
		t.Fatal("mutating a returned Job changed stored conditions")
	}
	if _, ok := job.Labels["mutated"]; ok {
		t.Fatal("mutating a returned Job changed stored labels")
	}
}

// TestFakeSuspensionControllerStateIsExplicit proves a successful fake PATCH is
// not itself a confirmation: the acknowledgment condition, counters and pod
// removal change only when the test explicitly advances the controller.
func TestFakeSuspensionControllerStateIsExplicit(t *testing.T) {
	fake := newFakeCluster()
	ctx := context.Background()
	created, err := fake.CreateJob(ctx, Job{
		Name:      "job-1",
		Namespace: testNamespace,
		Spec:      JobSpec{Image: testImage},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	fake.addPod(testPod(testIdentity(), "Running"))

	if _, err := fake.SuspendJob(ctx, testNamespace, "job-1", created.UID); err != nil {
		t.Fatalf("SuspendJob: %v", err)
	}
	job := fake.jobMustGet(t, testNamespace, "job-1")
	if len(job.Status.Conditions) != 0 {
		t.Fatalf("a bare PATCH acknowledged suspension: %+v", job.Status.Conditions)
	}
	if len(fake.podsSnapshot()) != 1 {
		t.Fatal("a bare PATCH removed the running pod")
	}

	fake.mutateJob(testNamespace, "job-1", func(job *Job) {
		zero := 0
		job.Status.Active = 0
		job.Status.Terminating = &zero
		job.Status.Conditions = []JobCondition{{Type: "Suspended", Status: "True"}}
	})
	fake.setPods(nil)
	job = fake.jobMustGet(t, testNamespace, "job-1")
	if job.Status.Conditions[0].Type != "Suspended" || job.Status.Conditions[0].Status != "True" {
		t.Fatalf("controller acknowledgment not observable: %+v", job.Status.Conditions)
	}
	if job.Status.Terminating == nil || *job.Status.Terminating != 0 {
		t.Fatalf("controller terminating counter = %v; want a present zero", job.Status.Terminating)
	}
}

// TestFakeSuspensionEnforcesUIDAcrossReplacement proves a GET-then-PATCH race
// cannot suspend a Job that was deleted and recreated under the same name: the
// PATCH waits at the gate, the name is reused with a new UID, and the stale
// expected UID is refused.
func TestFakeSuspensionEnforcesUIDAcrossReplacement(t *testing.T) {
	fake := newFakeCluster()
	ctx := context.Background()
	created, err := fake.CreateJob(ctx, Job{
		Name:      "job-1",
		Namespace: testNamespace,
		Spec:      JobSpec{Image: testImage},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	gate := fake.newGate("SuspendJob")

	result := make(chan error, 1)
	go func() {
		_, err := fake.SuspendJob(ctx, testNamespace, "job-1", created.UID)
		result <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("SuspendJob never reached the replacement barrier")
	}

	fake.removeJob(testNamespace, "job-1")
	replacement, err := fake.CreateJob(ctx, Job{
		Name:      "job-1",
		Namespace: testNamespace,
		Spec:      JobSpec{Image: testImage},
	})
	if err != nil {
		t.Fatalf("replacement create: %v", err)
	}
	if replacement.UID == created.UID {
		t.Fatalf("replacement reused UID %q", created.UID)
	}
	close(gate.release)

	select {
	case err := <-result:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("stale-UID suspension error = %v; want ErrConflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SuspendJob did not return after the barrier")
	}
	if job := fake.jobMustGet(t, testNamespace, "job-1"); job.Spec.Suspend != nil {
		t.Fatalf("stale-UID PATCH suspended the replacement: %v", job.Spec.Suspend)
	}
}
