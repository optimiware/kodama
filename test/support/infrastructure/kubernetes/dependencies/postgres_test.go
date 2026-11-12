package kubedeps

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// fakeCluster is the only cluster these unit tests touch. It records applied
// manifests and answers the kubectl JSON documents each readiness stage reads.
type fakeCluster struct {
	mu sync.Mutex

	applied  []string
	pod      map[string]any
	job      map[string]any
	logs     string
	kubectls [][]string
	// calls records every Kubectl and GetJSON invocation together with the
	// context the fixture passed, so tests can scan all argument vectors for a
	// credential and observe the bounded/cancellation safety of a cleanup.
	calls      []fakeCall
	namespaces map[string]bool

	applyFailKind   string
	applyFailErr    error
	getNamespaceErr error
	deleteErr       error
	keepNamespace   bool
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{namespaces: map[string]bool{}}
}

// fakeCall is one recorded cluster invocation. kind distinguishes the two
// seams because GetJSON applies its own namespace defaulting.
type fakeCall struct {
	kind        string
	args        []string
	ctxErr      error
	hasDeadline bool
	// remaining is the observed time until the deadline when the call was
	// recorded; zero without a deadline.
	remaining time.Duration
}

// record stores one call while the caller holds f.mu. The context error and
// deadline are captured at invocation time, not after the call returns.
func (f *fakeCluster) record(kind string, ctx context.Context, args []string) {
	call := fakeCall{kind: kind, args: append([]string(nil), args...), ctxErr: ctx.Err()}
	if deadline, ok := ctx.Deadline(); ok {
		call.hasDeadline = true
		call.remaining = time.Until(deadline)
	}
	f.calls = append(f.calls, call)
}

func (f *fakeCluster) allCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([]fakeCall, 0, len(f.calls))
	for _, call := range f.calls {
		copied := call
		copied.args = append([]string(nil), call.args...)
		calls = append(calls, copied)
	}
	return calls
}

func (f *fakeCluster) Apply(_ context.Context, manifest string) (harness.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, manifest)
	var header struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &header); err != nil {
		return harness.Result{}, fmt.Errorf("fake apply: decode manifest: %w", err)
	}
	if f.applyFailKind != "" && header.Kind == f.applyFailKind {
		if f.applyFailErr == nil {
			return harness.Result{}, fmt.Errorf("fake apply: %s refused", header.Kind)
		}
		return harness.Result{}, f.applyFailErr
	}
	if header.Kind == "Namespace" {
		f.namespaces[header.Metadata.Name] = true
	}
	return harness.Result{Stdout: []byte(header.Kind + "/" + header.Metadata.Name + " applied")}, nil
}

func (f *fakeCluster) Kubectl(ctx context.Context, args ...string) (harness.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kubectls = append(f.kubectls, append([]string(nil), args...))
	f.record("kubectl", ctx, args)
	if len(args) == 0 {
		return harness.Result{}, errors.New("fake kubectl: empty args")
	}
	switch args[0] {
	case "logs":
		return harness.Result{Stdout: []byte(f.logs)}, nil
	case "get":
		if len(args) < 3 || args[1] != "namespace" {
			return harness.Result{}, fmt.Errorf("fake kubectl: unsupported get %v", args)
		}
		if f.getNamespaceErr != nil {
			return harness.Result{}, f.getNamespaceErr
		}
		if f.namespaces[args[2]] {
			return harness.Result{Stdout: []byte("namespace/" + args[2])}, nil
		}
		return harness.Result{}, nil
	case "delete":
		if len(args) < 3 || args[1] != "namespace" {
			return harness.Result{}, fmt.Errorf("fake kubectl: unsupported delete %v", args)
		}
		if f.deleteErr != nil {
			return harness.Result{}, f.deleteErr
		}
		if !f.keepNamespace {
			delete(f.namespaces, args[2])
		}
		return harness.Result{Stdout: []byte("namespace \"" + args[2] + "\" deleted")}, nil
	default:
		return harness.Result{}, fmt.Errorf("fake kubectl: unsupported command %v", args)
	}
}

func (f *fakeCluster) GetJSON(ctx context.Context, args ...string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("getjson", ctx, args)
	if len(args) < 3 || args[0] != "get" {
		return nil, fmt.Errorf("fake cluster: unsupported get %v", args)
	}
	switch args[1] {
	case "pod":
		if f.pod == nil {
			return nil, errors.New("fake cluster: pod not found")
		}
		return cloneDocument(f.pod), nil
	case "job":
		if f.job == nil {
			return nil, errors.New("fake cluster: job not found")
		}
		return cloneDocument(f.job), nil
	default:
		return nil, fmt.Errorf("fake cluster: unsupported get %v", args)
	}
}

func (f *fakeCluster) setPod(document map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pod = cloneDocument(document)
}

func (f *fakeCluster) setJob(document map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.job = cloneDocument(document)
}

func (f *fakeCluster) setLogs(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = text
}

func (f *fakeCluster) appliedManifests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.applied...)
}

func (f *fakeCluster) kubectlCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([][]string, 0, len(f.kubectls))
	for _, call := range f.kubectls {
		calls = append(calls, append([]string(nil), call...))
	}
	return calls
}

func cloneDocument(document map[string]any) map[string]any {
	if document == nil {
		return nil
	}
	encoded, err := yaml.Marshal(document)
	if err != nil {
		panic(err)
	}
	var clone map[string]any
	if err := yaml.Unmarshal(encoded, &clone); err != nil {
		panic(err)
	}
	return clone
}

// fakeClock advances only when a readiness barrier sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
	return nil
}

func fastWaitOptions() harness.WaitOptions {
	return harness.WaitOptions{
		Timeout:        40 * time.Millisecond,
		AttemptTimeout: 20 * time.Millisecond,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}
}

func decodeManifest(t *testing.T, manifest string) map[string]any {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &document); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, manifest)
	}
	return document
}

func valueAt(t *testing.T, document map[string]any, path ...string) any {
	t.Helper()
	var current any = document
	for _, key := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not a mapping (%T)", path, key, current)
		}
		next, ok := mapping[key]
		if !ok {
			t.Fatalf("path %v: missing key %q in %v", path, key, mapping)
		}
		current = next
	}
	return current
}

func stringAt(t *testing.T, document map[string]any, path ...string) string {
	t.Helper()
	value, ok := valueAt(t, document, path...).(string)
	if !ok {
		t.Fatalf("path %v is not a string: %T", path, valueAt(t, document, path...))
	}
	return value
}

func mappingAt(t *testing.T, document map[string]any, path ...string) map[string]any {
	t.Helper()
	value, ok := valueAt(t, document, path...).(map[string]any)
	if !ok {
		t.Fatalf("path %v is not a mapping: %T", path, valueAt(t, document, path...))
	}
	return value
}

