package staging_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/support/stage"
)

// TestStageCleanInstall is stage_clean_install: the release is installed into a clean namespace set (no
// release, no workload and no per-attempt bookkeeping left by earlier use) and reaches readiness under
// KODAMA_ENV=production with the core assembly.
func TestStageCleanInstall(t *testing.T) {
	i := stage.New(t)
	ctx := i.Context(20 * time.Minute)
	if i.Installed(ctx) {
		if err := i.Uninstall(ctx); err != nil {
			t.Fatalf("uninstall the earlier release: %v", err)
		}
	}
	leftover := regexp.MustCompile(`^secret/kdm-.*-adm$`)
	for _, tenant := range i.S.Tenants {
		for _, kind := range []string{"job", "pod"} {
			if _, err := i.Kube.Run(ctx, "-n", tenant.Namespace, "delete", kind, "--all", "--wait=true", "--timeout=3m"); err != nil {
				t.Fatalf("clear %ss of %s: %v", kind, tenant.Namespace, err)
			}
		}
		names, err := i.Kube.Run(ctx, "-n", tenant.Namespace, "get", "secret", "-o", "name")
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range strings.Fields(names) {
			if leftover.MatchString(n) {
				if _, err := i.Kube.Run(ctx, "-n", tenant.Namespace, "delete", n, "--ignore-not-found"); err != nil {
					t.Fatalf("clear bookkeeping of %s: %v", tenant.Namespace, err)
				}
			}
		}
		if out, _ := i.Kube.Run(ctx, "-n", tenant.Namespace, "get", "job,pod,secret", "-o", "name"); regexp.MustCompile(`(?m)^(job|pod)/|^secret/kdm-`).MatchString(out) {
			t.Fatalf("namespace %s is not clean", tenant.Namespace)
		}
	}
	if out, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "deploy", i.Deployment(), "-o", "name", "--ignore-not-found"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("the server Deployment still exists (%v)", err)
	}
	if err := i.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}
	i.WaitReady(ctx)
	deploy, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "deploy", i.Deployment(), "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"--assembly"`, `"core"`, `"KODAMA_ENV"`, `"production"`} {
		if !strings.Contains(deploy, want) {
			t.Fatalf("the deployed server lacks %s", want)
		}
	}
	if me, err := i.API("admin").GetMeWithResponse(ctx); err != nil || me.StatusCode() != http.StatusOK {
		t.Fatalf("the installation does not serve its API: %v", err)
	}
}

// TestStageE2ERoundtrip is stage_e2e_roundtrip: a pipeline is submitted, runs on Kubernetes, and its logs and
// artifact are retained and readable after the workload is gone.
func TestStageE2ERoundtrip(t *testing.T) {
	i := stage.New(t)
	w := i.Seed()
	marker := "stage-roundtrip-" + suffix(t)
	item := w.WorkItem("Staging roundtrip", "# staging roundtrip")
	w.Provision(item.Id)
	def := w.Definition("stage-roundtrip", stage.Step{
		Script:    fmt.Sprintf(`echo %s; echo "stage artifact" > /kodama/outputs/report; printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`, marker),
		Timeout:   300,
		Outputs:   []string{"v"},
		Artifacts: map[string]string{"report": "/kodama/outputs/report"},
	})
	run := w.Run("stage-roundtrip", item, def.Id)
	attempt := w.WaitAttempt(run.Id, time.Minute)
	if final := w.WaitRun(run.Id, 5*time.Minute); final.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", final.Status, final.Reason)
	}
	outs := *w.Steps(run.Id)[0].Outputs
	if v, err := outs["v"].AsRunsScalar0(); err != nil || v != "ok" {
		t.Fatalf("scalar output %q (%v)", v, err)
	}
	artifactID, err := outs["report"].AsRunsScalar0()
	if err != nil || artifactID == "" {
		t.Fatalf("artifact output %v (%v)", outs["report"], err)
	}
	if got := string(downloadArtifact(t, i, w, artifactID)); got != "stage artifact\n" {
		t.Fatalf("artifact bytes %q", got)
	}
	w.WaitRemoved(attempt)
	if lines := strings.Join(w.WaitLogs("engineer", run.Id), "\n"); !strings.Contains(lines, marker) {
		t.Fatalf("the retained log lacks the step's output")
	}
}

