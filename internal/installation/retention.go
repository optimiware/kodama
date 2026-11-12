package installation

// Retention rules from S10 §5. These are pure value rules over explicit
// timestamps: no clock is read here and no record is deleted.

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// RetentionClass names one retained record family.
type RetentionClass string

const (
	// RetentionLogs is the retained operational log family.
	RetentionLogs RetentionClass = "logs"
	// RetentionEvidence is the ordinary execution evidence family.
	RetentionEvidence RetentionClass = "evidence"
	// RetentionAudit is the audit metadata family.
	RetentionAudit RetentionClass = "audit"
	// RetentionReceipts is the operation receipt family.
	RetentionReceipts RetentionClass = "receipts"
	// RetentionRawContent is raw model/tool content.
	RetentionRawContent RetentionClass = "raw_content"
)

// RetentionClasses returns every retained family in declaration order.
func RetentionClasses() []RetentionClass {
	return []RetentionClass{
		RetentionLogs,
		RetentionEvidence,
		RetentionAudit,
		RetentionReceipts,
		RetentionRawContent,
	}
}

// RetentionPolicy is the installation's retention choice. Raw model/tool
// content is disabled unless explicitly opted in.
type RetentionPolicy struct {
	RawContentOptIn bool
}

// RetentionWindow is the declared lifetime of one family.
type RetentionWindow struct {
	Class                RetentionClass
	Duration             time.Duration
	UntilProjectDeletion bool
	Disabled             bool
}

// Window returns the declared window for class.
func (policy RetentionPolicy) Window(class RetentionClass) (RetentionWindow, error) {
	switch class {
	case RetentionLogs:
		return RetentionWindow{Class: class, Duration: 30 * 24 * time.Hour}, nil
	case RetentionEvidence:
		return RetentionWindow{Class: class, Duration: 90 * 24 * time.Hour}, nil
	case RetentionAudit:
		return RetentionWindow{Class: class, Duration: 365 * 24 * time.Hour}, nil
	case RetentionReceipts:
		return RetentionWindow{Class: class, UntilProjectDeletion: true}, nil
	case RetentionRawContent:
		if !policy.RawContentOptIn {
			return RetentionWindow{Class: class, Disabled: true}, nil
		}
		return RetentionWindow{Class: class, Duration: 7 * 24 * time.Hour}, nil
	default:
		return RetentionWindow{}, fault.New(fault.InvalidInput, fmt.Sprintf("unknown retention class %q", class))
	}
}

// DeletionDecision is one retention decision with its public reason.
type DeletionDecision struct {
	Delete bool
	Reason string
}

// MayDelete decides whether one item may be deleted. An item is deletable only
// when it is strictly older than its window; exactly at the boundary it is
// kept. Active references, a disabled family, receipt retention until project
// deletion and a creation time after now all keep the item.
func (policy RetentionPolicy) MayDelete(class RetentionClass, created, now time.Time, activeReferences bool) (DeletionDecision, error) {
	window, err := policy.Window(class)
	if err != nil {
		return DeletionDecision{}, err
	}
	if window.Disabled {
		return DeletionDecision{Reason: "retention is disabled for this family"}, nil
	}
	if window.UntilProjectDeletion {
		return DeletionDecision{Reason: "retention continues until project deletion"}, nil
	}
	if created.After(now) {
		return DeletionDecision{Reason: "creation time is after the current time"}, nil
	}
	if activeReferences {
		return DeletionDecision{Reason: "active references extend retention"}, nil
	}
	if now.Sub(created) <= window.Duration {
		return DeletionDecision{Reason: "item is within its retention window"}, nil
	}
	return DeletionDecision{Delete: true, Reason: "item is older than its retention window"}, nil
}