func listAt(t *testing.T, document map[string]any, path ...string) []any {
	t.Helper()
	value, ok := valueAt(t, document, path...).([]any)
	if !ok {
		t.Fatalf("path %v is not a list: %T", path, valueAt(t, document, path...))
	}
	return value
}

func envEntry(t *testing.T, container map[string]any, name string) map[string]any {
	t.Helper()
	entries := listAt(t, container, "env")
	for _, entry := range entries {
		mapping, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("env entry is not a mapping: %T", entry)
		}
		if mapping["name"] == name {
			return mapping
		}
	}
	t.Fatalf("container has no %s environment variable: %v", name, entries)
	return nil
}

// TestPostgresDependencyManifestIsScopedAndPinned proves the fixture renders a
// namespace-scoped, digest-pinned PostgreSQL Pod and Service whose selector is
// unique to that fixture and whose credential exists only in the generated
// Secret payload, never in Pod arguments.
func TestPostgresDependencyManifestIsScopedAndPinned(t *testing.T) {
	const namespace = "kodama-dep-manifest"
	const name = "pg-manifest"

	fixture, err := newPostgres(newFakeCluster(), PostgresOptions{Namespace: namespace, Name: name})
	if err != nil {
		t.Fatalf("newPostgres: %v", err)
	}
	manifests, err := renderManifests(fixture)
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	probe, err := renderProbeJob(fixture, "pg-manifest-probe-unit")
	if err != nil {
		t.Fatalf("renderProbeJob: %v", err)
	}

	password := fixture.Connection().Password
	if password == "" {
		t.Fatal("fixture generated an empty password")
	}
	if fixture.Namespace() != namespace {
		t.Fatalf("namespace = %q; want %q", fixture.Namespace(), namespace)
	}
	if fixture.Namespace() == "default" {
		t.Fatal("fixture selected the default namespace")
	}

	namespaceDoc := decodeManifest(t, manifests.Namespace)
	if stringAt(t, namespaceDoc, "metadata", "name") != namespace {
		t.Fatalf("Namespace name = %q; want %q", stringAt(t, namespaceDoc, "metadata", "name"), namespace)
	}

	documents := map[string]map[string]any{
		"secret":  decodeManifest(t, manifests.Secret),
		"pod":     decodeManifest(t, manifests.Pod),
		"service": decodeManifest(t, manifests.Service),
		"probe":   decodeManifest(t, probe),
	}
	for label, document := range documents {
		got := stringAt(t, document, "metadata", "namespace")
		if got != namespace {
			t.Fatalf("%s namespace = %q; want %q", label, got, namespace)
		}
		if got == "default" {
			t.Fatalf("%s selected the default namespace", label)
		}
	}

	secretDoc := documents["secret"]
	secretData := mappingAt(t, secretDoc, "stringData")
	if secretData["password"] != password {
		t.Fatalf("Secret password does not carry the generated credential: %v", secretData)
	}
	for label, raw := range map[string]string{
		"namespace": manifests.Namespace,
		"pod":       manifests.Pod,
		"service":   manifests.Service,
		"probe":     probe,
	} {
		if strings.Contains(raw, password) {
			t.Fatalf("%s manifest contains the credential literal; it belongs only in the Secret", label)
		}
	}

	podDoc := documents["pod"]
	containers := listAt(t, podDoc, "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("pod containers = %d; want 1", len(containers))
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("container is not a mapping: %T", containers[0])
	}
	image := stringAt(t, container, "image")
	if image != PostgresImageReference {
		t.Fatalf("image = %q; want the immutable pinned reference %q", image, PostgresImageReference)
	}
	if !strings.Contains(image, "@sha256:") {
		t.Fatalf("image %q is not digest-pinned", image)
	}
	if strings.HasSuffix(image, ":latest") {
		t.Fatalf("image %q uses the mutable latest tag", image)
	}

	resources := mappingAt(t, container, "resources")
	for _, bound := range []string{"requests", "limits"} {
		values := mappingAt(t, resources, bound)
		for _, resource := range []string{"cpu", "memory"} {
			if values[resource] == nil || values[resource] == "" {
				t.Fatalf("pod %s %s is not bounded: %v", bound, resource, values)
			}
		}
	}

	readiness := mappingAt(t, container, "readinessProbe")
	if socket := mappingAt(t, readiness, "tcpSocket"); socket["port"] != "postgres" {
		t.Fatalf("readinessProbe tcpSocket port = %v; want the named container port postgres", socket["port"])
	}
	if readiness["timeoutSeconds"] != 1 || readiness["failureThreshold"] != 30 {
		t.Fatalf("readinessProbe bounds = timeoutSeconds %v, failureThreshold %v; want 1 and 30",
			readiness["timeoutSeconds"], readiness["failureThreshold"])
	}

	if strings.Contains(manifests.Pod, "hostPort") {
		t.Fatal("pod declares a host port")
	}
	if strings.Contains(manifests.Pod, "hostNetwork: true") {
		t.Fatal("pod declares host networking")
	}

	passwordEnv := envEntry(t, container, "POSTGRES_PASSWORD")
	if _, literal := passwordEnv["value"]; literal {
		t.Fatalf("pod passes the password as a literal environment value: %v", passwordEnv)
	}
	reference := mappingAt(t, passwordEnv, "valueFrom", "secretKeyRef")
	if reference["name"] != fixture.SecretName() {
		t.Fatalf("password secretKeyRef name = %v; want %q", reference["name"], fixture.SecretName())
	}
	if reference["key"] != "password" {
		t.Fatalf("password secretKeyRef key = %v; want password", reference["key"])
	}

	serviceDoc := documents["service"]
	if got := stringAt(t, serviceDoc, "spec", "type"); got != "ClusterIP" {
		t.Fatalf("service type = %q; want ClusterIP", got)
	}
	if strings.Contains(manifests.Service, "nodePort") {
		t.Fatal("service declares a node port")
	}
	selector := mappingAt(t, serviceDoc, "spec", "selector")
	if selector[LabelDependency] != DependencyPostgres {
		t.Fatalf("service selector dependency = %v; want %q", selector[LabelDependency], DependencyPostgres)
	}
	if selector[LabelInstance] != name {
		t.Fatalf("service selector instance = %v; want %q (a broad selector would select siblings)", selector[LabelInstance], name)
	}
	if len(selector) != 2 {
		t.Fatalf("service selector has %d labels; want exactly the dependency and instance labels: %v", len(selector), selector)
	}
	podLabels := mappingAt(t, podDoc, "metadata", "labels")
	if podLabels[LabelInstance] != name {
		t.Fatalf("pod instance label = %v; want %q", podLabels[LabelInstance], name)
	}
	servicePorts := listAt(t, serviceDoc, "spec", "ports")
	if len(servicePorts) != 1 {
		t.Fatalf("service ports = %d; want 1", len(servicePorts))
	}
	servicePort, ok := servicePorts[0].(map[string]any)
	if !ok {
		t.Fatalf("service port is not a mapping: %T", servicePorts[0])
	}
	if servicePort["port"] != PostgresPort {
		t.Fatalf("service port = %v; want %d", servicePort["port"], PostgresPort)
	}
	// The Service must target the named container port, not a hard-coded
	// numeric port: a renamed or missing container port would then route to a
	// closed socket while the Service still looked valid.
	containerPorts := listAt(t, container, "ports")
	if len(containerPorts) != 1 {
		t.Fatalf("pod container ports = %d; want 1", len(containerPorts))
	}
	containerPort, ok := containerPorts[0].(map[string]any)
	if !ok {
		t.Fatalf("pod container port is not a mapping: %T", containerPorts[0])
	}
	if containerPort["name"] != "postgres" || containerPort["containerPort"] != PostgresPort {
		t.Fatalf("pod container port = %v; want the named port postgres:%d", containerPort, PostgresPort)
	}
	if servicePort["targetPort"] != containerPort["name"] {
		t.Fatalf("service targetPort = %v; want the named container port %v", servicePort["targetPort"], containerPort["name"])
	}

	probeDoc := documents["probe"]
	probeContainers := listAt(t, probeDoc, "spec", "template", "spec", "containers")
	if len(probeContainers) != 1 {
		t.Fatalf("probe containers = %d; want 1", len(probeContainers))
	}
	probeContainer, ok := probeContainers[0].(map[string]any)
	if !ok {
		t.Fatalf("probe container is not a mapping: %T", probeContainers[0])
	}
	if got := stringAt(t, probeContainer, "image"); got != PostgresImageReference {
		t.Fatalf("probe image = %q; want the immutable pinned reference %q", got, PostgresImageReference)
	}
	hostEnv := envEntry(t, probeContainer, "PGHOST")
	host, _ := hostEnv["value"].(string)
	if host != fixture.ServiceHost() {
		t.Fatalf("probe PGHOST = %q; want the Service name %q", host, fixture.ServiceHost())
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		t.Fatalf("probe PGHOST = %q; the query must travel through the Service", host)
	}
	if _, literal := envEntry(t, probeContainer, "PGPASSWORD")["value"]; literal {
		t.Fatal("probe passes PGPASSWORD as a literal environment value")
	}
	probeReference := mappingAt(t, envEntry(t, probeContainer, "PGPASSWORD"), "valueFrom", "secretKeyRef")
	if probeReference["name"] != fixture.SecretName() {
		t.Fatalf("probe password secretKeyRef name = %v; want %q", probeReference["name"], fixture.SecretName())
	}
	if probeReference["key"] != "password" {
		t.Fatalf("probe password secretKeyRef key = %v; want password", probeReference["key"])
	}
	command := strings.Join(append(stringList(t, probeContainer, "command"), stringList(t, probeContainer, "args")...), " ")
	if strings.Contains(command, password) {
		t.Fatal("probe command arguments contain the credential literal")
	}
	if !strings.Contains(command, "psql") {
		t.Fatalf("probe does not run a SQL client: %q", command)
	}

	// The one-shot Job is bounded: a broken probe must not retry forever or
	// leave a finished Pod behind indefinitely.
	probeSpec := mappingAt(t, probeDoc, "spec")
	if got := intValue(probeSpec["backoffLimit"]); got != 0 {
		t.Fatalf("probe Job backoffLimit = %v; want 0 so a failed probe is reported instead of retried as a new Pod", probeSpec["backoffLimit"])
	}
	if got := intValue(probeSpec["activeDeadlineSeconds"]); got != 120 {
		t.Fatalf("probe Job activeDeadlineSeconds = %v; want the bounded 120", probeSpec["activeDeadlineSeconds"])
	}
	if got := intValue(probeSpec["ttlSecondsAfterFinished"]); got != 180 {
		t.Fatalf("probe Job ttlSecondsAfterFinished = %v; want the bounded 180", probeSpec["ttlSecondsAfterFinished"])
	}

	// Every container of the server Pod and of the probe Job must carry the
	// full hardening policy, not only the object the fixture currently mounts.
	assertPodHardened(t, "postgres Pod", mappingAt(t, podDoc, "spec"))
	assertPodHardened(t, "SQL probe Job", mappingAt(t, probeDoc, "spec", "template", "spec"))
}

