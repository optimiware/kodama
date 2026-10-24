package chatops

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// PayloadSchemaVersion is the version of the generic webhook payload. Receivers
// must ignore unknown members; a member is only ever added within a version,
// never renamed, retyped or removed. A breaking change is a new version that a
// destination would have to opt into.
const PayloadSchemaVersion = 1

// Bounds of the projection.
const (
	// MaxPayloadBytes bounds the whole encoded payload.
	MaxPayloadBytes = 8 << 10
	// MaxSummaryBytes bounds the human-readable summary.
	MaxSummaryBytes = 256
	// MaxAttributes and MaxAttributeBytes bound the event-specific attributes.
	MaxAttributes     = 12
	MaxAttributeBytes = 128
)

// SupportedEvents is the explicit allow-list a destination may select from:
// work transitions, gate requests/decisions/invalidations, run state changes
// and deployment state changes. Nothing else is ever forwarded.
var SupportedEvents = []string{
	eventsv1.TypeWorkItemTransitioned,
	eventsv1.TypeGateRequested,
	eventsv1.TypeGateDecided,
	eventsv1.TypeGateInvalidated,
	eventsv1.TypeRunStateChanged,
	eventsv1.TypeDeploymentStateChanged,
}

// IsSupportedEvent reports whether the type is on the allow-list.
func IsSupportedEvent(eventType string) bool { return slices.Contains(SupportedEvents, eventType) }

// Payload is the generic JSON document POSTed to a destination. It is a bounded
// projection of one catalog event, never the event itself: it carries
// identities, versions, a short summary and a few classified attributes, and
// never source content, transcripts, credentials or free-form event data. The
// URL is a current-record link: possessing it grants nothing (the target route
// authenticates and authorizes whoever follows it).
type Payload struct {
	SchemaVersion  int               `json:"schemaVersion"`
	NotificationID string            `json:"notificationId"`
	Event          PayloadEvent      `json:"event"`
	TenantID       string            `json:"tenantId"`
	ProjectID      string            `json:"projectId"`
	Resource       PayloadResource   `json:"resource"`
	Summary        string            `json:"summary"`
	Attributes     map[string]string `json:"attributes,omitempty"`
	URL            string            `json:"url"`
}

// PayloadEvent identifies the source event.
type PayloadEvent struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	OccurredAt    string `json:"occurredAt"`
	CorrelationID string `json:"correlationId"`
}

// PayloadResource is the resource the event observed, at the observed version.
type PayloadResource struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// Encode returns the canonical bytes (struct order, sorted attribute keys) and
// checks the bound.
func (p Payload) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("chatops: encode payload: %w", err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) > MaxPayloadBytes {
		return nil, invalidf("the projected payload exceeds %d bytes", MaxPayloadBytes)
	}
	return b, nil
}

// PayloadDigest identifies payload content for the immutability check.
func PayloadDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Project builds the payload of one event for one delivery. The notification
// ID is the delivery identity. An event type outside SupportedEvents, or a
// payload that does not decode as its catalog type, is an error: the caller
// classifies it as permanent (the same event can never project differently).
func Project(env eventsv1.Envelope, notification DeliveryID, links LinkBuilder) (Payload, error) {
	if !IsSupportedEvent(env.Type) {
		return Payload{}, invalidf("event type %q is not forwarded", env.Type)
	}
	summary, attrs, link, err := summarize(env)
	if err != nil {
		return Payload{}, err
	}
	url, err := links.URL(domain.Scope{TenantID: env.TenantID, ProjectID: env.ProjectID}, link.kind, link.id)
	if err != nil {
		return Payload{}, err
	}
	if len(attrs) > MaxAttributes {
		return Payload{}, invalidf("too many attributes")
	}
	return Payload{
		SchemaVersion:  PayloadSchemaVersion,
		NotificationID: string(notification),
		Event: PayloadEvent{ID: env.ID, Type: env.Type, OccurredAt: env.OccurredAt.UTC().Format(time.RFC3339Nano),
			CorrelationID: bounded(env.CorrelationID, 128)},
		TenantID:   string(env.TenantID),
		ProjectID:  string(env.ProjectID),
		Resource:   PayloadResource{Type: env.Resource.Type, ID: env.Resource.ID, Version: env.Resource.Version},
		Summary:    bounded(summary, MaxSummaryBytes),
		Attributes: attrs,
		URL:        url,
	}, nil
}

