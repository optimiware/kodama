package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func (f fixture) created(t *testing.T) kubernetes.Job {
	t.Helper()
	j, err := f.fake.CreateJob(context.Background(), f.exp.Identity.Namespace, f.exp.Job)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func Test_verify_job_accepts_its_own_job(t *testing.T) {
	f := newFixture(t)
	v, err := kube.VerifyJob(f.exp, f.created(t))
	if err != nil || v.UID() == "" {
		t.Fatalf("own Job rejected: %v", err)
	}
}

// Test_verify_job_rejects_controlled_changes: every security-relevant mutation
// of an existing same-name Job is a foreign workload, whatever annotations claim.
func Test_verify_job_rejects_controlled_changes(t *testing.T) {
	ns := func(f fixture) string { return f.exp.Identity.Namespace }
	edit := func(f fixture, mut func(doc map[string]any)) error {
		j := f.created(t)
		f.fake.MutateJobRaw(ns(f), j.Metadata.Name, mut)
		got, _ := f.fake.GetJob(context.Background(), ns(f), j.Metadata.Name)
		_, err := kube.VerifyJob(f.exp, got)
		return err
	}
	pod := func(doc map[string]any) map[string]any {
		return doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	}
	container := func(doc map[string]any) map[string]any { return pod(doc)["containers"].([]any)[0].(map[string]any) }
	cases := map[string]func(doc map[string]any){
		"privileged": func(d map[string]any) {
			container(d)["securityContext"].(map[string]any)["privileged"] = true
		},
		"privilege escalation": func(d map[string]any) {
			container(d)["securityContext"].(map[string]any)["allowPrivilegeEscalation"] = true
		},
		"added capability": func(d map[string]any) {
			container(d)["securityContext"].(map[string]any)["capabilities"].(map[string]any)["add"] = []any{"SYS_ADMIN"}
		},
		"root user": func(d map[string]any) { container(d)["securityContext"].(map[string]any)["runAsUser"] = 0 },
		"image":     func(d map[string]any) { container(d)["image"] = "evil.invalid/x@sha256:" + strings.Repeat("a", 64) },
		"command":   func(d map[string]any) { container(d)["command"] = []any{"/bin/sh"} },
		"args": func(d map[string]any) {
			container(d)["args"] = append(container(d)["args"].([]any), "--server-url=https://evil.invalid")
		},
		"extra env": func(d map[string]any) {
			container(d)["env"] = append(container(d)["env"].([]any), map[string]any{"name": "X", "value": "y"})
		},
		"extra container": func(d map[string]any) {
			pod(d)["containers"] = append(pod(d)["containers"].([]any), map[string]any{"name": "sidecar", "image": "evil.invalid/x@sha256:" + strings.Repeat("b", 64)})
		},
		"extra init container": func(d map[string]any) {
			pod(d)["initContainers"] = append(pod(d)["initContainers"].([]any), map[string]any{"name": "evil", "image": "x"})
		},
		"memory limit": func(d map[string]any) {
			container(d)["resources"].(map[string]any)["limits"].(map[string]any)["memory"] = "64Gi"
		},
		"dropped resource": func(d map[string]any) {
			delete(container(d)["resources"].(map[string]any)["limits"].(map[string]any), "cpu")
		},
		"host path volume": func(d map[string]any) {
			pod(d)["volumes"] = append(pod(d)["volumes"].([]any), map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}})
		},
		"volume source": func(d map[string]any) {
			pod(d)["volumes"].([]any)[1] = map[string]any{"name": "workspace", "hostPath": map[string]any{"path": "/var"}}
		},
		"unbounded scratch": func(d map[string]any) {
			delete(pod(d)["volumes"].([]any)[1].(map[string]any)["emptyDir"].(map[string]any), "sizeLimit")
		},
		"disk secrets": func(d map[string]any) {
			delete(pod(d)["volumes"].([]any)[5].(map[string]any)["emptyDir"].(map[string]any), "medium")
		},
		"service account": func(d map[string]any) { pod(d)["serviceAccountName"] = "default" },
		"token automount": func(d map[string]any) { pod(d)["automountServiceAccountToken"] = true },
		"host network":    func(d map[string]any) { pod(d)["hostNetwork"] = true },
		"host pid":        func(d map[string]any) { pod(d)["hostPID"] = true },
		"restart policy":  func(d map[string]any) { pod(d)["restartPolicy"] = "OnFailure" },
		"node name":       func(d map[string]any) { pod(d)["nodeName"] = "control-plane" },
		"runtime class":   func(d map[string]any) { pod(d)["runtimeClassName"] = "x" },
		"pod seccomp": func(d map[string]any) {
			pod(d)["securityContext"].(map[string]any)["seccompProfile"] = map[string]any{"type": "Unconfined"}
		},
		"backoff limit":   func(d map[string]any) { d["spec"].(map[string]any)["backoffLimit"] = 6 },
		"parallelism":     func(d map[string]any) { d["spec"].(map[string]any)["parallelism"] = 2 },
		"ttl":             func(d map[string]any) { d["spec"].(map[string]any)["ttlSecondsAfterFinished"] = 0 },
		"active deadline": func(d map[string]any) { d["spec"].(map[string]any)["activeDeadlineSeconds"] = 1 },
		"suspended":       func(d map[string]any) { d["spec"].(map[string]any)["suspend"] = true },
		"indexed":         func(d map[string]any) { d["spec"].(map[string]any)["completionMode"] = "Indexed" },
		"template label": func(d map[string]any) {
			d["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)["allow-all-egress"] = "true"
		},
		"template annotation": func(d map[string]any) {
			d["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"] = map[string]any{"inject": "sidecar"}
		},
		"selector": func(d map[string]any) {
			d["spec"].(map[string]any)["selector"].(map[string]any)["matchLabels"].(map[string]any)["app"] = "x"
		},
		"owner": func(d map[string]any) {
			d["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "batch/v1", "kind": "CronJob", "name": "c", "uid": "u"}}
		},
		"fingerprint annotation": func(d map[string]any) {
			d["metadata"].(map[string]any)["annotations"].(map[string]any)[kube.AnnFingerprint] = "sha256:" + strings.Repeat("f", 64)
		},
		"attempt annotation": func(d map[string]any) {
			d["metadata"].(map[string]any)["annotations"].(map[string]any)[kube.AnnAttempt] = "att_other"
		},
		"managed label": func(d map[string]any) {
			d["metadata"].(map[string]any)["labels"].(map[string]any)[kube.LabelManagedBy] = "someone"
		},
		"unknown finalizer": func(d map[string]any) {
			d["metadata"].(map[string]any)["finalizers"] = []any{"example.com/hold"}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if err := edit(f, mut); !errors.Is(err, kube.ErrForeignWorkload) {
				t.Fatalf("mutation accepted: %v", err)
			}
		})
	}
}