// assertPodHardened pins the security and resource policy on a Pod spec and
// every container it declares: non-root, no privilege escalation, all
// capabilities dropped, an explicit image pull policy, both resource bounds, no
// service-account token, and no host namespace, host port or host path.
func assertPodHardened(t *testing.T, label string, podSpec map[string]any) {
	t.Helper()
	if automount, ok := podSpec["automountServiceAccountToken"].(bool); !ok || automount {
		t.Fatalf("%s automounts a service account token: %v", label, podSpec["automountServiceAccountToken"])
	}
	if hostNetwork, ok := podSpec["hostNetwork"].(bool); ok && hostNetwork {
		t.Fatalf("%s uses host networking", label)
	}
	podSecurity := mappingAt(t, podSpec, "securityContext")
	if runAsNonRoot, ok := podSecurity["runAsNonRoot"].(bool); !ok || !runAsNonRoot {
		t.Fatalf("%s pod securityContext runAsNonRoot = %v; want true", label, podSecurity["runAsNonRoot"])
	}
	runAsUser, ok := podSecurity["runAsUser"].(int)
	if !ok || runAsUser == 0 {
		t.Fatalf("%s pod securityContext runAsUser = %v; want a non-zero uid", label, podSecurity["runAsUser"])
	}
	if profile := mappingAt(t, podSecurity, "seccompProfile"); profile["type"] != "RuntimeDefault" {
		t.Fatalf("%s pod securityContext seccompProfile type = %v; want RuntimeDefault", label, profile["type"])
	}
	if volumes, ok := podSpec["volumes"].([]any); ok {
		for _, volume := range volumes {
			mapping, _ := volume.(map[string]any)
			if _, hostPath := mapping["hostPath"]; hostPath {
				t.Fatalf("%s mounts a host path: %v", label, mapping)
			}
		}
	}
	containers := listAt(t, podSpec, "containers")
	if len(containers) == 0 {
		t.Fatalf("%s declares no containers", label)
	}
	for index, entry := range containers {
		container, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("%s container %d is not a mapping: %T", label, index, entry)
		}
		assertContainerHardened(t, fmt.Sprintf("%s container %d", label, index), container)
	}
}

