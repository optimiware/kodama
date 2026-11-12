package factory

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

// TestCleanupExitCode proves the post-run exit-code rule: a best-effort sweep
// error alone never changes a passing result, while a leak this process caused
// does. This is the regression guard for a package-level FAIL caused by another
// lane removing a foreign labelled resource between the orphan sweep's list and
// its inspect.
func TestCleanupExitCode(t *testing.T) {
	sweepErr := errors.New("orphan sweep: podman inspect failed: no such object")
	ownErr := errors.New("remove run containers: remove container: failed")

	cases := []struct {
		name    string
		code    int
		outcome CleanupOutcome
		want    int
	}{
		{name: "clean pass", code: 0, outcome: CleanupOutcome{}, want: 0},
		{name: "orphan sweep error only", code: 0, outcome: CleanupOutcome{Sweep: sweepErr}, want: 0},
		{name: "own-run leak", code: 0, outcome: CleanupOutcome{Own: ownErr}, want: 1},
		{name: "own-run leak with sweep error", code: 0, outcome: CleanupOutcome{Own: ownErr, Sweep: sweepErr}, want: 1},
		{name: "already failing stays failing", code: 1, outcome: CleanupOutcome{}, want: 1},
		{name: "failing with sweep error", code: 1, outcome: CleanupOutcome{Sweep: sweepErr}, want: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CleanupExitCode(testCase.code, testCase.outcome); got != testCase.want {
				t.Fatalf("CleanupExitCode(%d, %+v) = %d; want %d", testCase.code, testCase.outcome, got, testCase.want)
			}
		})
	}
}

// TestClassifyKindProbe proves the post-run own-cluster classification: a probe
// failure after this process created a cluster can hide a leak, a foreign
// sweep/probe error is logged, a missing kind binary without a created cluster
// is not an error, and a surviving own-pid cluster is a fatal leak.
func TestClassifyKindProbe(t *testing.T) {
	unavailable := fmt.Errorf("kind resolution: %w", kubesupport.ErrKindUnavailable)
	broken := errors.New("kind list failed")

	cases := []struct {
		name      string
		probeErr  error
		leaked    []string
		created   bool
		pid       int
		wantOwn   bool
		wantSweep bool
	}{
		{name: "clean probe", pid: 4242},
		{name: "own cluster leaked", leaked: []string{"kodama-t-4242-0123abcd"}, pid: 4242, wantOwn: true},
		{name: "probe failure after creating a cluster", probeErr: broken, created: true, pid: 4242, wantOwn: true},
		{name: "foreign probe failure", probeErr: broken, pid: 4242, wantSweep: true},
		{name: "kind unavailable without a created cluster", probeErr: unavailable, pid: 4242},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			own, sweep := ClassifyKindProbe(testCase.probeErr, testCase.leaked, testCase.created, testCase.pid)
			if (own != nil) != testCase.wantOwn {
				t.Fatalf("own error = %v; want present=%v", own, testCase.wantOwn)
			}
			if (sweep != nil) != testCase.wantSweep {
				t.Fatalf("sweep error = %v; want present=%v", sweep, testCase.wantSweep)
			}
		})
	}

	if own, _ := ClassifyKindProbe(nil, []string{"kodama-t-4242-0123abcd", "kodama-t-4242-fedcba98"}, false, 4242); own == nil ||
		!strings.Contains(own.Error(), "2 cluster(s) owned by pid 4242") {
		t.Fatalf("leak error = %v; want both owned cluster names and the pid", own)
	}
}

// TestOwnClusterNames proves the own-pid cluster probe selects exactly this
// process's generated names and never a foreign, protected or malformed one.
func TestOwnClusterNames(t *testing.T) {
	names := []string{
		"kodama-t-4242-0123abcd",
		"kodama-t-4242-fedcba98",
		"kodama-t-4243-0123abcd", // another live lane
		"kodama-probe",           // protected cluster
		"kodama-t-4242-0123ABCD", // uppercase is outside the naming contract
		"kodama-t-4242-0123abc",  // short suffix
		"other-4242-0123abcd",    // foreign prefix
		"kodama-t--0123abcd",     // missing pid
	}
	got := OwnClusterNames(names, 4242)
	want := []string{"kodama-t-4242-0123abcd", "kodama-t-4242-fedcba98"}
	if len(got) != len(want) {
		t.Fatalf("OwnClusterNames = %v; want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("OwnClusterNames = %v; want %v", got, want)
		}
	}
	if names := OwnClusterNames(names, 0); len(names) != 0 {
		t.Fatalf("OwnClusterNames with pid 0 = %v; want none", names)
	}
}
