//go:build qualification

package production

// Live stage support for the Q02 isolation probes. Everything here is inert
// until a TestStageRuntimeIsolation or TestStageCrossTenantIsolation probe runs
// with every named input configured. The unit tests drive the same parsers and
// command builders against fixtures and fake HTTP edges.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// isolationRecordSkippedEvidence writes one explicit skipped-outcome record for
// a sub-check the probe could not run. The main probe record may still pass, so
// an unconfigured Git fixture is never silently indistinguishable from a green
// one in the evidence directory.
func isolationRecordSkippedEvidence(t *testing.T, probe *resilienceProbe, name string, started time.Time) {
	t.Helper()
	if probe == nil || probe.writer == nil {
		return
	}
	if _, err := probe.writer.Write(name, map[string]any{
		"probe":       probe.name,
		"outcome":     "skipped",
		"started_at":  started,
		"finished_at": time.Now().UTC(),
	}); err != nil {
		t.Errorf("write the %q evidence record: %v", name, err)
	}
}

// isolationTenant is one tenant's public, Kubernetes and credential handles.
type isolationTenant struct {
	ID        string
	ProjectID string
	Namespace string
	Admin     Secret
	Stage     *resilienceStage
}

// isolationRuntime bundles the two tenants and the guarded kubectl scope.
type isolationRuntime struct {
	Config  Config
	API     *API
	Cluster resilienceCluster
	Kubectl *Kubectl
	A       isolationTenant
	B       isolationTenant
}

// isolationRuntimeInputs is the complete input set the runtime-isolation probe
// needs: the two public tenants, their two namespaces, the guarded context and
// the digest-pinned workload image.
func isolationRuntimeInputs() []Input {
	return []Input{
		InputAPIURL,
		InputCAPEM,
		InputTLSServerName,
		InputAdminToken,
		InputTenantID,
		InputProjectID,
		InputTenantNamespace,
		InputSecondTenantID,
		InputSecondProjectID,
		InputSecondNamespace,
		InputSecondAdminToken,
		InputKubeContext,
		InputImage,
		InputEvidenceDir,
	}
}

// newIsolationRuntime builds the public client, the two tenant views and the
// confinement guard that admits exactly the two assigned namespaces.
func newIsolationRuntime(t *testing.T, config Config) *isolationRuntime {
	t.Helper()
	client, err := NewAPI(config)
	if err != nil {
		t.Fatalf("build the stage API helper: %v", err)
	}
	if config.TenantNamespace != isolationNamespaceForTenant(config.TenantID) {
		t.Fatalf("Q02_TENANT_NAMESPACE = %q; the chart derives %q from Q02_TENANT_ID %q",
			config.TenantNamespace, isolationNamespaceForTenant(config.TenantID), config.TenantID)
	}
	if config.SecondNamespace != isolationNamespaceForTenant(config.SecondTenantID) {
		t.Fatalf("Q02_SECOND_TENANT_NAMESPACE = %q; the chart derives %q from Q02_SECOND_TENANT_ID %q",
			config.SecondNamespace, isolationNamespaceForTenant(config.SecondTenantID), config.SecondTenantID)
	}
	if config.TenantNamespace == config.SecondNamespace {
		t.Fatalf("both tenants were assigned namespace %q", config.TenantNamespace)
	}
	if config.TenantID == config.SecondTenantID {
		t.Fatalf("both tenants were assigned the same tenant id %q", config.TenantID)
	}
	if config.ProjectID == config.SecondProjectID {
		t.Fatalf("both tenants were assigned the same project id %q", config.ProjectID)
	}
	guard, err := NewGuard(config.KubeContext, config.TenantNamespace, config.SecondNamespace)
	if err != nil {
		t.Fatalf("build the two-tenant confinement guard: %v", err)
	}
	kubectl := &Kubectl{Guard: guard, Path: "kubectl", Timeout: 3 * time.Minute}
	cluster := resilienceNewCluster(kubectl)
	runtime := &isolationRuntime{
		Config:  config,
		API:     client,
		Cluster: cluster,
		Kubectl: kubectl,
		A:       isolationTenant{ID: config.TenantID, ProjectID: config.ProjectID, Namespace: config.TenantNamespace, Admin: config.AdminToken},
		B:       isolationTenant{ID: config.SecondTenantID, ProjectID: config.SecondProjectID, Namespace: config.SecondNamespace, Admin: config.SecondAdminToken},
	}
	runtime.A.Stage = isolationTenantStage(config, client, cluster, runtime.A)
	runtime.B.Stage = isolationTenantStage(config, client, cluster, runtime.B)
	return runtime
}

