package installation

// Retention tests. They pin the lifetimes from S10 §5, the exact
// expiry boundary, active-reference protection, receipt retention until project
// deletion and raw-content opt-in, using explicit timestamps only.

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

var retentionNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func TestRetentionClassesFrozen(t *testing.T) {
	want := []RetentionClass{
		RetentionLogs,
		RetentionEvidence,
		RetentionAudit,
		RetentionReceipts,
		RetentionRawContent,
	}
	got := RetentionClasses()
	if len(got) != len(want) {
		t.Fatalf("RetentionClasses() = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("RetentionClasses() = %v, want %v", got, want)
		}
	}
}

func TestRetentionWindows(t *testing.T) {
	policy := RetentionPolicy{}
	for _, testCase := range []struct {
		class    RetentionClass
		duration time.Duration
	}{
		{class: RetentionLogs, duration: 30 * 24 * time.Hour},
		{class: RetentionEvidence, duration: 90 * 24 * time.Hour},
		{class: RetentionAudit, duration: 365 * 24 * time.Hour},
	} {
		t.Run(string(testCase.class), func(t *testing.T) {
			window, err := policy.Window(testCase.class)
			if err != nil {
				t.Fatalf("Window: %v", err)
			}
			if window.Class != testCase.class {
				t.Errorf("class = %q", window.Class)
			}
			if window.Duration != testCase.duration {
				t.Errorf("duration = %v, want %v", window.Duration, testCase.duration)
			}
			if window.UntilProjectDeletion || window.Disabled {
				t.Errorf("window = %+v, want a plain TTL", window)
			}
		})
	}

	receipts, err := policy.Window(RetentionReceipts)
	if err != nil {
		t.Fatalf("Window(receipts): %v", err)
	}
	if receipts.Duration != 0 {
		t.Errorf("receipts duration = %v, want no TTL", receipts.Duration)
	}
	if !receipts.UntilProjectDeletion {
		t.Error("receipts must remain until project deletion")
	}
	if receipts.Disabled {
		t.Error("receipts must not be disabled")
	}

	disabled, err := policy.Window(RetentionRawContent)
	if err != nil {
		t.Fatalf("Window(raw content): %v", err)
	}
	if !disabled.Disabled {
		t.Error("raw content must be disabled by default")
	}

	optedIn := RetentionPolicy{RawContentOptIn: true}
	raw, err := optedIn.Window(RetentionRawContent)
	if err != nil {
		t.Fatalf("Window(raw content opt-in): %v", err)
	}
	if raw.Disabled {
		t.Error("opted-in raw content must be enabled")
	}
	if raw.Duration != 7*24*time.Hour {
		t.Errorf("raw content duration = %v, want 7 days", raw.Duration)
	}
}

func TestRetentionBoundaries(t *testing.T) {
	policy := RetentionPolicy{RawContentOptIn: true}
	for _, testCase := range []struct {
		class    RetentionClass
		duration time.Duration
	}{
		{class: RetentionLogs, duration: 30 * 24 * time.Hour},
		{class: RetentionEvidence, duration: 90 * 24 * time.Hour},
		{class: RetentionAudit, duration: 365 * 24 * time.Hour},
		{class: RetentionRawContent, duration: 7 * 24 * time.Hour},
	} {
		t.Run(string(testCase.class), func(t *testing.T) {
			for _, boundary := range []struct {
				name   string
				age    time.Duration
				delete bool
			}{
				{name: "fresh", age: 0, delete: false},
				{name: "one second before boundary", age: testCase.duration - time.Second, delete: false},
				{name: "exactly at boundary", age: testCase.duration, delete: false},
				{name: "one second after boundary", age: testCase.duration + time.Second, delete: true},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					decision, err := policy.MayDelete(testCase.class, retentionNow.Add(-boundary.age), retentionNow, false)
					if err != nil {
						t.Fatalf("MayDelete: %v", err)
					}
					if decision.Delete != boundary.delete {
						t.Fatalf("delete = %v, want %v", decision.Delete, boundary.delete)
					}
					if !decision.Delete && decision.Reason == "" {
						t.Error("a kept item must carry a reason")
					}
				})
			}
		})
	}
}

func TestRetentionReceiptsUntilProjectDeletion(t *testing.T) {
	policy := RetentionPolicy{RawContentOptIn: true}
	for _, age := range []time.Duration{
		365 * 24 * time.Hour,
		10 * 365 * 24 * time.Hour,
	} {
		decision, err := policy.MayDelete(RetentionReceipts, retentionNow.Add(-age), retentionNow, false)
		if err != nil {
			t.Fatalf("MayDelete(receipts): %v", err)
		}
		if decision.Delete {
			t.Fatalf("receipt aged %v was deleted by a TTL", age)
		}
		if decision.Reason == "" {
			t.Error("kept receipt must carry a reason")
		}
	}
}

func TestRetentionRawContentDisabled(t *testing.T) {
	policy := RetentionPolicy{}
	decision, err := policy.MayDelete(RetentionRawContent, retentionNow.Add(-100*24*time.Hour), retentionNow, false)
	if err != nil {
		t.Fatalf("MayDelete(raw content): %v", err)
	}
	if decision.Delete {
		t.Fatal("disabled raw content must never be deleted")
	}
	if decision.Reason == "" {
		t.Error("kept raw content must carry a reason")
	}
}

func TestRetentionActiveReferencesExtend(t *testing.T) {
	policy := RetentionPolicy{RawContentOptIn: true}
	for _, class := range []RetentionClass{
		RetentionLogs,
		RetentionEvidence,
		RetentionAudit,
		RetentionRawContent,
	} {
		t.Run(string(class), func(t *testing.T) {
			window, err := policy.Window(class)
			if err != nil {
				t.Fatalf("Window: %v", err)
			}
			decision, err := policy.MayDelete(class, retentionNow.Add(-(window.Duration + 100*24*time.Hour)), retentionNow, true)
			if err != nil {
				t.Fatalf("MayDelete: %v", err)
			}
			if decision.Delete {
				t.Fatal("an actively referenced item was deleted")
			}
			if decision.Reason == "" {
				t.Error("kept item must carry a reason")
			}
		})
	}
}

func TestRetentionFutureCreationIsKept(t *testing.T) {
	policy := RetentionPolicy{}
	decision, err := policy.MayDelete(RetentionLogs, retentionNow.Add(time.Hour), retentionNow, false)
	if err != nil {
		t.Fatalf("MayDelete: %v", err)
	}
	if decision.Delete {
		t.Fatal("an item created after now was deleted")
	}
}

func TestRetentionUnknownClassRefused(t *testing.T) {
	policy := RetentionPolicy{}
	if _, err := policy.Window(RetentionClass("forever")); err == nil {
		t.Fatal("Window accepted an unknown class")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if _, err := policy.MayDelete(RetentionClass("forever"), retentionNow, retentionNow, false); err == nil {
		t.Fatal("MayDelete accepted an unknown class")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
}
