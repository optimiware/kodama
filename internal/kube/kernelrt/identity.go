package kernelrt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Errors specific to this adapter. Each wraps a kernel sentinel so the engine
// classifies it without knowing Kubernetes.
var (
	// ErrForeign: an object under the derived name does not carry the caller's
	// tenant, project, run and identity. It is never adopted or deleted.
	ErrForeign = errors.New("kernelrt: object belongs to another identity")
	// ErrClosed: the identity was stopped or removed and is never started again.
	ErrClosed = fmt.Errorf("kernelrt: identity is stopped: %w", runtime.ErrExecutionClosed)
	// ErrUnverified: a live object does not have the restricted shape this
	// adapter writes, so it is not adopted.
	ErrUnverified = fmt.Errorf("kernelrt: object does not match the restricted specification: %w", runtime.ErrSpecMismatch)
)

// notReached marks an error as an effect that provably never reached the
// cluster, so the engine may retry it.
func notReached(err error) error {
	if errors.Is(err, runtime.ErrNotReached) {
		return err
	}
	return fmt.Errorf("%w: %w", runtime.ErrNotReached, err)
}

func unsupported(format string, args ...any) error {
	return notReached(fmt.Errorf("%w: %s", runtime.ErrCapability, fmt.Sprintf(format, args...)))
}

// Object kinds.
type objKind string

const (
	kindWorkload  objKind = "workload"
	kindWorkspace objKind = "workspace"
	kindService   objKind = "service"
)

func (k objKind) prefix() string {
	switch k {
	case kindWorkload:
		return "kj"
	case kindWorkspace:
		return "kw"
	}
	return "ks"
}

// Label and annotation keys. Labels hold hashes (identifiers can exceed the
// 63-character label limit); annotations hold the full identity.
const (
	labelManaged = "app.kubernetes.io/managed-by"
	valueManaged = "kodama-kernel"
	labelScope   = "kodama.io/scope"
	labelRun     = "kodama.io/run"
	labelKind    = "kodama.io/kind"
	labelRole    = "kodama.io/role"
	labelObject  = "kodama.io/object"

	roleRecord   = "record"
	roleObject   = "object"
	roleMaterial = "material"

	annTenant     = "kodama.io/tenant"
	annProject    = "kodama.io/project"
	annRun        = "kodama.io/run"
	annStep       = "kodama.io/step"
	annAttempt    = "kodama.io/attempt"
	annIdentity   = "kodama.io/identity"
	annName       = "kodama.io/name"
	annBackend    = "kodama.io/backend"
	annDigest     = "kodama.io/spec-digest"
	annFence      = "kodama.io/fence"
	annState      = "kodama.io/state"
	annVolumes    = "kodama.io/volumes"
	annSecretsDir = "kodama.io/secrets-dir"
	annSettled    = "kodama.io/state-since"
	// annMarkerPrefix and annCreatedPrefix carry per-operation evidence: a
	// marker is a create that was issued and may still land; a created UID is
	// the instance this identity established.
	annMarkerPrefix  = "kodama.io/inflight."
	annReapingPrefix = "kodama.io/reaping."
	annCreatedPrefix = "kodama.io/created."
	annClaimGone     = "kodama.io/claim-gone"
	annCompensated   = "kodama.io/compensated-claims"
	annWorkspace     = "kodama.io/workspace-claim"

	// Operations that create one cluster object each.
	opJob     = "job"
	opPod     = "pod"
	opService = "service"
	opPVC     = "pvc"
	opSecret  = "secret"

	containerStep = "step"
	containerSvc  = "service"
)

// ident is the stable identity of one object the adapter owns.
type ident struct {
	scope   domain.Scope
	run     domain.RunID
	step    domain.StepID
	attempt int
	kind    objKind
	id      string // WorkloadID or resource Identity
	name    string // resource name (declared service name)
	digest  string
	backend string
}

func h(parts ...string) string {
	sum := sha256.New()
	for _, p := range parts {
		sum.Write([]byte(strconv.Itoa(len(p)) + ":" + p))
	}
	return hex.EncodeToString(sum.Sum(nil))[:32]
}

