package testkit

import (
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel/engine"
)

var _ engine.Clock = (*Clock)(nil)

func TestClockAdvancesOnlyExplicitly(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := NewClock(base)

	if got := clock.Now(); !got.Equal(base) {
		t.Fatalf("Now() = %s, want %s", got, base)
	}

	first := clock.Advance(90 * time.Minute)
	wantFirst := base.Add(90 * time.Minute)
	if !first.Equal(wantFirst) {
		t.Fatalf("Advance(90m) = %s, want %s", first, wantFirst)
	}
	if got := clock.Now(); !got.Equal(wantFirst) {
		t.Fatalf("Now() after Advance = %s, want %s", got, wantFirst)
	}

	replacement := base.Add(48 * time.Hour)
	if got := clock.Set(replacement); !got.Equal(replacement) {
		t.Fatalf("Set() = %s, want %s", got, replacement)
	}
	if got := clock.Now(); !got.Equal(replacement) {
		t.Fatalf("Now() after Set = %s, want %s", got, replacement)
	}
}

func TestClockInstancesAreIndependent(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := NewClock(base)
	second := NewClock(base)

	first.Advance(time.Hour)

	if got := second.Now(); !got.Equal(base) {
		t.Fatalf("second Now() = %s, want untouched %s", got, base)
	}
}

func TestClockConcurrentAdvanceIsRaceFreeAndDeterministic(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := NewClock(base)

	const workers = 8
	const advances = 100

	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for range advances {
				clock.Advance(time.Millisecond)
				_ = clock.Now()
			}
		}()
	}
	wait.Wait()

	want := base.Add(workers * advances * time.Millisecond)
	if got := clock.Now(); !got.Equal(want) {
		t.Fatalf("Now() after concurrent advances = %s, want %s", got, want)
	}
}