// isolationTenantStage returns the resilience helpers bound to one tenant.
func isolationTenantStage(config Config, client *API, cluster resilienceCluster, tenant isolationTenant) *resilienceStage {
	tenantConfig := config
	tenantConfig.TenantID = tenant.ID
	tenantConfig.ProjectID = tenant.ProjectID
	tenantConfig.TenantNamespace = tenant.Namespace
	tenantConfig.AdminToken = tenant.Admin
	return &resilienceStage{Config: tenantConfig, API: client, Cluster: cluster}
}

// isolationStart starts one inline execution and registers a bounded cancel on
// every failure path. The returned execution id carries no credential.
func isolationStart(t *testing.T, stage *resilienceStage, document []byte, keyPrefix string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key, err := probeIdempotencyKey(keyPrefix)
	if err != nil {
		t.Fatalf("generate the %s idempotency key: %v", keyPrefix, err)
	}
	started, err := resilienceStartInline(ctx, stage, document, key)
	if err != nil {
		t.Fatalf("start the %s execution: %v", keyPrefix, err)
	}
	isolationCancelOnCleanup(t, stage, started.ExecutionID)
	return started.ExecutionID
}

// isolationCancelOnCleanup cancels a nonterminal execution at its current
// version and waits for the terminal state. A cleanup failure is reported, not
// hidden.
func isolationCancelOnCleanup(t *testing.T, stage *resilienceStage, executionID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		inspection, err := resilienceInspect(ctx, stage, executionID)
		if err != nil {
			t.Errorf("inspect execution %q during cleanup: %v", executionID, err)
			return
		}
		if terminalExecutionState(inspection.State) {
			return
		}
		key, err := probeIdempotencyKey("q02-isolation-cancel")
		if err != nil {
			t.Errorf("generate the cleanup cancel key: %v", err)
			return
		}
		if _, err := stage.API.PostVersioned(ctx, stage.Config.AdminToken, "execution.cancel",
			api.ExecutionCancelRequest{ExecutionID: executionID}, key, inspection.Version); err != nil {
			t.Errorf("cancel execution %q during cleanup: %v", executionID, err)
			return
		}
		if _, _, err := resilienceWaitTerminal(ctx, stage, executionID, 60*time.Second); err != nil {
			t.Errorf("execution %q did not settle after the cleanup cancel: %v", executionID, err)
		}
	})
}

// isolationReadNamespace reads exactly one assigned namespace object.
func isolationReadNamespace(ctx context.Context, runtime *isolationRuntime, namespace string) (isolationNamespace, error) {
	data, err := runtime.Kubectl.GetNamespace(ctx, namespace)
	if err != nil {
		return isolationNamespace{}, err
	}
	return parseIsolationNamespace(data)
}

// isolationReadLimitRange reads one tenant's LimitRange.
func isolationReadLimitRange(ctx context.Context, runtime *isolationRuntime, tenant isolationTenant) (isolationResources, error) {
	data, err := runtime.Kubectl.Run(ctx, tenant.Namespace, "get", "limitranges", isolationLimitRangeName, "-o", "json")
	if err != nil {
		return isolationResources{}, err
	}
	return parseIsolationLimitRange(data, tenant.ID, tenant.Namespace)
}

// isolationReadNetworkPolicy reads one tenant's default-deny policy.
func isolationReadNetworkPolicy(ctx context.Context, runtime *isolationRuntime, tenant isolationTenant) (isolationNetworkPolicy, error) {
	data, err := runtime.Kubectl.Run(ctx, tenant.Namespace, "get", "networkpolicies", isolationNetworkPolicyName, "-o", "json")
	if err != nil {
		return isolationNetworkPolicy{}, err
	}
	return parseIsolationNetworkPolicy(data)
}

// isolationPodSelector is the product label that attributes a workload Pod to
// one execution.
const isolationExecutionLabel = "kodama.dev/execution"

// isolationWaitPod polls one execution/namespace selector until exactly one
// Pod carries a pod address, then returns its bounded facts.
func isolationWaitPod(ctx context.Context, kubectl *Kubectl, namespace, selector string, timeout time.Duration) (isolationPod, error) {
	var observed isolationPod
	_, err := (resilienceWaiter{Timeout: timeout}).Wait(ctx, func(ctx context.Context) (bool, error) {
		data, err := kubectl.Run(ctx, namespace, "get", "pods", "-l", selector, "-o", "json")
		if err != nil {
			return false, nil
		}
		pods, err := parseIsolationPods(data)
		if err != nil {
			return false, err
		}
		switch len(pods) {
		case 0:
			return false, nil
		case 1:
			if pods[0].Status.PodIP == "" {
				return false, nil
			}
			observed = pods[0]
			return true, nil
		default:
			return false, fmt.Errorf("selector %q matched %d pods; want exactly one", selector, len(pods))
		}
	})
	if err != nil {
		return isolationPod{}, fmt.Errorf("no workload pod for selector %q in namespace %q: %w", selector, namespace, err)
	}
	return observed, nil
}

