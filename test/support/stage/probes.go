package stage

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"
)

func (i *Installation) ProbeImage() string {
	i.T.Helper()
	image := os.Getenv("KODAMA_STAGE_PROBE_IMAGE")
	if !regexp.MustCompile(`^[^\s]+@sha256:[0-9a-f]{64}$`).MatchString(image) {
		i.T.Fatal("KODAMA_STAGE_PROBE_IMAGE must pin the test-only helper by digest (see deploy/stage/README.md)")
	}
	return image
}

// ApplyFixture applies an in-memory manifest without including its possibly sensitive body in errors.
func (i *Installation) ApplyFixture(ctx context.Context, object any) {
	i.T.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		i.T.Fatal("fixture manifest encoding failed")
	}
	if _, err := i.Kube.RunInput(ctx, data, "apply", "-f", "-"); err != nil {
		i.T.Fatalf("fixture apply: %v", err)
	}
}

func (i *Installation) RemoveFixture(kind, name string) {
	i.T.Helper()
	i.T.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := i.Kube.Run(c, "-n", i.S.Namespace, "delete", kind, name, "--ignore-not-found", "--wait=true", "--timeout=90s"); err != nil {
			i.T.Errorf("cleanup fixture %s %s: %v", kind, name, err)
		}
	})
}

// RunKMSProbe uses the deployment service account and its projected KMS credential, not the
// operator's identity. The helper's only output is allowlisted pass/fail metadata.
func (i *Installation) RunKMSProbe(ctx context.Context, config any, deniedToken string) {
	i.T.Helper()
	name := "kq-kms-" + random(4)
	sa, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "deploy", i.Deployment(), "-o", "jsonpath={.spec.template.spec.serviceAccountName}")
	if err != nil {
		i.T.Fatal(err)
	}
	data, _ := json.Marshal(config)
	i.RemoveFixture("secret", name)
	i.ApplyFixture(ctx, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": name, "namespace": i.S.Namespace}, "data": map[string][]byte{"config.json": data, "secret-key": []byte(deniedToken)}})
	security := map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}}
	var values struct {
		Secrets struct {
			KMS struct {
				SCW struct {
					CredentialsSecret string `yaml:"credentialsSecret"`
				} `yaml:"scw"`
			} `yaml:"kms"`
		} `yaml:"secrets"`
	}
	i.ReadValues(&values)
	if values.Secrets.KMS.SCW.CredentialsSecret == "" {
		i.T.Fatal("session does not project Scaleway workload credentials")
	}
	i.RemoveFixture("job", name)
	i.ApplyFixture(ctx, map[string]any{
		"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]string{"name": name, "namespace": i.S.Namespace},
		"spec": map[string]any{"backoffLimit": 0, "activeDeadlineSeconds": 360, "template": map[string]any{"spec": map[string]any{
			"restartPolicy": "Never", "serviceAccountName": strings.TrimSpace(sa), "automountServiceAccountToken": false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{"name": "probe", "image": i.ProbeImage(), "args": []string{"-mode=kms"}, "securityContext": security,
				"resources":    map[string]any{"limits": map[string]string{"memory": "256Mi", "cpu": "500m"}},
				"volumeMounts": []any{map[string]any{"name": "input", "mountPath": "/probe", "readOnly": true}, map[string]any{"name": "input", "mountPath": "/denied", "readOnly": true}, map[string]any{"name": "kms", "mountPath": "/kms", "readOnly": true}}}},
			"volumes": []any{map[string]any{"name": "input", "secret": map[string]any{"secretName": name, "defaultMode": 0440}}, map[string]any{"name": "kms", "secret": map[string]any{"secretName": values.Secrets.KMS.SCW.CredentialsSecret, "defaultMode": 0440}}},
		}}},
	})
	i.T.Cleanup(func() {
		if i.T.Failed() {
			i.Diagnose()
		}
	})
	var succeeded bool
	WaitForEvery(i.T, "in-cluster KMS procedure", 7*time.Minute, 2*time.Second, func() bool {
		var status struct {
			Status struct{ Succeeded, Failed int }
		}
		if i.Kube.JSON(context.Background(), &status, "-n", i.S.Namespace, "get", "job", name) != nil {
			return false
		}
		succeeded = status.Status.Succeeded == 1
		return succeeded || status.Status.Failed > 0
	})
	logs, err := i.Kube.Run(i.Context(time.Minute), "-n", i.S.Namespace, "logs", "job/"+name)
	if err == nil {
		for _, line := range strings.Split(logs, "\n") {
			if regexp.MustCompile(`^(kms: |PASS kms )[a-zA-Z0-9 _:.=-]{1,200}$`).MatchString(line) {
				i.T.Log(line)
			}
		}
	}
	if !succeeded {
		i.T.Fatal("in-cluster KMS procedure failed (only safe helper phase/class metadata logged)")
	}
}
