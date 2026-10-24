package kube_test

import (
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func parseRecord(t *testing.T, s kubernetes.Secret) map[string]any {
	t.Helper()
	var m map[string]any
	jsonUnmarshal(t, []byte(s.Metadata.Annotations["kodama.optimi.com/workload"]), &m)
	return m
}

type kubetestFault = kubetest.Fault
