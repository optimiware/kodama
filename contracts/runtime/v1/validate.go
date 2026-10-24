package runtimev1

import (
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

const (
	maxQuestionRunes = 4096
	maxOutputs       = 64
	maxDiagnostics   = 32
	maxListItems     = 256
	maxInt           = int64(1) << 40
)

func inRange(path string, v, lo, hi int64) error {
	if v < lo || v > hi {
		return fail(path, "must be between %d and %d", lo, hi)
	}
	return nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r AdmitRequest) Validate() error {
	return firstError(
		matches("admissionCredential", "AdmissionCredential", r.AdmissionCredential),
		matches("nonce", "Nonce", r.Nonce),
		matches("clientVersion", "ClientVersion", r.ClientVersion),
		r.Job.Validate(),
	)
}

func (j JobIdentity) Validate() error {
	return firstError(
		matches("job.name", "KubernetesName", j.Name),
		matches("job.uid", "UID", j.UID),
		matches("job.podName", "KubernetesName", j.PodName),
		matches("job.podUid", "UID", j.PodUID),
	)
}

func (t TokenInfo) Validate() error {
	if t.TokenType != "Bearer" {
		return fail("token.tokenType", "must be Bearer")
	}
	if t.Audience != Audience {
		return fail("token.audience", "must be %s", Audience)
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

func (r AdmitResponse) Validate() error {
	if err := r.Token.Validate(); err != nil {
		return err
	}
	return r.Context.Validate()
}

func (r TokenResponse) Validate() error { return r.Token.Validate() }

func (c Context) Validate() error {
	if err := firstError(
		wrap("tenantId", c.TenantID.Validate()), wrap("projectId", c.ProjectID.Validate()),
		wrap("runId", c.RunID.Validate()), wrap("attemptId", c.AttemptID.Validate()),
		matches("stepId", "StepName", c.StepID), matches("actionKey", "ActionKey", c.ActionKey),
		wrap("definition.id", c.Definition.ID.Validate()), wrap("definition.digest", c.Definition.Digest.Validate()),
	); err != nil {
		return err
	}
	if c.WorkItem != nil {
		if err := c.WorkItem.validate(); err != nil {
			return err
		}
	}
	if c.DesignBaseline != nil {
		if err := c.DesignBaseline.Validate(); err != nil {
			return err
		}
	}
	if p := c.Profile; p != nil {
		if err := firstError(matches("profile.name", "Name", p.Name), inRange("profile.version", p.Version, 1, domain.MaxSafeInteger), matches("profile.wrapperVersion", "ClientVersion", p.WrapperVersion)); err != nil {
			return err
		}
	}
	if err := firstError(c.Command.validate(), c.Workspace.validate(), c.Limits.validate()); err != nil {
		return err
	}
	if c.Inputs == nil {
		return fail("inputs", "must be an object")
	}
	for name, v := range c.Inputs {
		if err := firstError(matches("inputs."+name, "Name", name), v.Validate()); err != nil {
			return err
		}
	}
	if err := c.validateOutputs(); err != nil {
		return err
	}
	if err := c.validateSecrets(); err != nil {
		return err
	}
	if e := c.Environment; e != nil {
		return firstError(wrap("environment.id", e.ID.Validate()), wrap("environment.configDigest", e.ConfigDigest.Validate()))
	}
	return nil
}

func (w *WorkItem) validate() error {
	if err := firstError(wrap("workItem.id", w.ID.Validate()), wrap("workItem.contentRevision", w.ContentRevision.Validate())); err != nil {
		return err
	}
	if len(w.Spec) > 1<<20 {
		return fail("workItem.spec", "longer than 1 MiB")
	}
	if w.AcceptanceCriteria == nil || len(w.AcceptanceCriteria) > maxListItems {
		return fail("workItem.acceptanceCriteria", "must be an array of at most %d items", maxListItems)
	}
	seen := map[string]bool{}
	for _, ac := range w.AcceptanceCriteria {
		if err := matches("workItem.acceptanceCriteria.id", "SafeName", ac.ID); err != nil {
			return err
		}
		if seen[ac.ID] {
			return fail("workItem.acceptanceCriteria", "duplicate id %q", ac.ID)
		}
		seen[ac.ID] = true
		if ac.Text == "" || len(ac.Text) > 4096 {
			return fail("workItem.acceptanceCriteria.text", "must be 1-4096 bytes")
		}
	}
	return nil
}

func (c Command) validate() error {
	if err := matches("command.image", "ImageRef", c.Image); err != nil {
		return err
	}
	if c.Command == nil || c.Args == nil || len(c.Command) > maxListItems || len(c.Args) > maxListItems {
		return fail("command", "command and args must be arrays of at most %d items", maxListItems)
	}
	for _, s := range append(append([]string{}, c.Command...), c.Args...) {
		if len(s) > 4096 {
			return fail("command", "argument longer than 4096 bytes")
		}
	}
	if c.Env == nil || len(c.Env) > maxListItems {
		return fail("command.env", "must be an object of at most %d entries", maxListItems)
	}
	for k, v := range c.Env {
		if err := matches("command.env."+k, "EnvName", k); err != nil {
			return err
		}
		if len(v) > 4096 {
			return fail("command.env."+k, "value longer than 4096 bytes")
		}
	}
	return nil
}

func (w Workspace) validate() error {
	switch w.Access {
	case AccessNone:
		if w.Source != nil {
			return fail("workspace.source", "must be null when access is none")
		}
		return nil
	case AccessRead, AccessWrite:
	default:
		return fail("workspace.access", "unknown access %q", w.Access)
	}
	s := w.Source
	if s == nil {
		return fail("workspace.source", "required when access is %s", w.Access)
	}
	if err := firstError(matches("workspace.source.repositoryUrl", "RepositoryURL", s.RepositoryURL), wrap("workspace.source.commit", s.Commit.Validate())); err != nil {
		return err
	}
	if w.Access == AccessRead {
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

func (l Limits) validate() error {
	return firstError(
		inRange("limits.timeoutSeconds", l.TimeoutSeconds, 1, 604800),
		inRange("limits.maxOutputBytes", l.MaxOutputBytes, 1, maxInt),
		inRange("limits.maxOutputFiles", l.MaxOutputFiles, 1, 100000),
		inRange("limits.maxDiagnosticBytes", l.MaxDiagnosticBytes, 1, maxInt),
	)
}

func (c Context) validateOutputs() error {
	if c.Outputs == nil || len(c.Outputs) > maxOutputs {
		return fail("outputs", "must be an array of at most %d declarations", maxOutputs)
	}
	seen := map[string]bool{}
	for _, o := range c.Outputs {
		if err := matches("outputs.name", "Name", o.Name); err != nil {
			return err
		}
		if seen[o.Name] {
			return fail("outputs", "duplicate output %q", o.Name)
		}
		seen[o.Name] = true
		switch o.Type {
		case OutputArtifact:
			if o.Path == nil {
				return fail("outputs."+o.Name+".path", "required for file artifacts")
			}
			if err := matches("outputs."+o.Name+".path", "OutputPath", *o.Path); err != nil {
				return err
			}
		case OutputString, OutputBoolean, OutputInteger, OutputOCIArtifact:
			if o.Path != nil {
				return fail("outputs."+o.Name+".path", "must be null for type %s", o.Type)
			}
		default:
			return fail("outputs."+o.Name+".type", "unknown type %q", o.Type)
		}
	}
	return nil
}

func (c Context) validateSecrets() error {
	if c.Secrets == nil || len(c.Secrets) > maxListItems {
		return fail("secrets", "must be an array of at most %d bindings", maxListItems)
	}
	names := map[string]bool{}
	for _, s := range c.Secrets {
		if err := firstError(matches("secrets.ref", "SecretRef", s.Ref), inRange("secrets.version", s.Version, 1, domain.MaxSafeInteger), matches("secrets.name", "SafeName", s.Name)); err != nil {
			return err
		}
		if s.Delivery != DeliveryFile && s.Delivery != DeliveryEnv {
			return fail("secrets.delivery", "unknown delivery %q", s.Delivery)
		}
		if names[s.Name] {
			return fail("secrets", "duplicate binding name %q", s.Name)
		}
		names[s.Name] = true
	}
	return nil
}

func (r SecretResolveRequest) Validate() error {
	return firstError(matches("ref", "SecretRef", r.Ref), inRange("version", r.Version, 1, domain.MaxSafeInteger))
}

func (r SecretResolveResponse) Validate() error {
	if err := firstError(matches("ref", "SecretRef", r.Ref), inRange("version", r.Version, 1, domain.MaxSafeInteger), matches("name", "SafeName", r.Name), matches("valueBase64", "Base64", r.ValueBase64)); err != nil {
		return err
	}
	if len(r.ValueBase64) > 1<<20 || len(r.ValueBase64)%4 != 0 {
		return fail("valueBase64", "must be padded base64 of at most 1 MiB")
	}
	if r.Delivery != DeliveryFile && r.Delivery != DeliveryEnv {
		return fail("delivery", "unknown delivery %q", r.Delivery)
	}
	return nil
}

func (r ArtifactReserveRequest) Validate() error {
	if err := firstError(matches("name", "SafeName", r.Name), wrap("digest", r.Digest.Validate())); err != nil {
		return err
	}
	if r.Role != RoleOutput && r.Role != RoleDiagnostic && r.Role != RoleBundle {
		return fail("role", "unknown role %q", r.Role)
	}
	switch r.Kind {
	case KindFile:
		if r.MediaType == nil || r.SizeBytes == nil || r.Registry != nil {
			return fail("kind", "file artifacts require mediaType and sizeBytes and a null registry")
		}
		return firstError(matches("mediaType", "MediaType", *r.MediaType), inRange("sizeBytes", *r.SizeBytes, 0, maxInt))
	case KindOCI:
		if r.Registry == nil || r.MediaType != nil || r.SizeBytes != nil || r.Role != RoleOutput {
			return fail("kind", "oci artifacts require registry, an output role, and null mediaType and sizeBytes")
		}
		return matches("registry", "RegistryRef", *r.Registry)
	}
	return fail("kind", "unknown kind %q", r.Kind)
}

func (r ArtifactReserveResponse) Validate() error {
	if err := wrap("artifactId", r.ArtifactID.Validate()); err != nil {
		return err
	}
	switch r.State {
	case StateReserved:
		if r.Upload == nil {
			return fail("upload", "required for a reserved artifact")
		}
		return r.Upload.validate()
	case StateRegistered:
		if r.Upload != nil {
			return fail("upload", "must be null for a registered artifact")
		}
		return nil
	}
	return fail("state", "must be reserved or registered")
}

func (u *Upload) validate() error {
	if u.Method != "PUT" {
		return fail("upload.method", "must be PUT")
	}
	if u.Headers == nil || len(u.Headers) > 32 {
		return fail("upload.headers", "must be an object of at most 32 entries")
	}
	return firstError(matches("upload.url", "UploadURL", u.URL), wrap("upload.expiresAt", u.ExpiresAt.Validate()))
}

func (r ArtifactCompleteRequest) Validate() error {
	return firstError(wrap("digest", r.Digest.Validate()), inRange("sizeBytes", r.SizeBytes, 0, maxInt))
}

func (r ArtifactCompleteResponse) Validate() error {
	if r.State != StateUsable {
		return fail("state", "must be usable")
	}
	return firstError(wrap("artifactId", r.ArtifactID.Validate()), wrap("digest", r.Digest.Validate()), inRange("sizeBytes", r.SizeBytes, 0, maxInt))
}

func (r SourceProposalRequest) Validate() error {
	return firstError(wrap("commit", r.Commit.Validate()), wrap("expectedTip", r.ExpectedTip.Validate()), wrap("bundleArtifactId", r.BundleArtifactID.Validate()))
}

func (r SourceProposalResponse) Validate() error {
	if err := wrap("commit", r.Commit.Validate()); err != nil {
		return err
	}
	switch r.Status {
	case PublishAccepted:
		if r.AcceptedCommit == nil || *r.AcceptedCommit != r.Commit {
			return fail("acceptedCommit", "must equal commit when accepted")
		}
	case PublishPending:
		if r.AcceptedCommit != nil {
			return fail("acceptedCommit", "must be null while pending")
		}
	default:
		return fail("status", "must be accepted or pending")
	}
	return nil
}

func (r Result) Validate() error {
	if r.ExitCode != nil {
		if err := inRange("exitCode", *r.ExitCode, 0, 255); err != nil {
			return err
		}
	}
	if r.SourceCommit != nil {
		if err := wrap("sourceCommit", r.SourceCommit.Validate()); err != nil {
			return err
		}
	}
	if r.Reason != nil {
		if err := matches("reason", "ReasonCode", *r.Reason); err != nil {
			return err
		}
	}
	if err := r.validateOutcome(); err != nil {
		return err
	}
	if r.Outputs == nil || len(r.Outputs) > maxOutputs {
		return fail("outputs", "must be an object of at most %d entries", maxOutputs)
	}
	for name, v := range r.Outputs {
		if err := firstError(matches("outputs."+name, "Name", name), v.Validate()); err != nil {
			return err
		}
	}
	if r.DiagnosticArtifactIDs == nil || len(r.DiagnosticArtifactIDs) > maxDiagnostics {
		return fail("diagnosticArtifactIds", "must be an array of at most %d IDs", maxDiagnostics)
	}
	seen := map[domain.ArtifactID]bool{}
	for _, id := range r.DiagnosticArtifactIDs {
		if err := wrap("diagnosticArtifactIds", id.Validate()); err != nil {
			return err
		}
		if seen[id] {
			return fail("diagnosticArtifactIds", "duplicate artifact %q", id)
		}
		seen[id] = true
	}
	return nil
}

func (r Result) validateOutcome() error {
	switch r.Outcome {
	case OutcomeCompleted:
		if r.ExitCode == nil || *r.ExitCode != 0 {
			return fail("exitCode", "completed requires exit code 0")
		}
		if r.Reason != nil || r.Question != nil {
			return fail("outcome", "completed requires null reason and question")
		}
	case OutcomeFailed:
		if r.Reason == nil {
			return fail("reason", "failed requires a reason code")
		}
		if r.Question != nil || r.SourceCommit != nil || len(r.Outputs) != 0 {
			return fail("outcome", "failed requires a null question and sourceCommit and no outputs")
		}
	case OutcomeNeedsInput:
		if r.Question == nil || strings.TrimSpace(*r.Question) == "" || utf8.RuneCountInString(*r.Question) > maxQuestionRunes {
			return fail("question", "needs_input requires a non-blank question of at most %d characters", maxQuestionRunes)
		}
		if r.Reason != nil || r.SourceCommit != nil || len(r.Outputs) != 0 {
			return fail("outcome", "needs_input requires a null reason and sourceCommit and no outputs")
		}
	default:
		return fail("outcome", "unknown outcome %q", r.Outcome)
	}
	return nil
}

// Validate checks the request structurally and that ResultHash is the hash of
// the canonical result.
func (r ResultRequest) Validate() error {
	if err := firstError(wrap("resultHash", r.ResultHash.Validate()), r.Result.Validate()); err != nil {
		return err
	}
	got, err := ResultHash(r.Result)
	if err != nil {
		return err
	}
	if got != r.ResultHash {
		return &ContractError{Code: CodeResultHashMismatch, Message: "resultHash does not match the canonical result"}
	}
	return nil
}

func (r ResultResponse) Validate() error {
	if r.AttemptStatus != AttemptSucceeded && r.AttemptStatus != AttemptFailed && r.AttemptStatus != AttemptCancelled {
		return fail("attemptStatus", "unknown status %q", r.AttemptStatus)
	}
	return firstError(wrap("attemptId", r.AttemptID.Validate()), wrap("resultHash", r.ResultHash.Validate()), wrap("acceptedAt", r.AcceptedAt.Validate()))
}

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

// Validate accepts only the empty object; the bearer token is the credential.
func (TokenRequest) Validate() error { return nil }
