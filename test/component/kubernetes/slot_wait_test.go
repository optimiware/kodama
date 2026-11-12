package kubernetesconformance

import (
	"testing"
	"time"
)

// TestSlotWaitBound pins the bounded queue wait derived from the package
// -timeout. It is hermetic: it starts no cluster and needs no infrastructure.
func TestSlotWaitBound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		outer time.Duration
		want  time.Duration
	}{
		{name: "absent or unbounded uses the ceiling", outer: 0, want: 15 * time.Minute},
		{name: "short outer bound uses the floor", outer: 6 * time.Minute, want: 5 * time.Minute},
		{name: "half of a medium outer bound", outer: 20 * time.Minute, want: 10 * time.Minute},
		{name: "medium-long outer bound", outer: 25 * time.Minute, want: 12*time.Minute + 30*time.Second},
		{name: "suite example timeout", outer: 40 * time.Minute, want: 15 * time.Minute},
		{name: "long outer bound is capped", outer: 60 * time.Minute, want: 15 * time.Minute},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := slotWaitBound(testCase.outer); got != testCase.want {
				t.Fatalf("slotWaitBound(%s) = %s; want %s", testCase.outer, got, testCase.want)
			}
		})
	}
}
