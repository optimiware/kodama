package runtimev2

import (
	"fmt"
	"regexp"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Patterns are the string syntaxes new in v2; the shared ones are
// runtimev1.Patterns. A test requires schema.json's definitions to equal them.
var Patterns = map[string]string{
	"ServiceName":  `^[a-z][a-z0-9-]{0,29}$`,
	"PortName":     `^[a-z][a-z0-9-]{0,14}$`,
	"EgressName":   `^[a-z][a-z0-9-]{0,62}$`,
	"EndpointHost": `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`,
	"ResourceKey":  `^(preparation|service\.[a-z][a-z0-9-]{0,29})$`,
	"WorkloadName": `^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`,
	"WorkloadUID":  `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`,
}

var compiled = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for name, p := range runtimev1.Patterns {
		m[name] = regexp.MustCompile(p)
	}
	for name, p := range Patterns {
		if _, dup := m[name]; dup {
			panic("runtimev2: pattern " + name + " shadows a v1 pattern")
		}
		m[name] = regexp.MustCompile(p)
	}
	return m
}()

// Bounds fixed by the contract or chosen here as engineering bounds (marked).
const (
	maxLineage         = 8192 // engineering bound: far above steps x retries of a valid run
	maxEndpoints       = 8
	maxEndpointPorts   = 8
	maxEgress          = 8
	maxServiceCommand  = 64
	maxServiceArgs     = 256
	maxServiceEnv      = 64
	maxServiceSecrets  = 80 // 64 environment + 16 file bindings
	maxElementBytes    = 8192
	maxPrepareTimeout  = 604800
	maxPrepareTries    = 100 // engineering bound on the policy's preparationAttempts
	maxByteCount       = int64(1) << 40
	maxReceiptAttempts = maxPrepareTries
)

func fail(path, format string, args ...any) error {
	return &ValidationError{Path: path, Message: fmt.Sprintf(format, args...)}
}

func matches(path, pattern, value string) error {
	if !compiled[pattern].MatchString(value) {
		return fail(path, "must match %s syntax", pattern)
	}
	return nil
}

