package kube_full_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/support/stage"
)

// directSecretDelivery qualifies the delivery the specifications describe (VS-03, VS-A10, O-19): an ordinary
// step that declares project secrets, with no deployment environment, receives exactly the pinned values as an
// ENV binding and as a file, before and after a server restart; the values are never in the Job or Pod objects;
// and a reference to a secret the project does not hold is refused when the definition is registered.
func directSecretDelivery(t *testing.T) {
	i := stage.New(t)
	w := i.Seed()
	ctx := i.Context(14 * time.Minute)
	suffix := runSuffix(t)
	secret := func(name string) []byte {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal("entropy failed")
		}
		value := []byte(hex.EncodeToString(raw))
		r, err := i.API("admin").CreateSecretWithResponse(ctx, w.TenantA, w.Project, &client.CreateSecretParams{IdempotencyKey: client.IdempotencyKey(name)}, client.CreateSecretRequest{Kind: client.SecretKindGeneric, Name: client.SecretName(name), Value: &value})
		if err != nil || r.StatusCode() != http.StatusCreated {
			t.Fatal("create project secret failed (body suppressed)")
		}
		return value
	}
	envName, fileName := "direct-env-"+suffix, "direct-file-"+suffix
	envValue, fileValue := secret(envName), secret(fileName)
	defer clear(envValue)
	defer clear(fileValue)
	digest := func(v []byte) string { sum := sha256.Sum256(v); return hex.EncodeToString(sum[:]) }

	// A reference to a secret that does not exist never reaches a run.
	missing := w.StepDocument(stage.Step{Script: "true", Env: map[string]string{"X": "secret://project/absent-" + suffix}})
	r, err := i.API("engineer").RegisterPipelineDefinitionWithBodyWithResponse(ctx, w.TenantA, w.Project, "application/json", stage.JSONBody(t, map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "absent-" + suffix},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{missing}},
	}))
	if err != nil || r.StatusCode() != http.StatusUnprocessableEntity || !strings.Contains(string(r.Body), "secret_denied") {
		t.Fatalf("a reference to an absent secret was not refused at registration (HTTP %v)", r.StatusCode())
	}

	script := stage.CommandBarrier + fmt.Sprintf(
		`f=$(echo /kodama/secrets/*/kms-file); test -f "$f"; `+
			`e=$(printf '%%s' "$KMS_ENV" | sha256sum); test "${e%%%% *}" = %s; `+
			`g=$(sha256sum < "$f"); test "${g%%%% *}" = %s; echo direct-secrets-consumed`,
		digest(envValue), digest(fileValue))
	def := w.Definition("direct-secrets", stage.Step{
		Script: script, Timeout: 600,
		Env:         map[string]string{"KMS_ENV": "secret://project/" + envName},
		SecretFiles: map[string]string{"kms-file": "secret://project/" + fileName},
	})
	ns := w.Namespace(w.TenantA)
	consume := func(label string) {
		item := w.WorkItem(label, "# direct secret delivery")
		w.Provision(item.Id)
		run := w.Run(label, item, def.Id)
		attempt := w.WaitAttempt(run.Id, time.Minute)
		job := i.WaitJob(ns, attempt, true, 2*time.Minute)
		pod := i.WaitPod(ns, job.Name, 2*time.Minute)
		i.WaitCommandBarrier(ns, pod.Name)
		// While the command waits, the delivered values exist only inside the step: the cluster objects hold none.
		objects, err := i.Kube.Run(ctx, "-n", ns, "get", "job,pod", "-o", "json")
		if err != nil {
			t.Fatalf("read workload objects: %v", err)
		}
		for name, v := range map[string][]byte{"ENV": envValue, "file": fileValue} {
			if strings.Contains(objects, string(v)) {
				t.Fatalf("%s secret value appears in a Job or Pod object", name)
			}
		}
		i.ReleaseCommand(ns, pod.Name)
		if final := w.WaitRun(run.Id, 4*time.Minute); final.Status != client.RunStatusSucceeded {
			t.Fatalf("%s failed: %s (%s)", label, final.Status, final.Reason)
		}
		w.WaitRemoved(attempt)
		logs := strings.Join(w.WaitLogs("engineer", run.Id), "\n")
		if strings.Contains(logs, string(envValue)) || strings.Contains(logs, string(fileValue)) || !strings.Contains(logs, "direct-secrets-consumed") {
			t.Fatal("secret use evidence missing or plaintext leaked")
		}
	}
	consume("before-direct-restart")
	i.Crash(i.Context(6 * time.Minute))
	consume("after-direct-restart")
}