func assertContainerHardened(t *testing.T, label string, container map[string]any) {
	t.Helper()
	if policy, ok := container["imagePullPolicy"].(string); !ok || policy == "" {
		t.Fatalf("%s has no explicit imagePullPolicy: %v", label, container["imagePullPolicy"])
	}
	resources := mappingAt(t, container, "resources")
	for _, bound := range []string{"requests", "limits"} {
		values := mappingAt(t, resources, bound)
		for _, resource := range []string{"cpu", "memory"} {
			value, ok := values[resource].(string)
			if !ok || value == "" {
				t.Fatalf("%s %s %s is not bounded: %v", label, bound, resource, values)
			}
		}
	}
	security := mappingAt(t, container, "securityContext")
	if allowed, ok := security["allowPrivilegeEscalation"].(bool); !ok || allowed {
		t.Fatalf("%s allowPrivilegeEscalation = %v; want false", label, security["allowPrivilegeEscalation"])
	}
	if privileged, ok := security["privileged"].(bool); ok && privileged {
		t.Fatalf("%s runs privileged", label)
	}
	drop := listAt(t, mappingAt(t, security, "capabilities"), "drop")
	allDropped := false
	for _, entry := range drop {
		if entry == "ALL" {
			allDropped = true
		}
	}
	if !allDropped {
		t.Fatalf("%s capabilities.drop = %v; want ALL", label, drop)
	}
}

func stringList(t *testing.T, container map[string]any, key string) []string {
	t.Helper()
	raw, ok := container[key]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("container %s is not a list: %T", key, raw)
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			t.Fatalf("container %s item is not a string: %T", key, item)
		}
		values = append(values, value)
	}
	return values
}

func readyPod() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "pg"},
		"status": map[string]any{
			"phase": "Running",
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
			},
		},
	}
}

func runningPodWithoutReady() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "pg"},
		"status":   map[string]any{"phase": "Running"},
	}
}

// TestPostgresDependencyReadyRequiresSQL proves neither a Running Pod nor an
// open TCP port (the Pod Ready condition, which the fixture's TCP readiness
// probe sets) is treated as dependency readiness: the barrier returns only
// after a real SQL query through the Service produces its expected result.
func TestPostgresDependencyReadyRequiresSQL(t *testing.T) {
	cases := []struct {
		name    string
		pod     map[string]any
		job     map[string]any
		logs    string
		wantErr bool
	}{
		{
			name:    "running pod alone is not ready",
			pod:     runningPodWithoutReady(),
			job:     map[string]any{"status": map[string]any{"succeeded": 1}},
			logs:    "1\n",
			wantErr: true,
		},
		{
			name:    "open tcp port alone is not ready",
			pod:     readyPod(),
			job:     map[string]any{"status": map[string]any{"failed": 1}},
			logs:    "psql: error: connection refused\n",
			wantErr: true,
		},
		{
			name:    "sql result through the service is required",
			pod:     readyPod(),
			job:     map[string]any{"status": map[string]any{"succeeded": 1}},
			logs:    "0\n",
			wantErr: true,
		},
		{
			name:    "successful sql through the service is ready",
			pod:     readyPod(),
			job:     map[string]any{"status": map[string]any{"succeeded": 1}},
			logs:    "1\n",
			wantErr: false,
		},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := newFakeCluster()
			fixture, err := StartPostgres(context.Background(), cluster, PostgresOptions{
				Namespace:    fmt.Sprintf("kodama-dep-ready-%d", index),
				Name:         "pg-ready",
				Clock:        newFakeClock(),
				WaitOptions:  fastWaitOptions(),
				ReadyTimeout: 40 * time.Millisecond,
				ProbeTimeout: 40 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("StartPostgres: %v", err)
			}
			cluster.setPod(tc.pod)
			cluster.setJob(tc.job)
			cluster.setLogs(tc.logs)

			err = fixture.WaitReady(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("WaitReady succeeded; the fixture was not proven SQL-ready")
				}
				return
			}
			if err != nil {
				t.Fatalf("WaitReady: %v", err)
			}
			applied := strings.Join(cluster.appliedManifests(), "\n")
			if !strings.Contains(applied, fixture.ServiceHost()) {
				t.Fatalf("readiness did not probe the Service %q:\n%s", fixture.ServiceHost(), applied)
			}
		})
	}
}

// TestPostgresDependencyCloseScopesDeletion proves Close deletes exactly this
// fixture's namespace, never a sibling or the cluster, and reports a failed or
// unverified deletion instead of reducing it to a log line.
func TestPostgresDependencyCloseScopesDeletion(t *testing.T) {
	cluster := newFakeCluster()
	fixture, err := StartPostgres(context.Background(), cluster, PostgresOptions{
		Namespace: "kodama-dep-close-a",
		Name:      "pg-close",
	})
	if err != nil {
		t.Fatalf("StartPostgres: %v", err)
	}
	cluster.mu.Lock()
	cluster.namespaces["kodama-dep-close-b"] = true
	cluster.mu.Unlock()

	if err := fixture.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cluster.mu.Lock()
	own, sibling := cluster.namespaces["kodama-dep-close-a"], cluster.namespaces["kodama-dep-close-b"]
	cluster.mu.Unlock()
	if own {
		t.Fatal("Close left its own namespace behind")
	}
	if !sibling {
		t.Fatal("Close deleted a sibling namespace")
	}
	var deletion []string
	for _, call := range cluster.kubectlCalls() {
		if len(call) >= 2 && call[0] == "delete" && call[1] == "namespace" {
			deletion = call
		}
	}
	if deletion == nil {
		t.Fatal("Close issued no namespace deletion")
	}
	if len(deletion) < 3 {
		t.Fatalf("Close deletion args = %v; want a namespace argument", deletion)
	}
	if deletion[2] != "kodama-dep-close-a" {
		t.Fatalf("Close deleted namespace %q; want its own %q", deletion[2], "kodama-dep-close-a")
	}
	waits := false
	for _, argument := range deletion {
		if argument == "--wait=true" {
			waits = true
		}
		if argument == "--all" || argument == "kodama-dep-close-b" {
			t.Fatalf("Close deletion args = %v; want only its own namespace", deletion)
		}
	}
	if !waits {
		t.Fatalf("Close deletion args = %v; want a bounded waiting delete", deletion)
	}
	if err := fixture.Close(context.Background()); err != nil {
		t.Fatalf("second Close is not idempotent: %v", err)
	}

	failing := newFakeCluster()
	failing.deleteErr = errors.New("delete refused")
	failingFixture, err := StartPostgres(context.Background(), failing, PostgresOptions{
		Namespace: "kodama-dep-close-fail",
		Name:      "pg-close-fail",
	})
	if err != nil {
		t.Fatalf("StartPostgres: %v", err)
	}
	if err := failingFixture.Close(context.Background()); err == nil {
		t.Fatal("Close returned nil after the namespace delete failed")
	}

	stuck := newFakeCluster()
	stuck.keepNamespace = true
	stuckFixture, err := StartPostgres(context.Background(), stuck, PostgresOptions{
		Namespace: "kodama-dep-close-stuck",
		Name:      "pg-close-stuck",
	})
	if err != nil {
		t.Fatalf("StartPostgres: %v", err)
	}
	if err := stuckFixture.Close(context.Background()); err == nil {
		t.Fatal("Close returned nil while its namespace still existed")
	}
}

