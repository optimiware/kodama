package eventsv1

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// TypeRunResourceStateChanged is the observation of one run resource's phase
// transition (docs/contracts/run-resources.md). Its aggregate is the run
// resource (envelope resource type "run_resource"); the project is required.
// The event is a safe-reference observation: it never authorizes a workload
// action and carries no provider UID, endpoint, log or credential.
const TypeRunResourceStateChanged = "run_resource.state_changed"

// ResourceTypeRunResource is the envelope resource type of the event.
const ResourceTypeRunResource = "run_resource"

// Run-resource kinds.
const (
	RunResourceKindNetwork     = "network"
	RunResourceKindWorkspace   = "workspace"
	RunResourceKindPreparation = "preparation"
	RunResourceKindService     = "service"
)

// Run-resource phases (run-resources.md, "Phases and evidence"). From may
// additionally be PhaseNone, the creation edge before the resource has any
// observed phase; To is always one of the six phases.
const (
	RunResourcePhaseNone    = "none"
	RunResourcePhasePending = "pending"
	RunResourcePhaseReady   = "ready"
	RunResourcePhaseFailed  = "failed"
	RunResourcePhaseStopped = "stopped"
	RunResourcePhaseAbsent  = "absent"
	RunResourcePhaseUnknown = "unknown"
)

var (
	runResourceKinds = []string{RunResourceKindNetwork, RunResourceKindWorkspace, RunResourceKindPreparation, RunResourceKindService}
	runResourcePhase = []string{RunResourcePhasePending, RunResourcePhaseReady, RunResourcePhaseFailed, RunResourcePhaseStopped, RunResourcePhaseAbsent, RunResourcePhaseUnknown}
)

// RunResourceStateChanged is the payload of run_resource.state_changed. All
// members are required, including an explicit uidBound (true or false): the
// event says whether a provider identity is bound, never the identity itself.
type RunResourceStateChanged struct {
	RunID           string `json:"runId"`
	Kind            string `json:"kind"`
	Key             string `json:"key"`
	From            string `json:"from"`
	To              string `json:"to"`
	Reason          string `json:"reason"`
	SpecFingerprint string `json:"specFingerprint"`
	UIDBound        bool   `json:"uidBound"`
}

func init() {
	register[RunResourceStateChanged](TypeRunResourceStateChanged, ResourceTypeRunResource, true)
	envelopeChecks[TypeRunResourceStateChanged] = checkRunResourceEnvelope
}

// CheckRunResourceKey reports whether key is the canonical key of kind: the
// fixed keys "network", "workspace" and "preparation", or "service.NAME" for a
// service.
func CheckRunResourceKey(kind, key string) error {
	if err := oneOf("kind", kind, runResourceKinds...); err != nil {
		return err
	}
	if err := matchPattern("key", "RunResourceKey", key); err != nil {
		return err
	}
	if kind == RunResourceKindService {
		if !strings.HasPrefix(key, "service.") {
			return invalidf("key %q is not a service key for kind service", truncate(key, 64))
		}
		return nil
	}
	if key != kind {
		return invalidf("key %q does not match kind %q", truncate(key, 64), kind)
	}
	return nil
}

// RunResourceID is the audit/event resource ID of one run resource:
//
//	"rr-" + first 40 lowercase hex characters of SHA-256 over the
//	length-prefixed UTF-8 tuple (tenantId, projectId, runId, key)
//
// where each element is preceded by its byte length as an unsigned 4-byte
// big-endian integer. The result satisfies the ID domain (43 characters), is
// unambiguous across the tuple boundaries (unlike a separator join, which
// aliases for example tenant "t_a" project "p" with tenant "t" project "a_p")
// and reveals nothing about its inputs. The key must be canonical
// (CheckRunResourceKey); identifiers follow the domain rules. The framing is the
// repository-wide one of the idempotency and cursor encodings.
func RunResourceID(tenant domain.TenantID, project domain.ProjectID, run domain.RunID, key string) (string, error) {
	if err := tenant.Validate(); err != nil {
		return "", invalidf("tenantId: %v", err)
	}
	if err := project.Validate(); err != nil {
		return "", invalidf("projectId: %v", err)
	}
	if err := run.Validate(); err != nil {
		return "", invalidf("runId: %v", err)
	}
	if err := matchPattern("key", "RunResourceKey", key); err != nil {
		return "", err
	}
	var b []byte
	for _, part := range []string{string(tenant), string(project), string(run), key} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(part)))
		b = append(b, part...)
	}
	sum := sha256.Sum256(b)
	return "rr-" + hex.EncodeToString(sum[:])[:40], nil
}

func (v *RunResourceStateChanged) validate() error {
	if err := firstError(matchID("runId", v.RunID), CheckRunResourceKey(v.Kind, v.Key),
		oneOf("from", v.From, append([]string{RunResourcePhaseNone}, runResourcePhase...)...),
		oneOf("to", v.To, runResourcePhase...), matchCode("reason", v.Reason), matchDigest("specFingerprint", v.SpecFingerprint)); err != nil {
		return err
	}
	if v.From == v.To {
		return invalidf("from and to are the same phase %q", v.To)
	}
	if v.To == RunResourcePhaseReady && !v.UIDBound {
		return invalidf("a ready resource has a bound identity (uidBound must be true)")
	}
	return nil
}

// validateMembers requires every member, with uidBound the literal true or
// false: typed decoding cannot tell an omitted or null boolean from false.
func (v *RunResourceStateChanged) validateMembers(members map[string]json.RawMessage) error {
	for _, name := range []string{"runId", "kind", "key", "from", "to", "reason", "specFingerprint", "uidBound"} {
		if _, ok := members[name]; !ok {
			return invalidf("%s is required", name)
		}
	}
	if s := strings.TrimSpace(string(members["uidBound"])); s != "true" && s != "false" {
		return invalidf("uidBound must be a boolean")
	}
	return nil
}

// checkRunResourceEnvelope binds the envelope to its payload: the resource ID
// must be the RunResourceID of the envelope scope and the payload's run and key,
// so an event cannot describe a run resource of another scope.
func checkRunResourceEnvelope(e Envelope) error {
	var v RunResourceStateChanged
	if err := unmarshalExact(e.Data, &v); err != nil {
		return fmt.Errorf("%w: data: %v", ErrInvalid, err)
	}
	want, err := RunResourceID(e.TenantID, e.ProjectID, domain.RunID(v.RunID), v.Key)
	if err != nil {
		return err
	}
	if e.Resource.ID != want {
		return invalidf("resource.id must be the run resource ID of tenant, project, runId and key")
	}
	return nil
}