type linkTarget struct{ kind, id string }

func summarize(env eventsv1.Envelope) (summary string, attrs map[string]string, link linkTarget, err error) {
	res := env.Resource
	attrs = map[string]string{}
	set := func(k, v string) {
		if v != "" {
			attrs[k] = bounded(v, MaxAttributeBytes)
		}
	}
	switch env.Type {
	case eventsv1.TypeWorkItemTransitioned:
		var d eventsv1.WorkItemTransitioned
		if err = json.Unmarshal(env.Data, &d); err != nil {
			return "", nil, link, invalidf("work item payload is malformed")
		}
		set("from", d.From)
		set("to", d.To)
		set("transitionId", d.TransitionID)
		return fmt.Sprintf("Work item %s moved from %s to %s", res.ID, bounded(d.From, 32), bounded(d.To, 32)), attrs, linkTarget{"work_item", res.ID}, nil
	case eventsv1.TypeGateRequested:
		var d eventsv1.GateRequested
		if err = json.Unmarshal(env.Data, &d); err != nil {
			return "", nil, link, invalidf("gate payload is malformed")
		}
		set("subjectDigest", d.SubjectDigest)
		set("contentRevision", strconv.FormatInt(d.ContentRevision, 10))
		set("reviewerScope", d.ReviewerScope)
		set("deadline", d.Deadline)
		return fmt.Sprintf("Review requested: gate %s awaits a decision", res.ID), attrs, linkTarget{"gate", res.ID}, nil
	case eventsv1.TypeGateDecided:
		var d eventsv1.GateDecided
		if err = json.Unmarshal(env.Data, &d); err != nil {
			return "", nil, link, invalidf("gate payload is malformed")
		}
		set("decision", d.Decision)
		set("state", d.State)
		set("subjectDigest", d.SubjectDigest)
		set("decidedByType", string(d.DecidedBy.Type))
		return fmt.Sprintf("Gate %s was decided: %s", res.ID, bounded(d.Decision, 32)), attrs, linkTarget{"gate", res.ID}, nil
	case eventsv1.TypeGateInvalidated:
		var d eventsv1.GateInvalidated
		if err = json.Unmarshal(env.Data, &d); err != nil {
			return "", nil, link, invalidf("gate payload is malformed")
		}
		set("subjectDigest", d.SubjectDigest)
		set("reason", d.Reason)
		if d.SupersedingCommit != nil {
			set("supersedingCommit", *d.SupersedingCommit)
		}
		if d.SupersedingRevision != nil {
			set("supersedingRevision", strconv.FormatInt(*d.SupersedingRevision, 10))
		}
		return fmt.Sprintf("Gate %s was invalidated: %s", res.ID, bounded(d.Reason, 64)), attrs, linkTarget{"gate", res.ID}, nil
	case eventsv1.TypeRunStateChanged:
		var d eventsv1.RunStateChanged
		if err = json.Unmarshal(env.Data, &d); err != nil {
			return "", nil, link, invalidf("run payload is malformed")
		}
		set("from", d.From)
		set("to", d.To)
		set("reason", d.Reason)
		return fmt.Sprintf("Run %s changed from %s to %s", res.ID, bounded(d.From, 32), bounded(d.To, 32)), attrs, linkTarget{"run", res.ID}, nil
	case eventsv1.TypeDeploymentStateChanged:
		var d eventsv1.DeploymentStateChanged
		if err = json.Unmarshal(env.Data, &d); err != nil {
			return "", nil, link, invalidf("deployment payload is malformed")
		}
		set("environmentId", d.EnvironmentID)
		set("from", d.From)
		set("to", d.To)
		if d.SourceCommit != nil {
			set("sourceCommit", *d.SourceCommit)
		}
		if d.ImageDigest != nil {
			set("imageDigest", *d.ImageDigest)
		}
		return fmt.Sprintf("Deployment %s changed from %s to %s", res.ID, bounded(d.From, 32), bounded(d.To, 32)), attrs, linkTarget{"deployment", res.ID}, nil
	}
	return "", nil, link, invalidf("event type %q is not forwarded", env.Type)
}

// bounded keeps printable text only and truncates at a rune boundary.
func bounded(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsPrint(r) {
			r = ' '
		}
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