func namespaceDeletions(calls [][]string) [][]string {
	var deletions [][]string
	for _, call := range calls {
		if len(call) >= 3 && call[0] == "delete" && call[1] == "namespace" {
			deletions = append(deletions, call)
		}
	}
	return deletions
}

func startTestFixture(t *testing.T, cluster *fakeCluster, options PostgresOptions) *Postgres {
	t.Helper()
	fixture, err := StartPostgres(context.Background(), cluster, options)
	if err != nil {
		t.Fatalf("StartPostgres: %v", err)
	}
	return fixture
}

// TestPostgresDependencyStartCleansUpPartialFixture proves a failed apply after
// the namespace was created deletes exactly that namespace once, so a partial
// fixture cannot leak; when the cleanup delete fails too, both causes reach the
// caller. The error never carries the credential.
func TestPostgresDependencyStartCleansUpPartialFixture(t *testing.T) {
	const password = "partial-fixture-password-9f"
	causes := map[string]string{
		"Secret":  "create dependency Secret",
		"Pod":     "create dependency Pod",
		"Service": "create dependency Service",
	}
	for _, kind := range []string{"Secret", "Pod", "Service"} {
		t.Run(kind+" apply fails", func(t *testing.T) {
			cluster := newFakeCluster()
			cluster.applyFailKind = kind
			cluster.applyFailErr = errors.New("apply refused by fake")
			namespace := "kodama-dep-partial-" + strings.ToLower(kind)
			fixture, err := StartPostgres(context.Background(), cluster, PostgresOptions{
				Namespace: namespace,
				Name:      "pg-partial",
				Password:  password,
			})
			if err == nil {
				t.Fatalf("StartPostgres succeeded after the %s apply failed", kind)
			}
			if fixture != nil {
				t.Fatalf("StartPostgres returned a fixture after the %s apply failed: %+v", kind, fixture)
			}
			if !strings.Contains(err.Error(), "apply refused by fake") {
				t.Fatalf("StartPostgres error = %v; want the apply failure", err)
			}
			if !strings.Contains(err.Error(), causes[kind]) {
				t.Fatalf("StartPostgres error = %v; want the %q cause", err, causes[kind])
			}
			deletions := namespaceDeletions(cluster.kubectlCalls())
			if len(deletions) != 1 {
				t.Fatalf("cleanup issued %d namespace deletions; want exactly one: %v", len(deletions), deletions)
			}
			deletion := deletions[0]
			if deletion[2] != namespace {
				t.Fatalf("cleanup deleted namespace %q; want its own %q (never default or a sibling)", deletion[2], namespace)
			}
			waits := false
			for _, argument := range deletion {
				if argument == "--wait=true" {
					waits = true
				}
			}
			if !waits {
				t.Fatalf("cleanup deletion args = %v; want a bounded waiting delete", deletion)
			}
			if cluster.namespaces[namespace] {
				t.Fatalf("partial fixture leaked namespace %q", namespace)
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("StartPostgres error leaked the credential: %v", err)
			}
		})
	}

	t.Run("cleanup delete also fails", func(t *testing.T) {
		cluster := newFakeCluster()
		cluster.applyFailKind = "Pod"
		cluster.applyFailErr = errors.New("pod apply refused")
		cluster.deleteErr = errors.New("delete refused by fake")
		namespace := "kodama-dep-partial-both"
		_, err := StartPostgres(context.Background(), cluster, PostgresOptions{
			Namespace: namespace,
			Name:      "pg-partial-both",
			Password:  password,
		})
		if err == nil {
			t.Fatal("StartPostgres succeeded after both the apply and the cleanup delete failed")
		}
		for _, want := range []string{"create dependency Pod", "clean up dependency namespace", "delete refused by fake"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("StartPostgres error = %v; want %q so both causes are reported", err, want)
			}
		}
		if !cluster.namespaces[namespace] {
			t.Fatal("cleanup delete failed but the fake no longer has the namespace; the failure was not really exercised")
		}
		if n := len(namespaceDeletions(cluster.kubectlCalls())); n != 1 {
			t.Fatalf("cleanup issued %d namespace deletions; want exactly one", n)
		}
		if strings.Contains(err.Error(), password) {
			t.Fatalf("StartPostgres error leaked the credential: %v", err)
		}
	})
}

// TestPostgresDependencyCloseFailureIsRetryable proves Close marks itself
// closed only after a verified deletion: an unverifiable deletion returns an
// error, and a retry after the fault clears issues a second delete and then
// succeeds. A failed delete is likewise retried, never reported as done.
func TestPostgresDependencyCloseFailureIsRetryable(t *testing.T) {
	t.Run("unverifiable deletion", func(t *testing.T) {
		cluster := newFakeCluster()
		fixture := startTestFixture(t, cluster, PostgresOptions{Namespace: "kodama-dep-verify-fail", Name: "pg-verify"})
		cluster.getNamespaceErr = errors.New("verification get refused by fake")
		if err := fixture.Close(context.Background()); err == nil {
			t.Fatal("Close returned nil while the deletion could not be verified")
		}
		cluster.getNamespaceErr = nil
		if exists, err := fixture.NamespaceExists(context.Background()); err != nil {
			t.Fatalf("NamespaceExists after the failed verification: %v", err)
		} else if exists {
			t.Fatal("the fake namespace survived the delete")
		}
		before := len(namespaceDeletions(cluster.kubectlCalls()))
		if err := fixture.Close(context.Background()); err != nil {
			t.Fatalf("Close retry after the verification fault cleared: %v", err)
		}
		after := len(namespaceDeletions(cluster.kubectlCalls()))
		if after != before+1 {
			t.Fatalf("Close retry issued %d namespace deletions; want exactly one more (a closed flag set before verification leaks the namespace)", after-before)
		}
		if cluster.namespaces[fixture.Namespace()] {
			t.Fatalf("Close retry left namespace %q behind", fixture.Namespace())
		}
		if err := fixture.Close(context.Background()); err != nil {
			t.Fatalf("Close after a verified deletion is not idempotent: %v", err)
		}
		if n := len(namespaceDeletions(cluster.kubectlCalls())); n != after {
			t.Fatalf("Close after a verified deletion issued another namespace deletion (%d -> %d)", after, n)
		}
	})

	t.Run("failed deletion", func(t *testing.T) {
		cluster := newFakeCluster()
		fixture := startTestFixture(t, cluster, PostgresOptions{Namespace: "kodama-dep-delete-fail", Name: "pg-delete"})
		cluster.deleteErr = errors.New("delete refused by fake")
		if err := fixture.Close(context.Background()); err == nil {
			t.Fatal("Close returned nil after the namespace delete failed")
		} else if !strings.Contains(err.Error(), "delete dependency namespace") {
			t.Fatalf("Close error = %v; want the delete failure reported, not only the verification failure", err)
		}
		cluster.deleteErr = nil
		before := len(namespaceDeletions(cluster.kubectlCalls()))
		if err := fixture.Close(context.Background()); err != nil {
			t.Fatalf("Close retry after the delete fault cleared: %v", err)
		}
		after := len(namespaceDeletions(cluster.kubectlCalls()))
		if after != before+1 {
			t.Fatalf("Close retry issued %d namespace deletions; want exactly one more", after-before)
		}
		if cluster.namespaces[fixture.Namespace()] {
			t.Fatalf("Close retry left namespace %q behind", fixture.Namespace())
		}
	})
}