// TestStageRestartReconcile is stage_restart_reconcile: a run is cancelled and its workload removed; a second
// run survives the server being killed (the replacement adopts the same Job) and then finishes.
func TestStageRestartReconcile(t *testing.T) {
	i := stage.New(t)
	w := i.Seed()
	barrier := held(w, "stage-restart", `echo reconciled`)

	cancelled, cancelledAttempt, _, _ := runHeld(t, i, w, barrier, "stage-cancel")
	if r := w.CancelRun(cancelled.Id); r.StatusCode() != http.StatusAccepted {
		t.Fatalf("cancel: HTTP %d", r.StatusCode())
	}
	if final := w.WaitRun(cancelled.Id, 4*time.Minute); final.Status != client.RunStatusCancelled {
		t.Fatalf("cancelled run is %s (%s)", final.Status, final.Reason)
	}
	w.WaitRemoved(cancelledAttempt)

	run, attempt, ns, pod := runHeld(t, i, w, barrier, "stage-restart")
	jobs := i.JobsOfAttempt(ns, attempt)
	if len(jobs) != 1 {
		t.Fatalf("%d Jobs before the restart, want 1", len(jobs))
	}
	uid := jobs[0].UID
	i.Crash(i.Context(6 * time.Minute))
	if got := w.GetRun(run.Id); got.Status != client.RunStatusRunning {
		t.Fatalf("the run is %s after the restart, want running", got.Status)
	}
	after := i.JobsOfAttempt(ns, attempt)
	if len(after) != 1 || after[0].UID != uid {
		t.Fatal("the restarted server did not adopt the running Job")
	}
	i.ReleaseCommand(ns, pod)
	if final := w.WaitRun(run.Id, 5*time.Minute); final.Status != client.RunStatusSucceeded {
		t.Fatalf("run after the restart is %s (%s)", final.Status, final.Reason)
	}
	w.WaitRemoved(attempt)
	if n := i.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id=$1`, run.Id); n != 1 {
		t.Fatalf("%d attempts, want 1", n)
	}
}

// TestStageLokiLogSearch is stage_loki_log_search: pipeline (workload) and control-plane (server) logs of one
// attempt are found by attempt_id in Loki through Grafana, which refuses an unauthenticated caller.
func TestStageLokiLogSearch(t *testing.T) {
	i := stage.New(t)
	g := openGrafana(t, i)
	if code, _, err := g.get("/api/datasources/proxy/uid/kodama-loki/loki/api/v1/labels", false); err != nil || code == http.StatusOK {
		t.Fatalf("Grafana answered an unauthenticated Loki query with HTTP %d (%v)", code, err)
	}
	if code, _, err := g.get("/api/datasources/proxy/uid/kodama-loki/loki/api/v1/labels", true); err != nil || code != http.StatusOK {
		t.Fatalf("the operator cannot query Loki through Grafana: HTTP %d (%v)", code, err)
	}
	w := i.Seed()
	_, run := w.Scripted("stage-loki", stage.Step{Script: `echo searchable`, Timeout: 120})
	attempt := w.WaitAttempt(run.Id, time.Minute)
	if final := w.WaitRun(run.Id, 5*time.Minute); final.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", final.Status, final.Reason)
	}
	w.WaitRemoved(attempt)
	tenantNS := w.Namespace(w.TenantA)
	queries := map[string]string{
		"pipeline (workload)":    fmt.Sprintf(`{namespace=%q} | attempt_id=%q`, tenantNS, attempt),
		"control plane (server)": fmt.Sprintf(`{namespace=%q, container="server"} | attempt_id=%q`, i.S.Namespace, attempt),
	}
	for name, query := range queries {
		stage.WaitForEvery(t, "Loki to return "+name+" logs by attempt_id", 6*time.Minute, 10*time.Second, func() bool {
			code, lines, err := g.lines(query, time.Hour)
			if err != nil || code != http.StatusOK || len(lines) == 0 {
				return false
			}
			for _, l := range lines {
				if !strings.Contains(l, attempt) {
					t.Fatalf("a %s line matched the attempt filter without naming the attempt", name)
				}
			}
			return true
		})
	}
}

// TestStageDevAdapterRefused is stage_dev_adapter_refused: the deployed configuration is the production one,
// and the deployed image, started with that configuration plus one development or test selection, stops
// before serving and names the refused adapter.
func TestStageDevAdapterRefused(t *testing.T) {
	i := stage.New(t)
	ctx := i.Context(15 * time.Minute)
	i.EnsureReady(ctx)
	deploy, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "deploy", i.Deployment(), "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(deploy, `"KODAMA_ENV"`) || !strings.Contains(deploy, `"production"`) {
		t.Fatal("the deployed server is not KODAMA_ENV=production")
	}
	for _, forbidden := range []string{"ALLOW_INSECURE_HTTP", "KODAMA_SECRETS_ADAPTER", "KODAMA_RUNTIME_STATE_DIR", "KODAMA_ENTRYPOINT_BINARY"} {
		if strings.Contains(deploy, forbidden) {
			t.Fatalf("the deployed server selects %s", forbidden)
		}
	}
	settings, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "configmap", i.S.Release+"-config", "-o", "jsonpath={.data}")
	if err != nil || !strings.Contains(settings, "kms") || strings.Contains(settings, "keyfile") {
		t.Fatalf("the settings document does not select the cloud KMS (%v)", err)
	}

	cases := []struct{ name, env, value, refused string }{
		{"development key-encryption key", "KODAMA_SECRETS_ADAPTER", "keyfile", "production refuses the keyfile adapter"},
		{"OIDC fixture over plain HTTP", "KODAMA_OIDC_ALLOW_INSECURE_HTTP", "true", "production refuses the oidc-fixture adapter"},
		{"S3 fixture over plain HTTP", "KODAMA_S3_ENDPOINT", "http://127.0.0.1:9000", "production refuses the localstack-s3 adapter"},
		{"Process/Podman runtime", "KODAMA_RUNTIME_STATE_DIR", "/tmp/runtime-state", "production refuses the process/podman adapter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := runOverridden(t, i, deploy, c.env, c.value)
			if !strings.Contains(logs, c.refused) {
				t.Fatalf("the server did not refuse the selection (log %.300q)", logs)
			}
		})
	}
}

// runOverridden starts the deployed server's pod specification as a one-shot Pod with one extra environment
// variable, requires it to exit non-zero without becoming ready, and returns its log.
func runOverridden(t *testing.T, i *stage.Installation, deployJSON, name, value string) string {
	t.Helper()
	var d struct {
		Spec struct {
			Template struct {
				Spec map[string]any `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(deployJSON), &d); err != nil {
		t.Fatal(err)
	}
	spec := d.Spec.Template.Spec
	spec["restartPolicy"] = "Never"
	for _, c := range spec["containers"].([]any) {
		c := c.(map[string]any)
		if c["name"] != "server" {
			continue
		}
		for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
			delete(c, probe)
		}
		c["env"] = append(c["env"].([]any), map[string]any{"name": name, "value": value})
	}
	pod := "kq-refuse-" + suffix(t)
	manifest, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": pod, "namespace": i.S.Namespace, "labels": map[string]string{"kodama.optimi.com/qualification-session": i.S.Name}},
		"spec":     spec,
	})
	ctx := i.Context(5 * time.Minute)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = i.Kube.Run(c, "-n", i.S.Namespace, "delete", "pod", pod, "--ignore-not-found", "--wait=false")
	})
	if _, err := i.Kube.RunInput(ctx, manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("start the overridden server: %v", err)
	}
	var phase string
	stage.WaitForEvery(t, "the overridden server to stop", 3*time.Minute, 3*time.Second, func() bool {
		out, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "pod", pod, "-o", "jsonpath={.status.phase}")
		phase = strings.TrimSpace(out)
		return err == nil && (phase == "Failed" || phase == "Succeeded")
	})
	if phase != "Failed" {
		t.Fatalf("the overridden server ended %s, want Failed (non-zero exit)", phase)
	}
	logs, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "logs", pod, "-c", "server")
	if err != nil {
		t.Fatalf("read the refusal: %v", err)
	}
	return logs
}