func wrap(path string, err error) error {
	if err == nil {
		return nil
	}
	return fail(path, "%v", err)
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func inRange(path string, v, lo, hi int64) error {
	if v < lo || v > hi {
		return fail(path, "must be between %d and %d", lo, hi)
	}
	return nil
}

func (s WorkspaceState) valid() bool {
	return s == StateNone || s == StateClean || s == StateModified
}

// ---- attempt context ----

// Validate checks the structure and the cross-field rules of the context.
func (c ContextV2) Validate() error {
	if c.ContextVersion != ContextVersion {
		return fail("contextVersion", "must be %d", ContextVersion)
	}
	if c.RunModel != RunModel {
		return fail("runModel", "must be %s", RunModel)
	}
	// Every field v2 shares with v1 has its v1 meaning and validator. The v1
	// validator is applied through a projection whose workspace is the trivially
	// valid source-free one; the v2 workspace is validated below.
	shared := runtimev1.Context{
		TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
		Workspace: runtimev1.Workspace{Access: runtimev1.AccessNone}, Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets,
		Environment: c.Environment, Limits: c.Limits,
	}
	return firstError(
		shared.Validate(),
		c.Workspace.validate(c.AttemptID),
		validateEndpoints(c.Endpoints),
		validateEgress("egress", c.Egress),
		c.Run.validate(),
		c.Policy.validate("policy"),
	)
}

func (w WorkspaceV2) validate(self domain.AttemptID) error {
	if w.MountPath != MountPath {
		return fail("workspace.mountPath", "must be %s", MountPath)
	}
	switch w.Access {
	case AccessNone, AccessRead, AccessWrite:
	default:
		return fail("workspace.access", "unknown access %q", w.Access)
	}
	sourceFree := w.Source == nil
	if sourceFree {
		if w.Preparation != nil {
			return fail("workspace.preparation", "must be null for a source-free run")
		}
		if w.Access != AccessNone {
			return fail("workspace.access", "a source-free run has access none")
		}
	} else {
		if w.Preparation == nil {
			return fail("workspace.preparation", "required for a source-backed run")
		}
		if w.Access == AccessNone {
			return fail("workspace.access", "a source-backed run has effective access read or write")
		}
		if err := w.Source.validate(w.Access); err != nil {
			return err
		}
		if err := w.Preparation.validate(w.Source.InputCommit); err != nil {
			return err
		}
	}
	if w.Lineage == nil || len(w.Lineage) > maxLineage {
		return fail("workspace.lineage", "must be an array of at most %d entries", maxLineage)
	}
	var (
		head     GitCommit
		lastTime string
		seen     = map[domain.AttemptID]bool{}
	)
	if !sourceFree {
		head = w.Source.InputCommit
	}
	for i, e := range w.Lineage {
		p := fmt.Sprintf("workspace.lineage[%d]", i)
		if err := e.validate(p, sourceFree); err != nil {
			return err
		}
		if e.AttemptID == self {
			return fail(p+".attemptId", "an attempt is not part of its own lineage")
		}
		if seen[e.AttemptID] {
			return fail(p+".attemptId", "duplicate attempt in the lineage")
		}
		seen[e.AttemptID] = true
		at, _ := e.BeganAt.Time()
		if lastTime != "" {
			prev, _ := Timestamp(lastTime).Time()
			if at.Before(prev) {
				return fail(p+".beganAt", "lineage must be in beganAt order")
			}
		}
		lastTime = string(e.BeganAt)
		if e.PublishedCommit != nil {
			head = *e.PublishedCommit
		}
	}
	if !sourceFree && w.Source.HeadCommit != head {
		return fail("workspace.source.headCommit", "must be the latest commit published in this run, or the input commit before any")
	}
	return nil
}

func (s *WorkspaceSource) validate(access WorkspaceAccess) error {
	if err := firstError(
		matches("workspace.source.repositoryUrl", "RepositoryURL", s.RepositoryURL),
		wrap("workspace.source.inputCommit", s.InputCommit.Validate()),
		wrap("workspace.source.headCommit", s.HeadCommit.Validate()),
	); err != nil {
		return err
	}
	if access == AccessRead {
		if s.Branch != nil || s.ExpectedTip != nil {
			return fail("workspace.source", "branch and expectedTip must be null for read access")
		}
		return nil
	}
	if s.Branch == nil || s.ExpectedTip == nil {
		return fail("workspace.source", "branch and expectedTip are required for write access")
	}
	return firstError(matches("workspace.source.branch", "BranchRef", *s.Branch), wrap("workspace.source.expectedTip", s.ExpectedTip.Validate()))
}

func (p *PreparationPin) validate(input GitCommit) error {
	if err := firstError(
		wrap("workspace.preparation.commit", p.Commit.Validate()),
		wrap("workspace.preparation.treeId", p.TreeID.Validate()),
		wrap("workspace.preparation.receiptDigest", p.ReceiptDigest.Validate()),
	); err != nil {
		return err
	}
	if p.Commit != input {
		return fail("workspace.preparation.commit", "the receipt must be for the run's pinned input commit")
	}
	return nil
}

func (e LineageEntry) validate(path string, sourceFree bool) error {
	if err := firstError(
		matches(path+".stepId", "StepName", e.StepID),
		wrap(path+".attemptId", e.AttemptID.Validate()),
		wrap(path+".beganAt", e.BeganAt.Validate()),
	); err != nil {
		return err
	}
	switch e.Access {
	case AccessNone, AccessRead, AccessWrite:
	default:
		return fail(path+".access", "unknown access %q", e.Access)
	}
	if !e.WorkspaceState.valid() {
		return fail(path+".workspaceState", "unknown state %q", e.WorkspaceState)
	}
	if e.PublishedCommit != nil {
		if err := wrap(path+".publishedCommit", e.PublishedCommit.Validate()); err != nil {
			return err
		}
		if e.Access != AccessWrite {
			return fail(path+".publishedCommit", "only a writer publishes a commit")
		}
	}
	if sourceFree {
		if e.Access != AccessNone || e.WorkspaceState != StateNone || e.IgnoredPresent || e.PublishedCommit != nil {
			return fail(path, "a source-free entry has access none, state none, ignoredPresent false and no published commit")
		}
		return nil
	}
	if e.Access == AccessNone {
		return fail(path+".access", "a source-backed attempt has effective access read or write")
	}
	if e.WorkspaceState == StateNone {
		return fail(path+".workspaceState", "state none is only for source-free attempts")
	}
	return nil
}

func validateEndpoints(m map[string]Endpoint) error {
	if m == nil || len(m) > maxEndpoints {
		return fail("endpoints", "must be an object of at most %d services", maxEndpoints)
	}
	for name, e := range m {
		p := "endpoints." + name
		if err := firstError(matches(p, "ServiceName", name), matches(p+".host", "EndpointHost", e.Host)); err != nil {
			return err
		}
		if e.Ports == nil || len(e.Ports) < 1 || len(e.Ports) > maxEndpointPorts {
			return fail(p+".ports", "must be an object of 1 to %d ports", maxEndpointPorts)
		}
		for port, n := range e.Ports {
			if err := firstError(matches(p+".ports."+port, "PortName", port), inRange(p+".ports."+port, n, 1, 65535)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEgress(path string, names []string) error {
	if names == nil || len(names) > maxEgress {
		return fail(path, "must be an array of at most %d names", maxEgress)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if err := matches(path, "EgressName", n); err != nil {
			return err
		}
		if seen[n] {
			return fail(path, "duplicate egress profile")
		}
		seen[n] = true
	}
	return nil
}

func (r RunWindow) validate() error {
	if err := firstError(wrap("run.startedAt", r.StartedAt.Validate()), wrap("run.deadlineAt", r.DeadlineAt.Validate())); err != nil {
		return err
	}
	start, _ := r.StartedAt.Time()
	end, _ := r.DeadlineAt.Time()
	if !end.After(start) {
		return fail("run.deadlineAt", "must be later than startedAt")
	}
	return nil
}

func (p PolicyPin) validate(path string) error {
	return firstError(matches(path+".id", "SafeName", p.ID), wrap(path+".digest", p.Digest.Validate()))
}

// Validate checks the response as a client receives it.
func (r AdmitResponseV2) Validate() error {
	if err := r.Token.Validate(); err != nil {
		return err
	}
	return r.Context.Validate()
}

// ---- begin ----

// Validate checks the request structure; whether a null or non-null workspace is
// right for the attempt depends on the context (DeriveBegin).
func (r BeginRequest) Validate() error {
	if r.Workspace == nil {
		return nil
	}
	return wrap("workspace.head", r.Workspace.Head.Validate())
}

// Validate checks the response structure.
func (r BeginResponse) Validate() error {
	if err := firstError(wrap("attemptId", r.AttemptID.Validate()), wrap("beganAt", r.BeganAt.Validate())); err != nil {
		return err
	}
	if !r.WorkspaceState.valid() {
		return fail("workspaceState", "unknown state %q", r.WorkspaceState)
	}
	if r.WorkspaceState == StateNone && r.IgnoredPresent {
		return fail("ignoredPresent", "must be false for a source-free attempt")
	}
	return nil
}

// ---- resource subjects ----

// Validate checks the identity. Both Pod fields are null or both are non-null.
func (i ResourceIdentity) Validate() error {
	if err := firstError(matches("identity.workloadName", "WorkloadName", i.WorkloadName), matches("identity.workloadUid", "WorkloadUID", i.WorkloadUID)); err != nil {
		return err
	}
	if (i.PodName == nil) != (i.PodUID == nil) {
		return fail("identity", "podName and podUid are both null or both set")
	}
	if i.PodName != nil {
		return firstError(matches("identity.podName", "WorkloadName", *i.PodName), matches("identity.podUid", "WorkloadUID", *i.PodUID))
	}
	return nil
}

func (r ResourceAdmitRequest) Validate() error {
	return firstError(
		matches("admissionCredential", "AdmissionCredential", r.AdmissionCredential),
		matches("nonce", "Nonce", r.Nonce),
		matches("clientVersion", "ClientVersion", r.ClientVersion),
		r.Identity.Validate(),
	)
}

func (t ResourceTokenInfo) Validate() error {
	if t.TokenType != "Bearer" {
		return fail("token.tokenType", "must be Bearer")
	}
	if t.Audience != ResourceAudience {
		return fail("token.audience", "must be %s", ResourceAudience)
	}
	if err := firstError(matches("token.token", "TokenValue", t.Token), wrap("token.expiresAt", t.ExpiresAt.Validate()), wrap("token.maxExpiresAt", t.MaxExpiresAt.Validate())); err != nil {
		return err
	}
	expires, _ := t.ExpiresAt.Time()
	maximum, _ := t.MaxExpiresAt.Time()
	if expires.After(maximum) {
		return fail("token.expiresAt", "must not be later than maxExpiresAt")
	}
	return nil
}

func (r ResourceAdmitResponse) Validate() error {
	return firstError(r.Token.Validate(), r.Context.Validate())
}

// Validate accepts only the empty object; the bearer token is the credential.
func (ResourceTokenRequest) Validate() error { return nil }

func (r ResourceTokenResponse) Validate() error { return r.Token.Validate() }

// Validate checks the context structure and its cross-field rules: the key
// matches the kind, exactly one of preparation and service is set, and a service
// with secret bindings declares its command.
func (c ResourceContext) Validate() error {
	if c.ContextVersion != ContextVersion {
		return fail("contextVersion", "must be %d", ContextVersion)
	}
	if err := firstError(
		wrap("tenantId", c.TenantID.Validate()), wrap("projectId", c.ProjectID.Validate()), wrap("runId", c.RunID.Validate()),
		matches("resourceKey", "ResourceKey", c.ResourceKey), c.Policy.validate("policy"),
	); err != nil {
		return err
	}
	switch c.ResourceKind {
	case ResourcePreparation:
		if c.ResourceKey != string(ResourcePreparation) {
			return fail("resourceKey", "a preparation resource has the key preparation")
		}
		if c.Preparation == nil || c.Service != nil {
			return fail("preparation", "exactly the preparation member is set for a preparation resource")
		}
		return c.Preparation.validate()
	case ResourceService:
		if c.Service == nil || c.Preparation != nil {
			return fail("service", "exactly the service member is set for a service resource")
		}
		if c.ResourceKey != "service."+c.Service.Name {
			return fail("resourceKey", "a service resource has the key service.NAME of its service")
		}
		return c.Service.validate()
	}
	return fail("resourceKind", "unknown kind %q", c.ResourceKind)
}

func (p *PreparationContext) validate() error {
	if p.MountPath != MountPath {
		return fail("preparation.mountPath", "must be %s", MountPath)
	}
	return firstError(
		matches("preparation.repositoryUrl", "RepositoryURL", p.RepositoryURL),
		wrap("preparation.commit", p.Commit.Validate()),
		inRange("preparation.timeoutSeconds", p.TimeoutSeconds, 1, maxPrepareTimeout),
		inRange("preparation.maxBytes", p.MaxBytes, 1, maxByteCount),
		inRange("preparation.maxAttempts", p.MaxAttempts, 1, maxPrepareTries),
		wrap("preparation.deadlineAt", p.DeadlineAt.Validate()),
	)
}

func (s *ServiceContext) validate() error {
	if err := firstError(matches("service.name", "ServiceName", s.Name), wrap("service.deadlineAt", s.DeadlineAt.Validate())); err != nil {
		return err
	}
	if s.Role != RoleGeneral && s.Role != RoleBuilder {
		return fail("service.role", "unknown role %q", s.Role)
	}
	if err := s.Command.validate(); err != nil {
		return err
	}
	if s.Secrets == nil || len(s.Secrets) > maxServiceSecrets {
		return fail("service.secrets", "must be an array of at most %d bindings", maxServiceSecrets)
	}
	names := map[string]bool{}
	for _, b := range s.Secrets {
		if err := firstError(
			matches("service.secrets.ref", "SecretRef", b.Ref), inRange("service.secrets.version", b.Version, 1, domain.MaxSafeInteger),
			matches("service.secrets.name", "SafeName", b.Name),
		); err != nil {
			return err
		}
		if b.Delivery != runtimev1.DeliveryFile && b.Delivery != runtimev1.DeliveryEnv {
			return fail("service.secrets.delivery", "unknown delivery %q", b.Delivery)
		}
		if names[b.Name] {
			return fail("service.secrets", "duplicate binding name")
		}
		names[b.Name] = true
	}
	if len(s.Secrets) > 0 && s.Command.Command == nil {
		return fail("service.command.command", "a service with secret bindings declares its command")
	}
	return nil
}

func (c ServiceCommand) validate() error {
	if err := matches("service.command.image", "ImageRef", c.Image); err != nil {
		return err
	}
	if c.Command != nil && (len(c.Command) < 1 || len(c.Command) > maxServiceCommand) {
		return fail("service.command.command", "must be null or an array of 1 to %d elements", maxServiceCommand)
	}
	if c.Args == nil || len(c.Args) > maxServiceArgs {
		return fail("service.command.args", "must be an array of at most %d elements", maxServiceArgs)
	}
	for _, a := range append(append([]string{}, c.Command...), c.Args...) {
		if len(a) > maxElementBytes {
			return fail("service.command", "argument longer than %d bytes", maxElementBytes)
		}
	}
	if c.Env == nil || len(c.Env) > maxServiceEnv {
		return fail("service.command.env", "must be an object of at most %d entries", maxServiceEnv)
	}
	for k, v := range c.Env {
		if err := matches("service.command.env."+k, "EnvName", k); err != nil {
			return err
		}
		if len(v) > maxElementBytes {
			return fail("service.command.env."+k, "value longer than %d bytes", maxElementBytes)
		}
	}
	return nil
}

// Validate checks the receipt request: the three safety fields have their literal
// values. Whether the commit is the run's input commit and the try count is
// within maxAttempts depends on the context (CheckReceipt).
func (r PreparationReceiptRequest) Validate() error {
	if err := firstError(wrap("commit", r.Commit.Validate()), wrap("treeId", r.TreeID.Validate())); err != nil {
		return err
	}
	if !r.HooksDisabled {
		return fail("hooksDisabled", "must be true")
	}
	if r.Submodules != SubmodulesSkipped {
		return fail("submodules", "must be %s", SubmodulesSkipped)
	}
	if !r.CredentialsRemoved {
		return fail("credentialsRemoved", "must be true")
	}
	return inRange("attempts", r.Attempts, 1, maxReceiptAttempts)
}

func (r PreparationReceiptResponse) Validate() error {
	return firstError(wrap("receiptDigest", r.ReceiptDigest.Validate()), wrap("recordedAt", r.RecordedAt.Validate()))
}

// Validate checks the envelope: a known v2 code and the v1 message bounds.
func (r ErrorResponse) Validate() error {
	e := r.Error
	if _, ok := Status(e.Code); !ok {
		return fail("error.code", "unknown code %q", e.Code)
	}
	if e.Message == "" || len(e.Message) > 2048 {
		return fail("error.message", "must be 1-2048 bytes")
	}
	if e.RequestID == "" || len(e.RequestID) > 128 {
		return fail("error.requestId", "must be 1-128 bytes")
	}
	if e.Details == nil {
		return fail("error.details", "must be an object")
	}
	return nil
}

// ResourceKeyOf returns the resource key of a kind and, for a service, its name.
func ResourceKeyOf(kind ResourceKind, name string) string {
	if kind == ResourceService {
		return "service." + name
	}
	return string(ResourcePreparation)
}

// ServiceNameOf extracts NAME from a service resource key.
func ServiceNameOf(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, "service.")
	return name, ok && compiled["ServiceName"].MatchString(name)
}