// TestPostgresDependencyRefusesForeignNamespaces proves validation refuses the
// reserved, malformed and over-long namespace and object names before any
// cluster write, so a caller cannot point a fixture at the cluster's own
// namespace or at an invalid name.
func TestPostgresDependencyRefusesForeignNamespaces(t *testing.T) {
	namespaces := []struct {
		name      string
		namespace string
	}{
		{"default", "default"},
		{"kube-system", "kube-system"},
		{"kube-public", "kube-public"},
		{"kube-node-lease", "kube-node-lease"},
		{"uppercase", "Kodama-Dep-Upper"},
		{"over length", strings.Repeat("a", 64)},
		{"underscore", "kodama_dep"},
		{"dot", "kodama.dep"},
		{"leading hyphen", "-kodama-dep"},
		{"trailing hyphen", "kodama-dep-"},
		{"space", "kodama dep"},
	}
	for _, tc := range namespaces {
		t.Run("namespace "+tc.name, func(t *testing.T) {
			cluster := newFakeCluster()
			fixture, err := StartPostgres(context.Background(), cluster, PostgresOptions{
				Namespace: tc.namespace,
				Name:      "pg-refuse",
			})
			if err == nil {
				t.Fatalf("StartPostgres accepted namespace %q", tc.namespace)
			}
			if fixture != nil {
				t.Fatalf("StartPostgres returned a fixture for refused namespace %q", tc.namespace)
			}
			if applied := cluster.appliedManifests(); len(applied) != 0 {
				t.Fatalf("refused namespace %q still applied %d objects", tc.namespace, len(applied))
			}
			if calls := cluster.kubectlCalls(); len(calls) != 0 {
				t.Fatalf("refused namespace %q still issued kubectl calls: %v", tc.namespace, calls)
			}
			if n := len(namespaceDeletions(cluster.kubectlCalls())); n != 0 {
				t.Fatalf("refused namespace %q issued %d deletions", tc.namespace, n)
			}
		})
	}

	names := []struct {
		name  string
		value string
	}{
		{"uppercase", "Pg-Object"},
		{"underscore", "pg_object"},
		{"dot", "pg.object"},
		{"over length", strings.Repeat("b", 64)},
		{"leading hyphen", "-pg"},
		{"trailing hyphen", "pg-"},
	}
	for _, tc := range names {
		t.Run("object name "+tc.name, func(t *testing.T) {
			cluster := newFakeCluster()
			_, err := StartPostgres(context.Background(), cluster, PostgresOptions{
				Namespace: "kodama-dep-name-check",
				Name:      tc.value,
			})
			if err == nil {
				t.Fatalf("StartPostgres accepted object name %q", tc.value)
			}
			if applied := cluster.appliedManifests(); len(applied) != 0 {
				t.Fatalf("refused object name %q still applied %d objects", tc.value, len(applied))
			}
		})
	}
}

// TestPostgresDependencyRefusesPreExistingNamespace proves a fixture never
// adopts or deletes a namespace it did not create: a namespace that already
// exists is refused before the apply and survives untouched.
func TestPostgresDependencyRefusesPreExistingNamespace(t *testing.T) {
	cluster := newFakeCluster()
	const namespace = "kodama-dep-existing"
	cluster.namespaces[namespace] = true
	fixture, err := StartPostgres(context.Background(), cluster, PostgresOptions{
		Namespace: namespace,
		Name:      "pg-existing",
	})
	if err == nil {
		t.Fatal("StartPostgres adopted a namespace that already existed")
	}
	if fixture != nil {
		t.Fatalf("StartPostgres returned a fixture for an existing namespace: %+v", fixture)
	}
	if !strings.Contains(err.Error(), "existing namespace") {
		t.Fatalf("StartPostgres error = %v; want an existing-namespace refusal", err)
	}
	if applied := cluster.appliedManifests(); len(applied) != 0 {
		t.Fatalf("existing namespace was still followed by %d applies", len(applied))
	}
	if n := len(namespaceDeletions(cluster.kubectlCalls())); n != 0 {
		t.Fatalf("StartPostgres deleted a pre-existing namespace (%d deletions)", n)
	}
	if !cluster.namespaces[namespace] {
		t.Fatalf("pre-existing namespace %q was deleted", namespace)
	}
}

// TestPostgresDependencyRefusesUnobservableNamespace proves a failed namespace
// observation is reported, never read as "absent". Otherwise the guard would
// let StartPostgres apply into a namespace whose ownership is unknown, and a
// later Close would delete a namespace this fixture never created.
func TestPostgresDependencyRefusesUnobservableNamespace(t *testing.T) {
	cluster := newFakeCluster()
	const namespace = "kodama-dep-unobservable"
	cluster.getNamespaceErr = errors.New("namespace get refused by fake")
	fixture, err := StartPostgres(context.Background(), cluster, PostgresOptions{
		Namespace: namespace,
		Name:      "pg-unobservable",
	})
	if err == nil {
		t.Fatal("StartPostgres treated a failed namespace observation as an absent namespace")
	}
	if fixture != nil {
		t.Fatalf("StartPostgres returned a fixture after the namespace observation failed: %+v", fixture)
	}
	for _, want := range []string{"observe dependency namespace", "namespace get refused by fake"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("StartPostgres error = %v; want %q so the observation failure is wrapped", err, want)
		}
	}
	if applied := cluster.appliedManifests(); len(applied) != 0 {
		t.Fatalf("StartPostgres applied %d objects after the namespace observation failed:\n%s",
			len(applied), strings.Join(applied, "\n---\n"))
	}
	if n := len(namespaceDeletions(cluster.kubectlCalls())); n != 0 {
		t.Fatalf("StartPostgres deleted a namespace it never confirmed it created (%d deletions)", n)
	}
	cluster.mu.Lock()
	created := cluster.namespaces[namespace]
	cluster.mu.Unlock()
	if created {
		t.Fatalf("the fake namespace %q was created after the observation failed", namespace)
	}
}

