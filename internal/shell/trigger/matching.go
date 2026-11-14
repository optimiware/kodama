package trigger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
)

// ReportOperationGitRefUpdate is the only resource operation a push report may
// cite.
const ReportOperationGitRefUpdate = "git.ref.update"

// ReportTypeGitRefUpdated is the binding-declared report type of an exact Git
// ref update.
const ReportTypeGitRefUpdated = event.ReportType("git.ref.updated")

// Report is one parsed verified git.ref.updated payload. Commit is the
// candidate commit and PreviousCommit is the expected base; Deleted reports a
// ref deletion (or an empty candidate), which starts no pipeline.
type Report struct {
	Operation      string              `json:"operation"`
	Resource       resource.ResourceID `json:"resource"`
	Ref            string              `json:"ref"`
	Commit         string              `json:"commit"`
	PreviousCommit string              `json:"previous_commit"`
	Deleted        bool                `json:"deleted"`
}

// reportPayload is the exact durable JSON shape of one Git ref update report.
type reportPayload struct {
	Operation    string `json:"operation"`
	Resource     string `json:"resource"`
	Ref          string `json:"ref"`
	ExpectedBase string `json:"expected_base"`
	Candidate    string `json:"candidate"`
	Deleted      bool   `json:"deleted"`
}

// ParseReport parses and validates one verified git.ref.updated payload.
// Malformed JSON, a missing ref, an operation other than git.ref.update or a
// resource that disagrees with the envelope subject is refused with
// fault.InvalidInput. A deletion flag or an empty candidate marks the report as
// a deletion.
func ParseReport(envelope event.Envelope) (Report, error) {
	var payload reportPayload
	if err := json.Unmarshal(envelope.Data, &payload); err != nil {
		return Report{}, fault.Wrap(fault.InvalidInput, "push report data is not valid JSON", err)
	}
	if payload.Operation != ReportOperationGitRefUpdate {
		return Report{}, fault.New(fault.InvalidInput, "push report operation is not git.ref.update")
	}
	if payload.Ref == "" {
		return Report{}, fault.New(fault.InvalidInput, "push report ref is empty")
	}
	if payload.Resource == "" {
		return Report{}, fault.New(fault.InvalidInput, "push report resource is empty")
	}
	if payload.Resource != envelope.SubjectID {
		return Report{}, fault.New(fault.InvalidInput, "push report resource disagrees with the event subject")
	}
	return Report{
		Operation:      payload.Operation,
		Resource:       resource.ResourceID(payload.Resource),
		Ref:            payload.Ref,
		Commit:         payload.Candidate,
		PreviousCommit: payload.ExpectedBase,
		Deleted:        payload.Deleted || payload.Candidate == "",
	}, nil
}

// acceptedReport returns the parsed report of an accepted push event. Only a
// verified resource report of type git.ref.updated from a service source whose
// verified pusher was no execution is accepted. Deletions and
// execution-originated reports are never accepted.
//
// The verified-report ingress always sets CausationID to the cited verification
// and sets CorrelationID to the execution of the verified actor, read from the
// verification record and never from the report body. A non-empty correlation
// therefore marks an execution-originated push, including one reported after
// the execution ended; a human push carries a causation but no correlation.
func acceptedReport(envelope event.Envelope) (Report, bool) {
	if envelope.Type != event.TypeVerifiedReport {
		return Report{}, false
	}
	if envelope.ReportType != ReportTypeGitRefUpdated {
		return Report{}, false
	}
	if envelope.Source.Kind != event.SourceService {
		return Report{}, false
	}
	if envelope.CorrelationID != "" {
		return Report{}, false
	}
	report, err := ParseReport(envelope)
	if err != nil || report.Deleted {
		return Report{}, false
	}
	return report, true
}

// definitionPushTriggers returns the persisted on.push projection supplied by
// the candidate reader. The read definition carries the projection
// (pipelines.push_triggers) so adapters never parse YAML; a reader that still
// populates only the single parsed trigger keeps working through the fallback.
func definitionPushTriggers(definition pipeline.Definition) []pipeline.PushTrigger {
	if len(definition.PushTriggers) > 0 {
		return definition.PushTriggers
	}
	if definition.Push == nil {
		return nil
	}
	return []pipeline.PushTrigger{*definition.Push}
}