// isolationExecutionPods lists every pod carrying one execution label in a
// namespace.
func isolationExecutionPods(ctx context.Context, kubectl *Kubectl, namespace, executionID string) ([]isolationPod, error) {
	data, err := kubectl.Run(ctx, namespace, "get", "pods", "-l", isolationExecutionLabel+"="+executionID, "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseIsolationPods(data)
}

// isolationSecret is one bounded Secret object with base64 data.
type isolationSecret struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

// isolationSecretList is one bounded Secret list.
type isolationSecretList struct {
	Items []isolationSecret `json:"items"`
}

// isolationReadSecret reads one named Secret without echoing its data.
func isolationReadSecret(ctx context.Context, kubectl *Kubectl, namespace, name string) (isolationSecret, error) {
	data, err := kubectl.Run(ctx, namespace, "get", "secrets", name, "-o", "json")
	if err != nil {
		return isolationSecret{}, err
	}
	var object isolationSecret
	if err := json.Unmarshal(data, &object); err != nil {
		return isolationSecret{}, fmt.Errorf("Secret JSON is unreadable")
	}
	if object.Metadata.Name != name {
		return isolationSecret{}, fmt.Errorf("Secret object is %q, want %q", object.Metadata.Name, name)
	}
	return object, nil
}

// isolationExecutionSecrets lists the execution-labelled Secrets in one
// namespace.
func isolationExecutionSecrets(ctx context.Context, kubectl *Kubectl, namespace, executionID string) ([]isolationSecret, error) {
	data, err := kubectl.Run(ctx, namespace, "get", "secrets", "-l", isolationExecutionLabel+"="+executionID, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list isolationSecretList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("Secret list is unreadable")
	}
	return list.Items, nil
}

// isolationCaptureExecutionCredential polls the execution-labelled Secrets in
// the tenant namespace and returns the mounted execution credential. The value
// is kept as a Secret and never rendered.
func isolationCaptureExecutionCredential(ctx context.Context, kubectl *Kubectl, namespace, executionID string, timeout time.Duration) (Secret, error) {
	var token Secret
	_, err := (resilienceWaiter{Timeout: timeout}).Wait(ctx, func(ctx context.Context) (bool, error) {
		data, err := kubectl.Run(ctx, namespace, "get", "secrets", "-l", isolationExecutionLabel+"="+executionID, "-o", "json")
		if err != nil {
			return false, nil
		}
		var list isolationSecretList
		if err := json.Unmarshal(data, &list); err != nil {
			return false, fmt.Errorf("execution credential Secret list is unreadable")
		}
		switch len(list.Items) {
		case 0:
			return false, nil
		case 1:
			encoded := list.Items[0].Data["KODAMA_TOKEN"]
			if encoded == "" {
				return false, fmt.Errorf("the execution credential Secret has no KODAMA_TOKEN")
			}
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || len(raw) == 0 {
				return false, fmt.Errorf("the execution credential is not base64 data")
			}
			value := string(raw)
			parsed, err := stageCredential(&value, "kodex.")
			if err != nil {
				return false, fmt.Errorf("the execution credential is malformed: %w", err)
			}
			token = parsed
			return true, nil
		default:
			return false, fmt.Errorf("the execution selector matched %d Secrets; want exactly one", len(list.Items))
		}
	})
	if err != nil {
		return "", fmt.Errorf("execution %q credential was not captured: %w", executionID, err)
	}
	return token, nil
}

// isolationNotFoundError requires the uniform cross-tenant refusal: a public
// 404 not_found. Any success, any other status and any transport error is an
// error.
func isolationNotFoundError(err error) error {
	var public *APIError
	if !containsAPIError(err, &public) {
		return fmt.Errorf("not refused with a public error: %w", err)
	}
	if public.Status != http.StatusNotFound || public.Code != api.CodeNotFound {
		return fmt.Errorf("status %d code %q; want 404 not_found", public.Status, public.Code)
	}
	return nil
}

// isolationExpectNotFound fails unless the helper saw the uniform refusal.
func isolationExpectNotFound(t *testing.T, label string, err error) {
	t.Helper()
	if refusal := isolationNotFoundError(err); refusal != nil {
		t.Fatalf("%s %v", label, refusal)
	}
}

// containsAPIError is errors.As without importing errors at every call site.
func containsAPIError(err error, target **APIError) bool {
	for err != nil {
		if candidate, ok := err.(*APIError); ok {
			*target = candidate
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// isolationPipelineDocument is a stored pipeline body used only to prove a
// foreign read cannot see it. The timeout must satisfy the admitted minimum
// (one minute) even though the probe never starts this pipeline.
func isolationPipelineDocument(image, marker string) []byte {
	return resiliencePipelineYAML(image, "isolation-probe", "1m", []string{
		"echo " + shellQuote(marker),
	})
}

// isolationNetworkImage selects the image for the network listener/client pair.
// It must provide sh, curl, timeout, nc, kill and sleep; Q02_NETWORK_IMAGE lets
// a stage pin a small tool image for it while Q02_IMAGE stays the general
// workload image (an image without nc fails the runtime probe visibly).
func isolationNetworkImage(config Config) string {
	if config.NetworkImage != "" {
		return config.NetworkImage
	}
	return config.Image
}

// isolationWorkspacePipeline writes this tenant's marker into its own
// /workspace, waits for the peer to write its marker, then fails if the peer's
// file is visible.
func isolationWorkspacePipeline(image, ownFile, ownContent, otherFile, logMarker, isolatedMarker string) []byte {
	leak := shellQuote("q02-cross-tenant-workspace-visible")
	command := fmt.Sprintf(
		"printf '%%s' %s > /workspace/%s\n"+
			"echo %s\n"+
			"if [ -e /workspace/%s ]; then echo %s; exit 1; fi\n"+
			"sleep 45\n"+
			"if [ -e /workspace/%s ]; then echo %s; exit 1; fi\n"+
			"echo %s\n",
		shellQuote(ownContent), ownFile, shellQuote(logMarker), otherFile, leak, otherFile, leak, shellQuote(isolatedMarker))
	return resiliencePipelineYAML(image, "workspace", "2m", []string{command})
}

// isolationNetworkServerPipeline starts a bounded listener on tenant B. The
// listener loops for far longer than the client's pull, activation and curl
// window, so a slow start can never let the listener die first and turn the
// cross-tenant refusal into a vacuous pass. It prefers nc and falls back to
// python3 when the workload image carries python3 instead of nc (such as the
// digest-pinned buildpack-deps tools image); if neither is present or the
// listener exits non-zero, the action fails visibly and the probe separately
// requires the server execution to still be non-terminal once the client is
// terminal.
func isolationNetworkServerPipeline(image, readyMarker, doneMarker string) []byte {
	command := "if command -v nc >/dev/null 2>&1; then\n" +
		"  timeout 300 sh -c 'while :; do printf \"HTTP/1.1 200 OK\\r\\nContent-Length: 2\\r\\nConnection: close\\r\\n\\r\\nok\" | nc -l -p 18080 >/dev/null 2>&1; status=$?; if [ \"$status\" -ne 0 ]; then echo q02-net-listener-failed; exit 1; fi; sleep 0.1; done' &\n" +
		"elif command -v python3 >/dev/null 2>&1; then\n" +
		"  timeout 300 python3 -c 'import socket; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind((\"0.0.0.0\", 18080)); s.listen(16);\nwhile True:\n c,_=s.accept()\n try:\n  c.recv(1024); c.sendall(b\"HTTP/1.1 200 OK\\r\\nContent-Length: 2\\r\\nConnection: close\\r\\n\\r\\nok\")\n finally:\n  c.close()' &\n" +
		"else\n" +
		"  echo q02-nc-missing; exit 1\n" +
		"fi\n" +
		"server=$!\n" +
		"sleep 1\n" +
		"if ! kill -0 \"$server\" 2>/dev/null; then echo q02-net-listener-failed; exit 1; fi\n" +
		"echo " + shellQuote(readyMarker) + "\n" +
		"wait \"$server\" || true\n" +
		"echo " + shellQuote(doneMarker)
	return resiliencePipelineYAML(image, "net-server", "6m", []string{command})
}

// isolationNetworkClientPipeline proves tenant A cannot reach tenant B's
// listener while it can reach the Kodama service and cannot reach the
// Kubernetes API.
func isolationNetworkClientPipeline(image, targetIP, kodamaService, isolatedMarker string) []byte {
	command := fmt.Sprintf(
		"reached=0\n"+
			"for attempt in 1 2 3; do\n"+
			"  if curl -sS --noproxy '*' --connect-timeout 2 -m 3 %s >/dev/null 2>&1; then reached=1; fi\n"+
			"  sleep 1\n"+
			"done\n"+
			"if [ \"$reached\" = \"1\" ]; then echo q02-cross-tenant-reachable; exit 1; fi\n"+
			"if ! curl -sSk --noproxy '*' --connect-timeout 5 -m 10 %s >/dev/null; then echo q02-kodama-unreachable; exit 1; fi\n"+
			"if curl -sSk --noproxy '*' --connect-timeout 3 -m 5 'https://kubernetes.default.svc/api' >/dev/null 2>&1; then echo q02-api-reachable; exit 1; fi\n"+
			"echo %s\n",
		shellQuote("http://"+net.JoinHostPort(targetIP, "18080")+"/"),
		shellQuote("https://"+kodamaService+"."+ReleaseNamespace+".svc.cluster.local:8443/healthz"),
		shellQuote(isolatedMarker))
	return resiliencePipelineYAML(image, "net-client", "2m", []string{command})
}

// isolationGitAdvertisement performs one smart-HTTP upload-pack advertisement
// request. Only the status, content type and a bounded body are returned.
func isolationGitAdvertisement(ctx context.Context, client *http.Client, baseURL, repository string, token Secret) (int, http.Header, []byte, error) {
	if err := validateGitRepository(repository); err != nil {
		return 0, nil, nil, err
	}
	target, err := url.Parse(strings.TrimSuffix(baseURL, "/") + "/" + repository + "/info/refs")
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build the Git advertisement URL")
	}
	query := target.Query()
	query.Set("service", "git-upload-pack")
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build the Git advertisement request")
	}
	if token.IsSet() {
		request.SetBasicAuth("kodama", token.Reveal())
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("the Git advertisement request failed")
	}
	defer response.Body.Close()
	data, err := readBounded(response.Body, 64<<10)
	if err != nil {
		return 0, nil, nil, err
	}
	return response.StatusCode, response.Header.Clone(), data, nil
}

// isolationExpectGitAllowed requires a served advertisement.
func isolationExpectGitAllowed(t *testing.T, label string, status int, header http.Header, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if status != http.StatusOK {
		t.Fatalf("%s = status %d; want a 200 advertisement", label, status)
	}
	if !strings.Contains(header.Get("Content-Type"), "application/x-git-upload-pack-advertisement") {
		t.Fatalf("%s content type = %q; want a git-upload-pack advertisement", label, header.Get("Content-Type"))
	}
}

// isolationGitRefusedError requires a credential-scope refusal and never a
// served advertisement.
func isolationGitRefusedError(status int, err error) error {
	if err != nil {
		return err
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("status %d; want 401, 403 or 404", status)
	}
}

// isolationExpectGitRefused fails unless the helper saw a refusal.
func isolationExpectGitRefused(t *testing.T, label string, status int, err error) {
	t.Helper()
	if refusal := isolationGitRefusedError(status, err); refusal != nil {
		t.Fatalf("%s %v", label, refusal)
	}
}

// isolationFreePort reserves one loopback port for a guarded port-forward.
func isolationFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("reserved loopback address is not TCP")
	}
	return address.Port
}