// Test_verify_job_forged_annotations: a Job with the right name, the right
// annotations, and the right fingerprint annotation but another spec is foreign.
func Test_verify_job_forged_annotations(t *testing.T) {
	f := newFixture(t)
	var forged kubernetes.Job
	raw, _ := json.Marshal(f.exp.Job)
	_ = json.Unmarshal(raw, &forged)
	forged.Spec.Template.Spec.Containers[0].Image = "evil.invalid/x@sha256:" + strings.Repeat("c", 64)
	j := f.fake.PutJob(f.exp.Identity.Namespace, forged)
	if j.Metadata.Annotations[kube.AnnFingerprint] != f.rec.SpecFingerprint {
		t.Fatal("the fixture must carry the right fingerprint annotation")
	}
	if _, err := kube.VerifyJob(f.exp, j); !errors.Is(err, kube.ErrForeignWorkload) {
		t.Fatalf("forged Job accepted: %v", err)
	}
	// Name, namespace and UID mismatches.
	own := newFixture(t).created(t)
	other := own
	other.Metadata.Name = "kdm-" + strings.Repeat("0", 40)
	noUID := own
	noUID.Metadata.UID = ""
	wrongNS := own
	wrongNS.Metadata.Namespace = "elsewhere"
	for name, j := range map[string]kubernetes.Job{"name": other, "uid": noUID, "namespace": wrongNS} {
		if _, err := kube.VerifyJob(f.exp, j); !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("%s: accepted: %v", name, err)
		}
	}
	// A message names paths only, never values.
	_, err := kube.VerifyJob(f.exp, j)
	if err != nil && strings.Contains(err.Error(), "evil.invalid") {
		t.Fatal("the error leaks object values")
	}
}