// matchingTrigger returns the matching on.push trigger of one current candidate
// that the accepted report targets: the binding scope, resource and reporting
// service must match, the binding must declare the report type, and one
// persisted trigger must declare the same repository, a reporting service and
// an exact ref or trailing-slash ref prefix covering report.Ref. The matched
// trigger supplies the automatic-start service, so the selection never depends
// on the single parsed Push field.
func matchingTrigger(candidate Candidate, envelope event.Envelope, report Report) (pipeline.PushTrigger, bool) {
	binding := candidate.Binding
	if !reportingBinding(binding, envelope, report) {
		return pipeline.PushTrigger{}, false
	}
	if !binding.DeclaresReport(string(ReportTypeGitRefUpdated)) {
		return pipeline.PushTrigger{}, false
	}
	for _, trigger := range definitionPushTriggers(candidate.Definition) {
		if trigger.Repository != report.Resource {
			continue
		}
		if trigger.Service == "" {
			continue
		}
		if slices.ContainsFunc(trigger.Refs, func(declared string) bool {
			return pushRefMatches(declared, report.Ref)
		}) {
			return trigger, true
		}
	}
	return pipeline.PushTrigger{}, false
}

// pushRefMatches reports whether a declared on.push ref entry matches the
// reported ref: a trailing-slash entry is a prefix requiring a non-empty,
// non-prefix remainder; any other entry must match the full ref verbatim.
func pushRefMatches(declared, reported string) bool {
	if strings.HasSuffix(declared, "/") {
		return strings.HasPrefix(reported, declared) &&
			len(reported) > len(declared) &&
			!strings.HasSuffix(reported, "/")
	}
	return declared == reported
}

// reportingBinding reports whether a candidate's binding is the one the push
// was reported through: the event scope, the reported resource and the
// reporting service as its enforcing service. A repository may have bindings
// of several enforcing services, and the reader returns a candidate per
// binding; only the reporting binding may select, resume, authorize or lock a
// start for the event.
func reportingBinding(binding resource.Binding, envelope event.Envelope, report Report) bool {
	return binding.Scope == envelope.Scope &&
		binding.ResourceID == report.Resource &&
		binding.EnforcingService == service.ServiceID(envelope.Source.PrincipalID)
}

// pushInputValue returns the exact report value for one declared push input.
// Only repository, ref, commit and previous_commit can be supplied; any other
// declared key is never invented.
func pushInputValue(key id.InputKey, report Report) (string, bool) {
	switch string(key) {
	case "repository":
		return string(report.Resource), true
	case "ref":
		return report.Ref, true
	case "commit":
		return report.Commit, true
	case "previous_commit":
		return report.PreviousCommit, true
	default:
		return "", false
	}
}

// declaredInputs decodes the exact authored YAML and returns the declared input
// keys in declaration order. Decoding the persisted bytes is the only source of
// the declared input set; the processor never guesses from a current edit.
func declaredInputs(yaml []byte) ([]id.InputKey, error) {
	parsed, err := pipeline.Decode(yaml, pipeline.DefaultLimits)
	if err != nil {
		return nil, err
	}
	return append([]id.InputKey(nil), parsed.Inputs...), nil
}

// selectionInputs fills only the declared push inputs from the report.
func selectionInputs(keys []id.InputKey, report Report) map[id.InputKey]string {
	inputs := make(map[id.InputKey]string, len(keys))
	for _, key := range keys {
		value, ok := pushInputValue(key, report)
		if !ok {
			continue
		}
		inputs[key] = value
	}
	if len(inputs) == 0 {
		return nil
	}
	return inputs
}

// eventFingerprint is the deterministic content identity of one accepted event.
// It covers the event identity, type, report type, subject, source and exact
// data bytes, so two deliveries of the same event share one fingerprint.
func eventFingerprint(envelope event.Envelope) [32]byte {
	hasher := sha256.New()
	for _, part := range []string{
		string(envelope.ID),
		string(envelope.Type),
		string(envelope.ReportType),
		envelope.SubjectID,
		string(envelope.Source.Kind),
		string(envelope.Source.PrincipalID),
	} {
		hasher.Write([]byte(part))
		hasher.Write([]byte{0})
	}
	hasher.Write(envelope.Data)
	var fingerprint [32]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return fingerprint
}

// contentHash renders an event fingerprint as the common receipt content hash.
func contentHash(fingerprint [32]byte) string {
	return "sha256:" + hex.EncodeToString(fingerprint[:])
}
