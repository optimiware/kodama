//go:build component

package kubernetesconformance

import (
	"bytes"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// TestPodLogPaginationOverTheCombinedLog proves the real pod log endpoint, with
// no stream parameter (stock clusters reject it), the driver's prefix-discard
// offsets and the adapter's continuation cursor deliver the interleaved log
// completely and exactly once.
func TestPodLogPaginationOverTheCombinedLog(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-5")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k5", "action-k5", "workload-k5", "idem-k5")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{
			`printf '0123456789'`,
			`printf 'abcdefghij' >&2`,
		}, nil), nil, "tok-k5",
	))
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	pod := onePod(t, ctx, fx.Client, identity)

	// Read the whole combined log once. The cluster may interleave stdout and
	// stderr in either order, so pagination is compared with this exact observed
	// sequence rather than an assumed written order.
	whole, err := fx.Client.ReadPodLogs(ctx, testNamespace, pod.Name, runtimekube.PodLogRequest{
		Stream: runtimekube.LogStreamCombined,
		Offset: 0,
		Limit:  1 << 20,
	})
	if err != nil {
		t.Fatalf("ReadPodLogs whole log: %v", err)
	}
	if whole.Truncated {
		t.Fatalf("whole log = %+v; want the complete log", whole)
	}
	if !strings.Contains(string(whole.Data), "0123456789") || !strings.Contains(string(whole.Data), "abcdefghij") || len(whole.Data) != 20 {
		t.Fatalf("whole combined log = %q; want both ten-byte lines exactly once", whole.Data)
	}

	first, err := fx.Client.ReadPodLogs(ctx, testNamespace, pod.Name, runtimekube.PodLogRequest{
		Stream: runtimekube.LogStreamCombined,
		Offset: 0,
		Limit:  4,
	})
	if err != nil {
		t.Fatalf("ReadPodLogs first page: %v", err)
	}
	if !bytes.Equal(first.Data, whole.Data[:4]) || !first.Truncated || first.NextOffset != 4 {
		t.Fatalf("first page = %+v (%q); want %q at 4 truncated", first, first.Data, whole.Data[:4])
	}
	rest, err := fx.Client.ReadPodLogs(ctx, testNamespace, pod.Name, runtimekube.PodLogRequest{
		Stream: runtimekube.LogStreamCombined,
		Offset: 4,
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("ReadPodLogs rest: %v", err)
	}
	if !bytes.Equal(rest.Data, whole.Data[4:]) || rest.Truncated {
		t.Fatalf("rest = %+v (%q); want %q not truncated", rest, rest.Data, whole.Data[4:])
	}

	// A small limit must still deliver the complete log through the cursor.
	stdout, stderr := readAllLogs(t, ctx, fx.Adapter, identity, 3)
	if !bytes.Equal(stdout, whole.Data) || len(stderr) != 0 {
		t.Fatalf("adapter log = %q (stderr %q); want the complete combined log %q", stdout, stderr, whole.Data)
	}
}