// TestStageRetentionUninstall is stage_retention_uninstall: uninstalling the release removes the workload and
// retains the database, the object-store bucket and its data, and the installation's Key Manager keys.
// It is last: it leaves the session uninstalled.
func TestStageRetentionUninstall(t *testing.T) {
	i := stage.New(t)
	ctx := i.Context(20 * time.Minute)
	c := loadContract(t)
	i.EnsureReady(ctx)
	attemptsBefore := i.OwnerCount(`SELECT count(*) FROM step_attempts`)
	if attemptsBefore == 0 {
		t.Fatal("the session holds no run records to retain; the lifecycle tests did not run first")
	}
	if err := i.Uninstall(ctx); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if i.Installed(ctx) {
		t.Fatal("the release still exists after uninstall")
	}
	if out, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "deploy", i.Deployment(), "-o", "name", "--ignore-not-found"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("the server Deployment survived the uninstall (%v)", err)
	}
	// Database: schema and rows are retained.
	if n := i.OwnerCount(`SELECT count(*) FROM step_attempts`); n != attemptsBefore {
		t.Fatalf("run records changed across uninstall: %d before, %d after", attemptsBefore, n)
	}
	// Object store: the bucket and this installation's data are retained.
	accessKey, err := i.Kube.SecretValue(ctx, i.S.Namespace, "kodama-runtime", "s3-access-key")
	if err != nil {
		t.Fatalf("object store credential: %v", err)
	}
	secretKey, err := i.Kube.SecretValue(ctx, i.S.Namespace, "kodama-runtime", "s3-secret-key")
	if err != nil {
		t.Fatalf("object store credential: %v", err)
	}
	store := s3.New(s3.Options{Region: c.ObjectStorage.Region, BaseEndpoint: aws.String(c.ObjectStorage.Endpoint), Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")})
	if _, err := store.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(c.ObjectStorage.ArtifactsBucket)}); err != nil {
		t.Fatalf("the artifacts bucket is not retained: %T", err)
	}
	listed, err := store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(c.ObjectStorage.ArtifactsBucket), Prefix: aws.String(c.ObjectStorage.ArtifactsPrefix), MaxKeys: aws.Int32(5)})
	if err != nil || len(listed.Contents) == 0 {
		t.Fatalf("the installation's artifacts are not retained (%T)", err)
	}
	// Key Manager: the installation's keys still exist and are usable (operator read; no key is modified).
	if c.KMS.Provider != "scw" || len(c.KMS.Resources) == 0 {
		t.Fatal("the contract names no Scaleway Key Manager keys")
	}
	for label, resource := range c.KMS.Resources {
		id := resource[strings.LastIndex(resource, "/")+1:]
		req, _ := http.NewRequest(http.MethodGet, "https://api.scaleway.com/key-manager/v1alpha1/regions/fr-par/keys/"+id, nil)
		req.Header.Set("X-Auth-Token", os.Getenv("SCW_SECRET_KEY"))
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("read key %s: transport failed", label)
		}
		var key struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&key)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || key.State != "enabled" {
			t.Fatalf("installation key %s is not retained and enabled (HTTP %d, state %q)", label, resp.StatusCode, key.State)
		}
	}
}