// base is the derived object name: stable for the identity, unique across
// tenants, a valid DNS label and a valid label value.
func (i ident) base() string {
	return i.kind.prefix() + "-" + h(string(i.scope.TenantID), string(i.scope.ProjectID), string(i.run), string(i.kind), i.id)
}

func (i ident) recordName() string   { return i.base() + "-rec" }
func (i ident) materialName() string { return i.base() + "-sec" }

func scopeHash(s domain.Scope) string { return h(string(s.TenantID), string(s.ProjectID)) }
func runHash(s domain.Scope, r domain.RunID) string {
	return h(string(s.TenantID), string(s.ProjectID), string(r))
}

func (i ident) labels(role string) map[string]string {
	return map[string]string{
		labelManaged: valueManaged,
		labelScope:   scopeHash(i.scope),
		labelRun:     runHash(i.scope, i.run),
		labelKind:    string(i.kind),
		labelRole:    role,
		labelObject:  i.base(),
	}
}

// annotations are the immutable ownership evidence. The spec digest is the
// engine's immutable specification identity.
func (i ident) annotations() map[string]string {
	a := map[string]string{
		annTenant: string(i.scope.TenantID), annProject: string(i.scope.ProjectID), annRun: string(i.run),
		annIdentity: i.id, annBackend: i.backend, annDigest: i.digest,
	}
	if i.step != "" {
		a[annStep] = string(i.step)
		a[annAttempt] = strconv.Itoa(i.attempt)
	}
	if i.name != "" {
		a[annName] = i.name
	}
	return a
}

// owns reports whether labels and annotations say the object belongs to i. The
// specification digest is compared separately: a different digest on an owned
// object is a mismatch, not a foreign object.
func (i ident) owns(labels, ann map[string]string) bool {
	if labels[labelManaged] != valueManaged || labels[labelKind] != string(i.kind) || labels[labelObject] != i.base() {
		return false
	}
	want := i.annotations()
	for _, k := range []string{annTenant, annProject, annRun, annIdentity, annStep, annName} {
		if ann[k] != want[k] {
			return false
		}
	}
	return true
}

// digestMatches compares the recorded specification digest. A handle carries
// no digest, so an identity without one matches any.
func (i ident) digestMatches(ann map[string]string) bool {
	return i.digest == "" || ann[annDigest] == i.digest
}

// identFromAnnotations rebuilds the identity of an object found by listing; the
// reconciler removes it with the recorded identity, not a caller's.
func identFromAnnotations(kind objKind, ann map[string]string) (ident, error) {
	i := ident{
		scope: domain.Scope{TenantID: domain.TenantID(ann[annTenant]), ProjectID: domain.ProjectID(ann[annProject])},
		run:   domain.RunID(ann[annRun]), step: domain.StepID(ann[annStep]), kind: kind,
		id: ann[annIdentity], name: ann[annName], digest: ann[annDigest], backend: ann[annBackend],
	}
	if a := ann[annAttempt]; a != "" {
		n, err := strconv.Atoi(a)
		if err != nil {
			return ident{}, fmt.Errorf("kernelrt: record attempt %q", a)
		}
		i.attempt = n
	}
	if err := i.scope.Validate(); err != nil || i.id == "" {
		return ident{}, fmt.Errorf("kernelrt: record has no valid identity")
	}
	return i, nil
}

func workloadIdent(b runtime.RuntimeBinding) ident {
	return ident{scope: b.Scope, run: b.RunID, step: b.StepKey, attempt: b.Attempt, kind: kindWorkload, id: b.WorkloadID, digest: b.SpecDigest, backend: b.Backend}
}

func resourceIdent(scope domain.Scope, run domain.RunID, kind runtime.ResourceKind, name, identity, digest, backend string) ident {
	k := kindService
	if kind == runtime.ResourceWorkspace {
		k = kindWorkspace
	}
	return ident{scope: scope, run: run, kind: k, id: identity, name: name, digest: digest, backend: backend}
}

var imagePinRE = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// pinned reports whether the image names exact content.
func pinned(image string) bool {
	return !strings.ContainsAny(image, " \t\n") && imagePinRE.MatchString(image)
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}