// isolationGitClient builds a client that verifies the installation CA against
// the Git service's fixed cluster DNS name while connecting to a loopback
// port-forward.
func isolationGitClient(t *testing.T, config Config, service string) *http.Client {
	t.Helper()
	caPEM, err := os.ReadFile(config.CAPEM)
	if err != nil {
		t.Fatalf("read the installation CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("the installation CA contains no certificate")
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
				ServerName: service + "." + ReleaseNamespace + ".svc.cluster.local",
			},
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// isolationStartGitForward opens one guarded port-forward to a tenant Git
// service and returns the loopback base URL.
func isolationStartGitForward(t *testing.T, runtime *isolationRuntime, service string) string {
	t.Helper()
	port := isolationFreePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	forward, err := StartPortForward(ctx, "kubectl", runtime.Kubectl.Guard, ReleaseNamespace, service, port, 8443)
	if err != nil {
		cancel()
		t.Fatalf("start the port-forward to Git service %q: %v", service, err)
	}
	t.Cleanup(func() {
		_ = forward.Close()
		cancel()
	})
	return "https://127.0.0.1:" + strconv.Itoa(port)
}

// isolationWaitGitForward polls one advertisement until the forward answers.
func isolationWaitGitForward(t *testing.T, client *http.Client, baseURL, repository string, token Secret) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, _, _, err := isolationGitAdvertisement(context.Background(), client, baseURL, repository, token)
		if err == nil && status != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Git port-forward did not answer within 30s")
		}
		time.Sleep(500 * time.Millisecond)
	}
}