// TestPostgresDependencyGeneratesUniqueCredentials proves each fixture gets a
// fresh 48-hex password and a fresh kodama-dep-<8 hex> namespace, and that the
// generated credential never appears in an error or a kubectl argument.
func TestPostgresDependencyGeneratesUniqueCredentials(t *testing.T) {
	cluster := newFakeCluster()
	first := startTestFixture(t, cluster, PostgresOptions{Name: "pg-cred-a"})
	second := startTestFixture(t, cluster, PostgresOptions{Name: "pg-cred-b"})

	passwordPattern := regexp.MustCompile(`^[0-9a-f]{48}$`)
	namespacePattern := regexp.MustCompile(`^kodama-dep-[0-9a-f]{8}$`)
	for _, fixture := range []*Postgres{first, second} {
		password := fixture.Connection().Password
		if !passwordPattern.MatchString(password) {
			t.Fatalf("generated password %q does not match %s", password, passwordPattern)
		}
		if !namespacePattern.MatchString(fixture.Namespace()) {
			t.Fatalf("generated namespace %q does not match %s", fixture.Namespace(), namespacePattern)
		}
	}
	if first.Connection().Password == second.Connection().Password {
		t.Fatalf("two fixtures share the generated password %q", first.Connection().Password)
	}
	if first.Namespace() == second.Namespace() {
		t.Fatalf("two fixtures share the generated namespace %q", first.Namespace())
	}

	passwords := []string{first.Connection().Password, second.Connection().Password}
	for _, call := range cluster.kubectlCalls() {
		joined := strings.Join(call, " ")
		for _, password := range passwords {
			if strings.Contains(joined, password) {
				t.Fatalf("kubectl argument %v contains a generated credential", call)
			}
		}
	}
	for index, manifest := range cluster.appliedManifests() {
		document := decodeManifest(t, manifest)
		kind, _ := document["kind"].(string)
		for _, password := range passwords {
			if strings.Contains(manifest, password) && kind != "Secret" {
				t.Fatalf("applied %s object %d contains a generated credential", kind, index)
			}
		}
	}
}

// TestPostgresDependencyRefusesMutableImageReferences proves only an immutable
// digest reference is accepted; a mutable tag, a tagged reference, a short
// digest and an uppercase digest are refused before any object is applied.
func TestPostgresDependencyRefusesMutableImageReferences(t *testing.T) {
	refused := []struct {
		name  string
		image string
	}{
		{"latest tag", "docker.io/library/postgres:latest"},
		{"alpine tag", "docker.io/library/postgres:18-alpine"},
		{"short digest", "docker.io/library/postgres@sha256:1234abcd"},
		{"uppercase digest", "docker.io/library/postgres@sha256:" + strings.Repeat("AB", 32)},
		{"no digest", "docker.io/library/postgres"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			cluster := newFakeCluster()
			_, err := StartPostgres(context.Background(), cluster, PostgresOptions{
				Namespace: "kodama-dep-image-check",
				Name:      "pg-image",
				Image:     tc.image,
			})
			if err == nil {
				t.Fatalf("StartPostgres accepted mutable image reference %q", tc.image)
			}
			if applied := cluster.appliedManifests(); len(applied) != 0 {
				t.Fatalf("refused image reference %q still applied %d objects", tc.image, len(applied))
			}
		})
	}

	t.Run("valid digest accepted", func(t *testing.T) {
		cluster := newFakeCluster()
		image := "registry.example/kodama/postgres@sha256:" + strings.Repeat("ab", 32)
		fixture := startTestFixture(t, cluster, PostgresOptions{
			Namespace: "kodama-dep-image-ok",
			Name:      "pg-image-ok",
			Image:     image,
		})
		if fixture == nil {
			t.Fatal("StartPostgres returned no fixture for a valid digest reference")
		}
		if applied := cluster.appliedManifests(); len(applied) != 4 {
			t.Fatalf("valid digest applied %d objects; want 4", len(applied))
		}
		for _, manifest := range cluster.appliedManifests() {
			if strings.Contains(manifest, "image: ") && !strings.Contains(manifest, image) {
				t.Fatalf("applied manifest does not use the requested digest reference:\n%s", manifest)
			}
		}
	})
}

// TestPostgresDependencyCredentialNeverReachesCommandArguments scans every
// Kubectl and GetJSON argument vector the fixture issues on the ready, probe
// logs, close-delete and failed-start cleanup paths. The credential belongs only
// in the Secret payload; adding it to any of those argument lists must fail.
func TestPostgresDependencyCredentialNeverReachesCommandArguments(t *testing.T) {
	const password = "credential-scan-6b1f4d2a"

	serving := newFakeCluster()
	serving.setPod(readyPod())
	serving.setJob(map[string]any{"status": map[string]any{"succeeded": 1}})
	serving.setLogs("1\n")
	fixture := startTestFixture(t, serving, PostgresOptions{
		Namespace:    "kodama-dep-cred-scan",
		Name:         "pg-cred-scan",
		Password:     password,
		Clock:        newFakeClock(),
		WaitOptions:  fastWaitOptions(),
		ReadyTimeout: 40 * time.Millisecond,
		ProbeTimeout: 40 * time.Millisecond,
	})
	if err := fixture.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := fixture.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	partial := newFakeCluster()
	partial.applyFailKind = "Pod"
	partial.applyFailErr = errors.New("pod apply refused")
	_, err := StartPostgres(context.Background(), partial, PostgresOptions{
		Namespace: "kodama-dep-cred-scan-partial",
		Name:      "pg-cred-scan-partial",
		Password:  password,
	})
	if err == nil {
		t.Fatal("StartPostgres succeeded after the Pod apply failed")
	}

	for name, cluster := range map[string]*fakeCluster{"serving": serving, "partial": partial} {
		var sawGetJSON, sawDelete, sawLogs bool
		for _, call := range cluster.allCalls() {
			joined := strings.Join(call.args, " ")
			if strings.Contains(joined, password) {
				t.Fatalf("%s cluster %s call %v contains the generated credential", name, call.kind, call.args)
			}
			switch call.kind {
			case "getjson":
				sawGetJSON = true
			case "kubectl":
				if len(call.args) > 0 && call.args[0] == "delete" {
					sawDelete = true
				}
				if len(call.args) > 0 && call.args[0] == "logs" {
					sawLogs = true
				}
			}
		}
		// The scan is only meaningful if the paths really ran. The serving
		// cluster exercises GetJSON (Pod and Job observation) and the probe
		// logs path; the partial-start cluster exercises the cleanup delete.
		if name == "serving" && !sawGetJSON {
			t.Fatalf("%s cluster scan saw no GetJSON call", name)
		}
		if !sawDelete {
			t.Fatalf("%s cluster scan saw no delete call", name)
		}
		if name == "serving" && !sawLogs {
			t.Fatal("serving cluster scan saw no probe logs call")
		}
	}
}

