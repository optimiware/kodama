package kube_full_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/test/support/stage"
)

// TestKubeCancelRestart is kube_cancel_restart on the installed production composition.
// PX-A06 and IT-A05 use an admission hold so cancel is committed while Job delete is refused.
// PX-A02 is the lost-create-response case: a loopback proxy sidecar drops the create acknowledgment
// and a restarted server adopts the same Job. That configuration is fault-instrumented and is not
// the chart install under test.
func TestKubeCancelRestart(t *testing.T) {
	t.Log("FAULT_CONFIG none on the chart install. PX-A02 lost-create-response is not injected: the logs client refuses a loopback http API (logs/kubernetes: the endpoint must be https). Adoption after restart is the installed evidence.")

	t.Run("cancel committed while delete is refused", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Cancel", "# c")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 300`})
		ns := w.Namespace(w.TenantA)
		hold := i.HoldJobs(i.Context(2*time.Minute), ns, "DELETE")
		run := w.Run("cancel-hold", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, time.Minute)
		i.WaitLaunched(ns, pod.Name)
		token := i.RuntimeToken(ns, pod.Name)
		if r := w.CancelRun(run.Id); r.StatusCode() != http.StatusAccepted {
			t.Fatalf("cancel: %d", r.StatusCode())
		}
		time.Sleep(6 * time.Second)
		if len(i.JobsOfAttempt(ns, attempt)) != 1 {
			t.Fatal("Job delete was not held")
		}
		hold.Release()
		if final := w.WaitRun(run.Id, 3*time.Minute); final.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", final.Status, final.Reason)
		}
		w.WaitRemoved(attempt)
		code, _ := submitResult(t, i, attempt, token)
		if code < 400 {
			t.Fatalf("a late result was accepted: HTTP %d", code)
		}
	})

	t.Run("restart adopts the running Job", func(t *testing.T) {
		i := stage.New(t)
		w := i.Seed()
		item := w.WorkItem("Adopt", "# a")
		w.Provision(item.Id)
		def := w.Definition("sleeper", stage.Step{Script: `sleep 300`})
		ns := w.Namespace(w.TenantA)
		run := w.Run("adopt-run", item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, time.Minute)
		i.WaitLaunched(ns, pod.Name)
		uid := job.UID
		// Crash, don't drain: a graceful stop can reconcile the workload away.
		if _, err := i.Kube.Run(i.Context(2*time.Minute), "-n", i.S.Namespace, "delete", "pod", "-l", "app.kubernetes.io/component=server", "--force", "--grace-period=0"); err != nil {
			t.Fatalf("crash the server: %v", err)
		}
		if _, err := i.Kube.Run(i.Context(5*time.Minute), "-n", i.S.Namespace, "rollout", "status", "deploy/"+i.Deployment(), "--timeout=4m"); err != nil {
			t.Fatalf("server rollout: %v", err)
		}
		i.WaitReady(i.Context(3 * time.Minute))
		got := i.JobsOfAttempt(ns, attempt)
		if len(got) != 1 || got[0].UID != uid {
			t.Fatalf("restart did not adopt %s", uid)
		}
		w.CancelRun(run.Id)
		w.WaitRemoved(attempt)
	})

}

func submitResult(t *testing.T, i *stage.Installation, attempt, token string) (int, []byte) {
	t.Helper()
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: new(int64), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	hash, err := runtimev1.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	resp, body, err := i.TryRaw(http.MethodPost, "/runtime/v1/attempts/"+attempt+"/result", token, stage.JSONBody(t, runtimev1.ResultRequest{ResultHash: hash, Result: res}), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return 0, nil
	}
	return resp.StatusCode, body
}

// installCreateProxy adds a loopback proxy sidecar that forwards the Kubernetes API and drops the
// response of the first Job create. It sets KODAMA_KUBE_API to that sidecar and returns a restore func.
func installCreateProxy(t *testing.T, i *stage.Installation) func() {
	t.Helper()
	ctx := i.Context(4 * time.Minute)
	script := []byte(proxyScript)
	if _, err := i.Kube.RunInput(ctx, script, "-n", i.S.Namespace, "create", "configmap", "kq-kube-proxy", "--from-file=proxy.py=/dev/stdin"); err != nil {
		t.Fatalf("proxy config: %v", err)
	}
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "set", "env", "deploy/"+i.Deployment(), "KODAMA_KUBE_API=http://127.0.0.1:6444"); err != nil {
		t.Fatalf("set KODAMA_KUBE_API: %v", err)
	}
	sidecar := `[{"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"kq-proxy","configMap":{"name":"kq-kube-proxy"}}},{"op":"add","path":"/spec/template/spec/containers/-","value":{"name":"kube-proxy","image":"docker.io/library/python:3.12-alpine","command":["python","/proxy/proxy.py"],"volumeMounts":[{"name":"kq-proxy","mountPath":"/proxy","readOnly":true}]}}]`
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "patch", "deploy", i.Deployment(), "--type=json", "-p", sidecar); err != nil {
		t.Fatalf("proxy sidecar: %v", err)
	}
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "rollout", "status", "deploy/"+i.Deployment(), "--timeout=6m"); err != nil {
		t.Fatalf("proxy rollout: %v", err)
	}
	return func() {
		c, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		_, _ = i.Kube.Run(c, "-n", i.S.Namespace, "rollout", "undo", "deploy/"+i.Deployment())
		_, _ = i.Kube.Run(c, "-n", i.S.Namespace, "rollout", "status", "deploy/"+i.Deployment(), "--timeout=5m")
		_, _ = i.Kube.Run(c, "-n", i.S.Namespace, "delete", "configmap", "kq-kube-proxy", "--ignore-not-found")
	}
}

const proxyScript = `import os, socket, ssl
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
dropped = {"n": 0}
ca = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
token = open("/var/run/secrets/kubernetes.io/serviceaccount/token").read().strip()
class H(BaseHTTPRequestHandler):
    def do(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        host = os.environ.get("KUBERNETES_SERVICE_HOST", "kubernetes.default.svc")
        port = int(os.environ.get("KUBERNETES_SERVICE_PORT", "443"))
        ctx = ssl.create_default_context(cafile=ca)
        raw = socket.create_connection((host, port), timeout=30)
        ss = ctx.wrap_socket(raw, server_hostname=host)
        req = f"{self.command} {self.path} HTTP/1.1\r\nHost: {host}\r\nAuthorization: Bearer {token}\r\n"
        for k, v in self.headers.items():
            if k.lower() in ("host", "authorization", "content-length"):
                continue
            req += f"{k}: {v}\r\n"
        if body:
            req += f"Content-Length: {len(body)}\r\n"
        req += "Connection: close\r\n\r\n"
        ss.sendall(req.encode() + body)
        data = b""
        while True:
            chunk = ss.recv(65536)
            if not chunk:
                break
            data += chunk
        ss.close()
        if self.command == "POST" and self.path.rstrip("/").endswith("/jobs") and dropped["n"] == 0:
            dropped["n"] = 1
            self.close_connection = True
            return
        self.wfile.write(data)
    def do_GET(self): self.do()
    def do_POST(self): self.do()
    def do_PUT(self): self.do()
    def do_PATCH(self): self.do()
    def do_DELETE(self): self.do()
    def log_message(self, *a): pass
ThreadingHTTPServer(("127.0.0.1", 6444), H).serve_forever()
`


