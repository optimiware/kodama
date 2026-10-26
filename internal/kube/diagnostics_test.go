package kube

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/diag"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// A refused dispatch used to log only `unavailable`. The classified cause (what was being done, with which
// API operation and status) is now part of the safe description; a transport cause and any peer text stay out.
func TestExecutorFailuresDescribeTheirClassifiedCause(t *testing.T) {
	api := &kubernetes.Error{Op: "get job", Kind: kubernetes.ErrUnavailable, Status: 503, Cause: errors.New("dial tcp 10.0.0.9:6443: token=SECRET")}
	err := fmt.Errorf("execution: dispatch: %w", wrapCause(ErrUnavailable, api, "reading the Job"))
	got := diag.Describe(err)
	for _, want := range []string{"kube: dependency unavailable: reading the Job", "kubernetes: get job", "HTTP 503"} {
		if !strings.Contains(got, want) {
			t.Fatalf("description %q lacks %q", got, want)
		}
	}
	for _, leak := range []string{"SECRET", "10.0.0.9", "dial tcp"} {
		if strings.Contains(got, leak) {
			t.Fatalf("description %q leaks %q", got, leak)
		}
	}
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, kubernetes.ErrUnavailable) {
		t.Fatal("classification by errors.Is was lost")
	}
}
