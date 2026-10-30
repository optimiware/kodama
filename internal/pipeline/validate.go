package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
	"github.com/optimiware/kodama/internal/domain"
)

// Limits bound what a valid pipeline may contain. They are library options with
// conservative defaults; installation configuration, when it exists, supplies
// them explicitly at the composition root.
type Limits struct {
	MaxSteps       int
	MaxInputs      int
	MaxNeeds       int
	MaxArgs        int
	MaxEnv         int
	MaxSecretFiles int
	MaxOutputs     int
	MaxEvidence    int
	// MaxStringBytes bounds every command element, argument, literal and default.
	MaxStringBytes int

	MaxTimeoutSeconds int64
	MaxRetryAttempts  int64
	MaxBackoffSeconds int64
}

// maxStepsCeiling is the hard ceiling of Limits.MaxSteps: graph checks keep a
// steps-by-steps reachability matrix.
const maxStepsCeiling = 4096

// DefaultLimits returns the conservative local limits.
func DefaultLimits() Limits {
	return Limits{
		MaxSteps: 256, MaxInputs: 64, MaxNeeds: 64, MaxArgs: 256, MaxEnv: 64, MaxSecretFiles: 16,
		MaxOutputs: 32, MaxEvidence: 32, MaxStringBytes: 8192,
		MaxTimeoutSeconds: 24 * 60 * 60, MaxRetryAttempts: 10, MaxBackoffSeconds: 60 * 60,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	pick := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	pick(&l.MaxSteps, d.MaxSteps)
	pick(&l.MaxInputs, d.MaxInputs)
	pick(&l.MaxNeeds, d.MaxNeeds)
	pick(&l.MaxArgs, d.MaxArgs)
	pick(&l.MaxEnv, d.MaxEnv)
	pick(&l.MaxSecretFiles, d.MaxSecretFiles)
	pick(&l.MaxOutputs, d.MaxOutputs)
	pick(&l.MaxEvidence, d.MaxEvidence)
	pick(&l.MaxStringBytes, d.MaxStringBytes)
	pick64 := func(v *int64, def int64) {
		if *v <= 0 {
			*v = def
		}
	}
	pick64(&l.MaxTimeoutSeconds, d.MaxTimeoutSeconds)
	pick64(&l.MaxRetryAttempts, d.MaxRetryAttempts)
	pick64(&l.MaxBackoffSeconds, d.MaxBackoffSeconds)
	return l
}

// Config wires a Validator. Capabilities is required: with no statement of
// which step kinds are enabled nothing is registrable. Profiles and References
// may be nil; a document that needs a missing port is then rejected as
// unavailable, never accepted.
type Config struct {
	Parse        Options
	Limits       Limits
	Capabilities Capabilities
	Profiles     ProfileLookup
	References   ReferencePolicy
	// EnableV1Alpha2 opts the validator (and so registration) in to the
	// kodama/v1alpha2 run_shared document format. The zero value keeps every
	// installation on kodama/v1alpha1 only: a v1alpha2 document is then rejected
	// with format_disabled. Enabling it states only that registration accepts
	// the format; it is never evidence that run admission supports it.
	EnableV1Alpha2 bool
	// EnableV1Alpha3 opts in to the container-plugin document format. It is
	// independent of EnableV1Alpha2 so an installation can stage the new
	// bindings without accepting the older shared format for new runs.
	EnableV1Alpha3 bool
}

// Validator is the one structural and semantic validation library. The API and
// the local CLI both use it, so a document is valid in both places or neither.
// It is safe for concurrent use.
type Validator struct {
	parse  Options
	limits Limits
	caps   Capabilities
	prof   ProfileLookup
	refs   ReferencePolicy
	v2     bool
	v3     bool
}

// NewValidator validates cfg.
func NewValidator(cfg Config) (*Validator, error) {
	if cfg.Capabilities == nil {
		return nil, errors.New("pipeline: a capabilities lookup is required")
	}
	l := cfg.Limits.withDefaults()
	if l.MaxSteps > maxStepsCeiling {
		return nil, fmt.Errorf("pipeline: MaxSteps must not exceed %d", maxStepsCeiling)
	}
	return &Validator{parse: cfg.Parse.withDefaults(), limits: l, caps: cfg.Capabilities, prof: cfg.Profiles, refs: cfg.References, v2: cfg.EnableV1Alpha2, v3: cfg.EnableV1Alpha3}, nil
}

// FormatEnabled reports whether this validator registers documents of the given
// apiVersion. kodama/v1alpha1 is always enabled; shared formats only when their
// corresponding enable flag was set.
func (v *Validator) FormatEnabled(apiVersion string) bool {
	switch apiVersion {
	case pipelinev1.APIVersion:
		return true
	case pipelinev1alpha2.APIVersion:
		return v.v2
	case pipelinev1alpha3.APIVersion:
		return v.v3
	}
	return false
}

// ParseYAML parses one YAML document with this validator's bounds.
func (v *Validator) ParseYAML(data []byte) (Document, error) { return ParseYAML(data, v.parse) }

// ParseJSON parses one JSON document with this validator's bounds.
func (v *Validator) ParseJSON(data []byte) (Document, error) { return ParseJSON(data, v.parse) }

// ValidateYAML parses and validates a YAML document.
func (v *Validator) ValidateYAML(ctx context.Context, scope domain.Scope, actor Actor, data []byte) (Validated, error) {
	doc, err := v.ParseYAML(data)
	if err != nil {
		return Validated{}, err
	}
	return v.Validate(ctx, scope, actor, doc)
}

// ValidateJSON parses and validates a JSON document.
func (v *Validator) ValidateJSON(ctx context.Context, scope domain.Scope, actor Actor, data []byte) (Validated, error) {
	doc, err := v.ParseJSON(data)
	if err != nil {
		return Validated{}, err
	}
	return v.Validate(ctx, scope, actor, doc)
}

var (
	nameRE       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	identifierRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	fileNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	imageRE      = regexp.MustCompile(`^[a-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	profileRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	secretRefRE  = regexp.MustCompile(`^secret://project/([A-Za-z0-9][A-Za-z0-9._-]{0,127})$`)
)

const maxImageBytes = 512

// Validate applies every structural and semantic rule to doc and, when they
// pass, the capability, profile and reference lookups. It returns a Validated
// value, or a *ValidationError (errors.Is ErrInvalid) with bounded diagnostics,
// or an error wrapping ErrUnavailable when a lookup could not answer. Lookups
// run only for a document that is otherwise valid, never inside a database
// transaction, and never retrieve secret values.
func (v *Validator) Validate(ctx context.Context, scope domain.Scope, actor Actor, doc Document) (Validated, error) {
	if err := scope.Validate(); err != nil {
		return Validated{}, err
	}
	if err := actor.Validate(); err != nil {
		return Validated{}, err
	}
	if doc.IsShared() {
		return v.validateV2(ctx, scope, actor, doc)
	}
	var d diagnostics
	if path, ok := doc.hasV2Fields(); ok {
		d.add(CodeUnknownField, path, "the field is not part of this pipeline format version")
	}
	v.checkForbidden(doc, &d)
	n := normalize(doc)
	v.checkDocument(n, &d)
	if d.any() {
		return Validated{}, d.err(StageSemantic)
	}
	if err := v.checkLookups(ctx, scope, actor, n, &d); err != nil {
		return Validated{}, err
	}
	if d.any() {
		return Validated{}, d.err(StageSemantic)
	}
	out, err := newValidated(n)
	if err != nil {
		return Validated{}, err
	}
	return out, nil
}

// checkForbidden finds fields set on a step of a kind that does not have them.
// It runs on the document as given, because normalization drops them.
func (v *Validator) checkForbidden(doc Document, d *diagnostics) {
	for i, s := range doc.Spec.Steps {
		p := stepPath(i)
		set := map[string]bool{
			"workspace": s.Workspace != nil, "environment": s.Environment != "", "image": s.Image != "",
			"command": s.Command != nil, "args": s.Args != nil, "env": s.Env != nil, "secretFiles": s.SecretFiles != nil,
			"timeoutSeconds": s.TimeoutSeconds != 0, "retry": s.Retry != nil, "outputs": s.Outputs != nil,
			"agent": s.Agent != nil, "gate": s.Gate != nil, "when": s.When != nil,
		}
		var forbidden []string
		switch s.Kind {
		case KindGate:
			forbidden = []string{"when", "workspace", "environment", "image", "command", "args", "env", "secretFiles", "timeoutSeconds", "retry", "outputs", "agent"}
		case KindAgent:
			forbidden = []string{"environment", "image", "command", "args", "env", "secretFiles", "gate"}
		case KindContainer:
			forbidden = []string{"agent", "gate"}
		}
		for _, f := range forbidden {
			if !set[f] {
				continue
			}
			switch {
			case s.Kind == KindGate && f == "when":
				d.add(CodeConditionalGate, p+".when", "gates are unconditional and cannot have a when condition")
			case s.Kind == KindGate:
				d.add(CodeGateField, p+"."+f, "gates have no executable fields")
			default:
				d.add(CodeUnknownField, p+"."+f, "the field is not part of a %s step", s.Kind)
			}
		}
	}
}

func (v *Validator) checkString(path, s string, d *diagnostics) {
	if len(s) > v.limits.MaxStringBytes {
		d.add(CodeLimitExceeded, path, "the value is longer than %d bytes", v.limits.MaxStringBytes)
	}
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		d.add(CodeInvalidValue, path, "the value must be UTF-8 text without NUL characters")
	}
}

func (v *Validator) checkDocument(doc Document, d *diagnostics) {
	if doc.APIVersion != pipelinev1.APIVersion {
		d.add(CodeInvalidValue, "$.apiVersion", "apiVersion must be %s", pipelinev1.APIVersion)
	}
	if doc.Kind != pipelinev1.Kind {
		d.add(CodeInvalidValue, "$.kind", "kind must be %s", pipelinev1.Kind)
	}
	if !nameRE.MatchString(doc.Metadata.Name) {
		d.add(CodeInvalidFormat, "$.metadata.name", "the name must be a lowercase letter followed by lowercase letters, digits or hyphens, at most 63 characters")
	}
	steps := doc.Spec.Steps
	if len(steps) == 0 {
		d.add(CodeMissingField, "$.spec.steps", "at least one step is required")
		return
	}
	if len(steps) > v.limits.MaxSteps {
		d.add(CodeLimitExceeded, "$.spec.steps", "a pipeline may have at most %d steps", v.limits.MaxSteps)
		return
	}
	v.checkInputs(doc, d)

	ids := map[string]int{}
	for i, s := range steps {
		p := stepPath(i)
		switch {
		case !nameRE.MatchString(s.ID):
			d.add(CodeInvalidFormat, p+".id", "the step id must be a lowercase letter followed by lowercase letters, digits or hyphens, at most 63 characters")
		case s.ID == RunSource:
			d.add(CodeReservedID, p+".id", "run is reserved for workspace sources")
		default:
			if first, dup := ids[s.ID]; dup {
				d.add(CodeDuplicateStep, p+".id", "the step id %s is already used by step %d", s.ID, first)
			} else {
				ids[s.ID] = i
			}
		}
		if !s.Kind.Valid() {
			d.add(CodeInvalidValue, p+".kind", "the step kind must be container, agent or gate")
		}
		if len(s.Needs) > v.limits.MaxNeeds {
			d.add(CodeLimitExceeded, p+".needs", "a step may have at most %d dependencies", v.limits.MaxNeeds)
		}
	}

	g := buildGraph(steps, d)
	g.checkWriters(steps, d)
	r := &resolver{doc: doc, graph: g, d: d}
	for i, s := range steps {
		g.checkSource(steps, i, d)
		switch s.Kind {
		case KindContainer:
			v.checkContainer(i, s, r)
			v.checkExecutable(i, s, doc, r)
		case KindAgent:
			if s.Agent == nil || !profileRE.MatchString(s.Agent.Profile) {
				d.add(CodeInvalidFormat, stepPath(i)+".agent.profile", "an agent step must name its profile")
			}
			v.checkExecutable(i, s, doc, r)
		case KindGate:
			v.checkGate(i, s, r)
		}
	}
}

func (v *Validator) checkInputs(doc Document, d *diagnostics) {
	if len(doc.Spec.Inputs) > v.limits.MaxInputs {
		d.add(CodeLimitExceeded, "$.spec.inputs", "a pipeline may declare at most %d inputs", v.limits.MaxInputs)
		return
	}
	for _, name := range sortedKeys(doc.Spec.Inputs) {
		in := doc.Spec.Inputs[name]
		p := childPath("$.spec.inputs", name)
		if !identifierRE.MatchString(name) {
			d.add(CodeInvalidFormat, p, "input names are a lowercase letter followed by lowercase letters, digits, underscores or hyphens, at most 63 characters")
		}
		switch in.Type {
		case TypeString, TypeBoolean, TypeInteger, TypeArtifact:
		default:
			d.add(CodeInvalidValue, p+".type", "an input type must be string, boolean, integer or artifact")
			continue
		}
		if in.Default == nil {
			continue
		}
		switch {
		case in.Type == TypeArtifact:
			d.add(CodeInvalidValue, p+".default", "an artifact input cannot have a default; artifacts are supplied when the run is created")
		case in.Default.Type != in.Type:
			d.add(CodeTypeMismatch, p+".default", "the default must be a %s", in.Type)
		case in.Type == TypeString:
			v.checkString(p+".default", in.Default.Str, d)
			if strings.Contains(in.Default.Str, exprMarker) {
				d.add(CodeInterpolation, p+".default", "defaults are literal values, not expressions")
			}
		case in.Type == TypeInteger && (in.Default.Int < -domain.MaxSafeInteger || in.Default.Int > domain.MaxSafeInteger):
			d.add(CodeInvalidValue, p+".default", "integers must be within +/-%d", int64(domain.MaxSafeInteger))
		}
	}
}

func (v *Validator) checkWhen(i int, s Step, doc Document, d *diagnostics) {
	if s.When == nil {
		return
	}
	p := stepPath(i) + ".when"
	in, ok := doc.Spec.Inputs[s.When.Input]
	switch {
	case !ok:
		d.add(CodeUnknownInput, p+".input", "the condition names an input that is not declared")
	case in.Type == TypeArtifact:
		d.add(CodeInvalidWhen, p+".input", "a condition cannot test an artifact input")
	case s.When.Equals.Type != in.Type:
		d.add(CodeTypeMismatch, p+".equals", "the compared value must be a %s like the input", in.Type)
	case in.Type == TypeString:
		v.checkString(p+".equals", s.When.Equals.Str, d)
		if strings.Contains(s.When.Equals.Str, exprMarker) {
			d.add(CodeInterpolation, p+".equals", "conditions compare literal values, not expressions")
		}
	}
}

func (v *Validator) checkContainer(i int, s Step, r *resolver) {
	d := r.d
	p := stepPath(i)
	switch {
	case s.Image == "":
		d.add(CodeMissingField, p+".image", "a container step needs an image")
	case len(s.Image) > maxImageBytes || !imageRE.MatchString(s.Image):
		d.add(CodeMutableImage, p+".image", "images must be pinned by sha256 digest, for example registry.example/name@sha256:<64 hex digits>")
	}
	if s.Environment != "" {
		if err := domain.EnvironmentID(s.Environment).Validate(); err != nil {
			d.add(CodeInvalidFormat, p+".environment", "the environment must be a valid environment identifier")
		}
	}
	if len(s.Command) == 0 {
		d.add(CodeMissingField, p+".command", "a container step needs a command")
	}
	for k, c := range s.Command {
		cp := p + ".command[" + strconv.Itoa(k) + "]"
		v.checkString(cp, c, d)
		r.noExpression(cp, c)
		v.noSecretLiteral(cp, c, d)
	}
	if len(s.Args) > v.limits.MaxArgs {
		d.add(CodeLimitExceeded, p+".args", "a step may have at most %d arguments", v.limits.MaxArgs)
	}
	for k, a := range s.Args {
		ap := p + ".args[" + strconv.Itoa(k) + "]"
		v.checkString(ap, a, d)
		r.check(refSite{path: ap, consumer: i, allowed: scalarTypes, endpoints: true}, a)
		v.noSecretLiteral(ap, a, d)
	}
	if len(s.Env) > v.limits.MaxEnv {
		d.add(CodeLimitExceeded, p+".env", "a step may have at most %d environment entries", v.limits.MaxEnv)
	}
	for _, name := range sortedKeys(s.Env) {
		ep := childPath(p+".env", name)
		if !envNameRE.MatchString(name) {
			d.add(CodeInvalidFormat, ep, "environment names are letters, digits and underscores, not starting with a digit")
		}
		val := s.Env[name]
		switch {
		case val.Scalar != nil && val.SecretRef != "":
			d.add(CodeInvalidEnv, ep, "an environment entry is either a scalar or a secretRef")
		case val.Scalar == nil && val.SecretRef == "":
			d.add(CodeInvalidEnv, ep, "an environment entry needs a value")
		case val.SecretRef != "":
			if secretRefRE.FindStringSubmatch(val.SecretRef) == nil {
				d.add(CodeInvalidSecretRef, ep+".secretRef", "a secret reference is secret://project/NAME")
			}
		case val.Scalar.Type == TypeString:
			v.checkString(ep, val.Scalar.Str, d)
			r.check(refSite{path: ep, consumer: i, allowed: scalarTypes, endpoints: true}, val.Scalar.Str)
			v.noSecretLiteral(ep, val.Scalar.Str, d)
		case val.Scalar.Type == TypeInteger && (val.Scalar.Int < -domain.MaxSafeInteger || val.Scalar.Int > domain.MaxSafeInteger):
			d.add(CodeInvalidValue, ep, "integers must be within +/-%d", int64(domain.MaxSafeInteger))
		}
	}
	if len(s.SecretFiles) > v.limits.MaxSecretFiles {
		d.add(CodeLimitExceeded, p+".secretFiles", "a step may mount at most %d secret files", v.limits.MaxSecretFiles)
	}
	for _, name := range sortedKeys(s.SecretFiles) {
		fp := childPath(p+".secretFiles", name)
		if !fileNameRE.MatchString(name) {
			d.add(CodeInvalidFormat, fp, "secret file names are letters, digits, dots, underscores and hyphens, starting with a letter or digit")
		}
		if secretRefRE.FindStringSubmatch(s.SecretFiles[name]) == nil {
			d.add(CodeInvalidSecretRef, fp, "a secret reference is secret://project/NAME")
		}
	}
}

// noSecretLiteral rejects a secret reference written where it is not declared:
// secrets are reachable only through env secretRef entries and secretFiles.
func (v *Validator) noSecretLiteral(path, s string, d *diagnostics) {
	if strings.HasPrefix(s, "secret://") {
		d.add(CodeInvalidSecretRef, path, "secret references are only allowed as env secretRef entries and in secretFiles")
	}
}

// checkExecutable validates what container and agent steps share.
func (v *Validator) checkExecutable(i int, s Step, doc Document, r *resolver) {
	d := r.d
	p := stepPath(i)
	v.checkWhen(i, s, doc, d)

	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > v.limits.MaxTimeoutSeconds {
		d.add(CodeInvalidTimeout, p+".timeoutSeconds", "timeoutSeconds must be between 1 and %d", v.limits.MaxTimeoutSeconds)
	}
	v.checkRetry(p, s.Retry, d)

	if s.Workspace != nil {
		v.checkString(p+".workspace.from", s.Workspace.From, d)
		r.noExpression(p+".workspace.from", s.Workspace.From)
	}
	v.checkOutputs(i, s, d)
}

// checkRetry validates a normalized retry policy at the step path p. The
// kodama/v1alpha1 and kodama/v1alpha2 validators share it.
func (v *Validator) checkRetry(p string, rt *Retry, d *diagnostics) {
	if rt.MaxAttempts < 1 || rt.MaxAttempts > v.limits.MaxRetryAttempts {
		d.add(CodeInvalidRetry, p+".retry.maxAttempts", "maxAttempts counts the first attempt and must be between 1 and %d", v.limits.MaxRetryAttempts)
	}
	if rt.BackoffSeconds < 0 || rt.BackoffSeconds > v.limits.MaxBackoffSeconds {
		d.add(CodeInvalidRetry, p+".retry.backoffSeconds", "backoffSeconds must be between 0 and %d", v.limits.MaxBackoffSeconds)
	}
	seen := map[RetryClass]bool{}
	for k, c := range rt.On {
		switch {
		case c != RetryInfrastructure && c != RetryTransient:
			d.add(CodeInvalidRetry, p+".retry.on["+strconv.Itoa(k)+"]", "retry classes are infrastructure and transient; command failures are never retried automatically")
		case seen[c]:
			d.add(CodeInvalidRetry, p+".retry.on["+strconv.Itoa(k)+"]", "the retry class is listed more than once")
		}
		seen[c] = true
	}
	if rt.MaxAttempts > 1 && len(rt.On) == 0 {
		d.add(CodeInvalidRetry, p+".retry.on", "retries need at least one retry class")
	}
}

func (v *Validator) checkOutputs(i int, s Step, d *diagnostics) {
	p := stepPath(i) + ".outputs"
	if len(s.Outputs) > v.limits.MaxOutputs {
		d.add(CodeLimitExceeded, p, "a step may declare at most %d outputs", v.limits.MaxOutputs)
		return
	}
	var paths []string
	for _, name := range sortedKeys(s.Outputs) {
		op := childPath(p, name)
		out := s.Outputs[name]
		if !identifierRE.MatchString(name) {
			d.add(CodeInvalidFormat, op, "output names are a lowercase letter followed by lowercase letters, digits, underscores or hyphens, at most 63 characters")
		}
		if s.IsWriter() && name == ImplicitCommitOutput {
			d.add(CodeReservedOutput, op, "a writer step already provides the implicit commit output")
		}
		switch out.Type {
		case TypeString, TypeBoolean, TypeInteger, TypeOCIArtifact:
			if out.Path != "" {
				d.add(CodeInvalidOutput, op+".path", "only artifact outputs have a path")
			}
		case TypeArtifact:
			if !validOutputPath(out.Path) {
				d.add(CodeInvalidOutput, op+".path", "artifact paths must be clean absolute paths below %s", OutputsRoot)
				continue
			}
			paths = append(paths, out.Path)
		default:
			d.add(CodeInvalidValue, op+".type", "an output type must be string, boolean, integer, artifact or ociArtifact")
		}
	}
	sort.Strings(paths)
	for k := 1; k < len(paths); k++ {
		if paths[k] == paths[k-1] || strings.HasPrefix(paths[k], paths[k-1]+"/") {
			d.add(CodeInvalidOutput, p, "artifact output paths must be distinct and must not nest inside each other")
			break
		}
	}
}

// validOutputPath is the lexical containment rule: a clean absolute path
// strictly below /kodama/outputs. Symlinks and the live filesystem are
// enforced by the step entrypoint, not here.
func validOutputPath(p string) bool {
	if p == "" || len(p) > 1024 || !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\\") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return path.Clean(p) == p && strings.HasPrefix(p, OutputsRoot+"/") && len(p) > len(OutputsRoot)+1
}

// checkGate validates a gate step. Executable-only fields were rejected before
// normalization; what remains are the references.
func (v *Validator) checkGate(i int, s Step, r *resolver) {
	d := r.d
	p := stepPath(i) + ".gate"
	if s.Gate == nil { // normalization always supplies one
		return
	}
	if len(s.Gate.Evidence) > v.limits.MaxEvidence {
		d.add(CodeLimitExceeded, p+".evidence", "a gate may list at most %d evidence references", v.limits.MaxEvidence)
	}
	for k, e := range s.Gate.Evidence {
		ep := p + ".evidence[" + strconv.Itoa(k) + "]"
		v.checkString(ep, e, d)
		r.check(refSite{path: ep, consumer: i, allowed: []ValueType{TypeArtifact}, requireRef: true}, e)
	}
	if s.Gate.DeploymentID != "" {
		v.checkString(p+".deploymentId", s.Gate.DeploymentID, d)
		r.check(refSite{path: p + ".deploymentId", consumer: i, allowed: []ValueType{TypeString}, requireRef: true}, s.Gate.DeploymentID)
	}
}

// checkLookups runs the capability, profile and reference lookups for a
// document that passed every local rule.
func (v *Validator) checkLookups(ctx context.Context, scope domain.Scope, actor Actor, doc Document, d *diagnostics) error {
	enabled := map[StepKind]bool{}
	for i, s := range doc.Spec.Steps {
		ok, seen := enabled[s.Kind]
		if !seen {
			var err error
			ok, err = v.caps.StepKindEnabled(ctx, s.Kind)
			if err != nil {
				return Unavailablef(err, "the step capability lookup failed")
			}
			enabled[s.Kind] = ok
		}
		if !ok {
			d.add(CodeKindDisabled, stepPath(i)+".kind", "%s steps are not enabled in this installation", s.Kind)
		}
	}

	profiles := map[string]error{}
	secrets := map[string]error{}
	environments := map[string]error{}
	for i, s := range doc.Spec.Steps {
		if !enabled[s.Kind] {
			continue
		}
		p := stepPath(i)
		if s.Kind == KindAgent {
			if err := v.lookupProfile(ctx, scope, s.Agent.Profile, profiles); err != nil {
				switch {
				case errors.Is(err, ErrReferenceNotFound):
					d.add(CodeUnknownProfile, p+".agent.profile", "the profile does not exist in this project")
				case errors.Is(err, errMutableProfile):
					d.add(CodeMutableProfile, p+".agent.profile", "the profile reference is not an immutable profile version")
				default:
					return err
				}
			}
		}
		if s.Kind != KindContainer {
			continue
		}
		if s.Environment != "" {
			err := v.lookupReference(environments, "environment\x00"+s.Environment, func() error {
				if v.refs == nil {
					return Unavailablef(nil, "no reference policy is configured")
				}
				return v.refs.CheckEnvironment(ctx, scope, actor, domain.EnvironmentID(s.Environment))
			})
			if err != nil {
				if !isRefusal(err) {
					return err
				}
				d.add(CodeInvalidEnv, p+".environment", "the environment does not exist or cannot be used by this pipeline")
			}
		}
		check := func(path, ref string, use SecretUse) error {
			m := secretRefRE.FindStringSubmatch(ref)
			err := v.lookupReference(secrets, string(use)+"\x00"+m[1], func() error {
				if v.refs == nil {
					return Unavailablef(nil, "no reference policy is configured")
				}
				return v.refs.CheckSecret(ctx, scope, actor, m[1], use)
			})
			if err != nil {
				if !isRefusal(err) {
					return err
				}
				d.add(CodeSecretDenied, path, "the secret does not exist or cannot be used in this way")
			}
			return nil
		}
		for _, name := range sortedKeys(s.Env) {
			if ref := s.Env[name].SecretRef; ref != "" {
				if err := check(childPath(p+".env", name)+".secretRef", ref, SecretUseEnv); err != nil {
					return err
				}
			}
		}
		for _, name := range sortedKeys(s.SecretFiles) {
			if err := check(childPath(p+".secretFiles", name), s.SecretFiles[name], SecretUseFile); err != nil {
				return err
			}
		}
	}
	return nil
}

var errMutableProfile = errors.New("pipeline: profile is not immutable")

func isRefusal(err error) bool {
	return errors.Is(err, ErrReferenceNotFound) || errors.Is(err, ErrReferenceDenied)
}

// lookupReference memoizes one lookup per distinct reference and classifies the
// answer: nil, a refusal, or ErrUnavailable for anything else.
func (v *Validator) lookupReference(memo map[string]error, key string, do func() error) error {
	if err, ok := memo[key]; ok {
		return err
	}
	err := do()
	if err != nil && !isRefusal(err) && !errors.Is(err, ErrUnavailable) && !errors.Is(err, errMutableProfile) {
		err = Unavailablef(err, "the reference lookup failed")
	}
	memo[key] = err
	return err
}

func (v *Validator) lookupProfile(ctx context.Context, scope domain.Scope, name string, memo map[string]error) error {
	return v.lookupReference(memo, name, func() error {
		if v.prof == nil {
			return Unavailablef(nil, "no profile lookup is configured")
		}
		info, err := v.prof.LookupProfile(ctx, scope, name)
		if err != nil {
			return err
		}
		if !info.Immutable {
			return errMutableProfile
		}
		return nil
	})
}