// TestPostgresDependencyCloseWrapsDeleteCause proves Close preserves the
// underlying kubectl error through %w, not only its text: cleanup and reporting
// depend on errors.Is so a caller (and the cleanup join) can classify a refused
// or missing deletion.
func TestPostgresDependencyCloseWrapsDeleteCause(t *testing.T) {
	cause := errors.New("delete refused sentinel 4f1c")
	cluster := newFakeCluster()
	cluster.deleteErr = cause
	fixture := startTestFixture(t, cluster, PostgresOptions{
		Namespace: "kodama-dep-wrap-cause",
		Name:      "pg-wrap-cause",
	})
	err := fixture.Close(context.Background())
	if err == nil {
		t.Fatal("Close returned nil after the namespace delete failed")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("Close error = %v; want errors.Is(_, %v) so the delete cause survives wrapping", err, cause)
	}
}

// TestPostgresDependencyProbeJobConditions pins the Job Complete/Failed
// condition branches, not only the succeeded/failed counters: a condition is
// authoritative even before a counter is visible, and a Failed condition is
// reported as a failure rather than as "not completed yet".
func TestPostgresDependencyProbeJobConditions(t *testing.T) {
	condition := func(kind, status string) map[string]any {
		return map[string]any{"status": map[string]any{"conditions": []any{
			map[string]any{"type": kind, "status": status},
		}}}
	}
	cases := []struct {
		name    string
		job     map[string]any
		wantErr bool
		want    string
	}{
		{name: "complete condition", job: condition("Complete", "True"), wantErr: false},
		{name: "failed condition", job: condition("Failed", "True"), wantErr: true, want: "failed"},
		{name: "incomplete condition alone", job: condition("Complete", "False"), wantErr: true, want: "not completed"},
		{name: "failed condition false falls back to the failed counter", job: map[string]any{"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Failed", "status": "False"}},
			"failed":     1,
		}}, wantErr: true, want: "failed"},
		{name: "complete condition false falls back to the succeeded counter", job: map[string]any{"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Complete", "status": "False"}},
			"succeeded":  1,
		}}, wantErr: false},
		{name: "no conditions and no counters", job: map[string]any{"status": map[string]any{}}, wantErr: true, want: "not completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := jobComplete(tc.job)
			if tc.wantErr {
				if err == nil {
					t.Fatal("jobComplete succeeded; the probe Job was not proven complete")
				}
				if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
					t.Fatalf("jobComplete error = %v; want it to mention %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("jobComplete: %v", err)
			}
		})
	}
}

// TestPostgresDependencyCloseDeleteIsBounded proves Close bounds its delete and
// verification with the configured DeleteTimeout instead of inheriting an
// unbounded context. The fake records the deadline it was handed.
func TestPostgresDependencyCloseDeleteIsBounded(t *testing.T) {
	const deleteTimeout = 3 * time.Second
	cluster := newFakeCluster()
	fixture := startTestFixture(t, cluster, PostgresOptions{
		Namespace:     "kodama-dep-close-bounded",
		Name:          "pg-close-bounded",
		DeleteTimeout: deleteTimeout,
	})
	if err := fixture.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// StartPostgres also observes the namespace, unbounded by DeleteTimeout.
	// Only Close's delete and the verification get that follows it are in
	// scope, so keep the last of each.
	var deleteCall, verifyCall *fakeCall
	calls := cluster.allCalls()
	for index := range calls {
		call := &calls[index]
		if call.kind != "kubectl" || len(call.args) < 3 || call.args[1] != "namespace" {
			continue
		}
		switch call.args[0] {
		case "delete":
			deleteCall = call
		case "get":
			verifyCall = call
		}
	}
	if deleteCall == nil || verifyCall == nil {
		t.Fatalf("Close issued delete=%t verify=%t; want both", deleteCall != nil, verifyCall != nil)
	}
	for label, call := range map[string]*fakeCall{"delete": deleteCall, "verify": verifyCall} {
		if call.ctxErr != nil {
			t.Fatalf("Close %s call context error = %v; want a live bounded context", label, call.ctxErr)
		}
		if !call.hasDeadline {
			t.Fatalf("Close %s call has no deadline; want the configured %s bound", label, deleteTimeout)
		}
		if call.remaining > deleteTimeout+time.Second {
			t.Fatalf("Close %s call deadline is %s away; want at most %s", label, call.remaining, deleteTimeout)
		}
	}
}

// TestPostgresDependencyStartCleanupSurvivesCancelledCaller proves the
// failed-start cleanup runs on a context detached from the caller's
// cancellation (bounded by DeleteTimeout). Otherwise a caller whose context was
// cancelled while an apply was already in flight could leave the namespace it
// created behind.
func TestPostgresDependencyStartCleanupSurvivesCancelledCaller(t *testing.T) {
	const deleteTimeout = 2 * time.Second
	cluster := newFakeCluster()
	cluster.applyFailKind = "Pod"
	cluster.applyFailErr = errors.New("pod apply refused")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := StartPostgres(ctx, cluster, PostgresOptions{
		Namespace:     "kodama-dep-cleanup-cancelled",
		Name:          "pg-cleanup-cancelled",
		DeleteTimeout: deleteTimeout,
	})
	if err == nil {
		t.Fatal("StartPostgres succeeded after the Pod apply failed")
	}
	deletions := namespaceDeletions(cluster.kubectlCalls())
	if len(deletions) != 1 {
		t.Fatalf("cleanup issued %d namespace deletions; want exactly one", len(deletions))
	}
	for _, call := range cluster.allCalls() {
		if call.kind != "kubectl" || len(call.args) < 3 || call.args[0] != "delete" {
			continue
		}
		if call.ctxErr != nil {
			t.Fatalf("cleanup delete context error = %v; want a context detached from the cancelled caller", call.ctxErr)
		}
		if !call.hasDeadline {
			t.Fatalf("cleanup delete has no deadline; want the configured %s bound", deleteTimeout)
		}
		if call.remaining > deleteTimeout+time.Second {
			t.Fatalf("cleanup delete deadline is %s away; want at most %s", call.remaining, deleteTimeout)
		}
	}
}

// TestPostgresDependencyStartAppliesOwnedObjectsInOrder proves the namespace is
// created before anything that needs it and every scoped object lands in the
// fixture's own namespace.
func TestPostgresDependencyStartAppliesOwnedObjectsInOrder(t *testing.T) {
	cluster := newFakeCluster()
	const namespace = "kodama-dep-order"
	startTestFixture(t, cluster, PostgresOptions{Namespace: namespace, Name: "pg-order"})

	var kinds []string
	for _, manifest := range cluster.appliedManifests() {
		document := decodeManifest(t, manifest)
		kind, _ := document["kind"].(string)
		kinds = append(kinds, kind)
		if kind == "Namespace" {
			continue
		}
		if got := stringAt(t, document, "metadata", "namespace"); got != namespace {
			t.Fatalf("%s namespace = %q; want the fixture namespace %q", kind, got, namespace)
		}
	}
	want := []string{"Namespace", "Secret", "Pod", "Service"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("apply order = %v; want %v (the namespace must exist before the Pod)", kinds, want)
	}
}
