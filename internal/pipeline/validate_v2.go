package pipeline

import (
	"context"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
	"github.com/optimiware/kodama/internal/domain"
)

// kodama/v1alpha2 bounds. The prose contract is authoritative; the JSON schema
// states the same numbers and the tests require both to agree.
const (
	minRunSeconds         = 60
	maxRunSeconds         = 604800
	minRunWorkspaceMiB    = 64
	maxRunWorkspaceMiB    = 1048576
	maxServices           = 8
	maxEdges              = 32
	maxEgress             = 8
	maxServicePorts       = 8
	maxServiceStorage     = 4
	maxServiceCommand     = 64
	maxServiceArgs        = 256
	maxServiceEnv         = 64
	maxServiceSecretFiles = 16
	maxStoragePathChars   = 512
	maxProbePathChars     = 512
	maxProbeExecArgs      = 16
	maxProbeExecChars     = 1024
)

var (
	serviceNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`)
	portNameRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,14}$`)
	egressNameRE  = nameRE
	namespaceRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,126}[a-z0-9])?$`)
)

// storageForbiddenRoots are the paths a service volume may be neither at nor
// under.
var storageForbiddenRoots = []string{"/workspace", "/kodama", "/proc", "/sys", "/dev", "/run/secrets"}

// validateV2 is the kodama/v1alpha2 counterpart of Validate's body. The format
// capability gate comes first: a disabled installation learns nothing about the
// document beyond the format name, and runs no lookup.
func (v *Validator) validateV2(ctx context.Context, scope domain.Scope, actor Actor, doc Document) (Validated, error) {
	var d diagnostics
	if !v.v2 && !v.v3 {
		d.add(CodeFormatDisabled, "$.apiVersion", "the pipeline format is not enabled in this installation")
		return Validated{}, d.err(StageSemantic)
	}
	v.checkForbiddenV2(doc, &d)
	n := normalize(doc)
	v.checkDocumentV2(n, &d)
	if d.any() {
		d.constantMessages()
		return Validated{}, d.err(StageSemantic)
	}
	if err := v.checkLookups(ctx, scope, actor, n, &d); err != nil {
		return Validated{}, err
	}
	if err := v.checkServiceLookups(ctx, scope, actor, n, &d); err != nil {
		return Validated{}, err
	}
	if d.any() {
		d.constantMessages()
		return Validated{}, d.err(StageSemantic)
	}
	return newValidated(n)
}

// checkForbiddenV2 finds fields set on a step of a kind that does not have them,
// on the document as given (normalization drops them).
func (v *Validator) checkForbiddenV2(doc Document, d *diagnostics) {
	for i, s := range doc.Spec.Steps {
		p := stepPath(i)
		set := map[string]bool{
			"workspace": s.Workspace != nil, "environment": s.Environment != "", "image": s.Image != "",
			"command": s.Command != nil, "args": s.Args != nil, "env": s.Env != nil, "secretFiles": s.SecretFiles != nil,
			"timeoutSeconds": s.TimeoutSeconds != 0, "retry": s.Retry != nil, "outputs": s.Outputs != nil,
			"agent": s.Agent != nil, "gate": s.Gate != nil, "when": s.When != nil, "egress": s.Egress != nil,
			"state": s.State != nil, "artifactFiles": s.ArtifactFiles != nil,
		}
		var forbidden []string
		switch s.Kind {
		case KindGate:
			forbidden = []string{"when", "workspace", "environment", "image", "command", "args", "env", "secretFiles", "timeoutSeconds", "retry", "outputs", "agent", "egress", "state", "artifactFiles"}
		case KindAgent:
			forbidden = []string{"environment", "image", "command", "args", "env", "secretFiles", "gate", "egress"}
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
				d.add(CodeUnknownField, p+"."+f, "the field is not part of this step kind")
			}
		}
		if s.Workspace != nil && s.Workspace.From != "" {
			d.add(CodeWorkspaceFromRemoved, p+".workspace.from", "workspace.from is not part of this pipeline format")
		}
	}
}

func svcPath(name string) string { return childPath("$.spec.services", name) }

// checkDocumentV2 applies the structural and semantic rules of a normalized
// kodama/v1alpha2 document. Shape findings (checkShapeV2) end the pass: the
// semantic rules assume sane services, ports and edges.
func (v *Validator) checkDocumentV2(doc Document, d *diagnostics) {
	if !doc.IsShared() {
		d.add(CodeInvalidValue, "$.apiVersion", "apiVersion must be %s", pipelinev1alpha2.APIVersion)
		return
	}
	if doc.IsV1Alpha3() && doc.APIVersion != pipelinev1alpha3.APIVersion {
		d.add(CodeInvalidValue, "$.apiVersion", "apiVersion must be %s", pipelinev1alpha3.APIVersion)
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
	before := len(d.list)
	checkShapeV2(doc, d)
	if len(d.list) > before || d.truncated {
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
			d.add(CodeReservedID, p+".id", "run is a reserved identifier")
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
	r := &resolver{doc: doc, graph: g, d: d, v2: true}
	for i, s := range steps {
		switch s.Kind {
		case KindContainer:
			v.checkContainer(i, s, r)
			v.checkExecutableV2(i, s, doc, r)
		case KindAgent:
			if s.Agent == nil || !profileRE.MatchString(s.Agent.Profile) {
				d.add(CodeInvalidFormat, stepPath(i)+".agent.profile", "an agent step must name its profile")
			}
			v.checkExecutableV2(i, s, doc, r)
		case KindGate:
			v.checkGate(i, s, r)
		}
	}
	v.checkServicesV2(doc, r)
}

// checkExecutableV2 validates what container and agent steps share in
// kodama/v1alpha2: the step deadline is bounded by the run deadline instead of
// the kodama/v1alpha1 installation ceiling, and workspace has only an access.
func (v *Validator) checkExecutableV2(i int, s Step, doc Document, r *resolver) {
	d := r.d
	p := stepPath(i)
	v.checkWhen(i, s, doc, d)
	switch {
	case s.TimeoutSeconds < 1 || s.TimeoutSeconds > maxRunSeconds:
		d.add(CodeInvalidTimeout, p+".timeoutSeconds", "timeoutSeconds must be between 1 and %d", maxRunSeconds)
	case s.TimeoutSeconds > doc.Spec.TimeoutSeconds:
		d.add(CodeStepTimeoutExceedsRun, p+".timeoutSeconds", "a step timeout must not exceed spec.timeoutSeconds")
	}
	v.checkRetry(p, s.Retry, d)
	if s.Workspace != nil {
		switch s.Workspace.Access {
		case AccessNone, AccessRead, AccessWrite:
		default:
			d.add(CodeInvalidValue, p+".workspace.access", "workspace access must be none, read or write")
		}
	}
	v.checkOutputs(i, s, d)
	for _, name := range sortedKeys(s.Outputs) {
		// Output paths are not expression sites: the marker anywhere else is an
		// interpolation, in kodama/v1alpha2 as in the expression rules.
		r.noExpression(childPath(p+".outputs", name)+".path", s.Outputs[name].Path)
	}
	if doc.IsV1Alpha3() {
		if len(s.State) > 8 {
			d.add(CodeLimitExceeded, p+".state", "at most 8 state bindings are allowed")
		}
		for _, name := range sortedKeys(s.State) {
			b := s.State[name]
			bp := childPath(p+".state", name)
			if !nameRE.MatchString(name) {
				d.add(CodeInvalidFormat, bp, "a state binding name must be a lowercase letter followed by lowercase letters, digits or hyphens")
			}
			if !namespaceRE.MatchString(b.Namespace) {
				d.add(CodeInvalidFormat, bp+".namespace", "the state namespace must be a lowercase DNS label")
			}
			if b.Prefix != "" && (strings.HasPrefix(b.Prefix, "/") || strings.Contains(b.Prefix, "..") || strings.Contains(b.Prefix, "//")) {
				d.add(CodeInvalidValue, bp+".prefix", "the state prefix must be a normalized relative path")
			}
			if b.Access != StateRead && b.Access != StateReadWrite {
				d.add(CodeInvalidValue, bp+".access", "state access must be read or read-write")
			}
		}
		if len(s.ArtifactFiles) > 16 {
			d.add(CodeLimitExceeded, p+".artifactFiles", "at most 16 artifact file bindings are allowed")
		}
		for _, name := range sortedKeys(s.ArtifactFiles) {
			if !fileNameRE.MatchString(name) {
				d.add(CodeInvalidFormat, childPath(p+".artifactFiles", name), "an artifact file alias has an invalid format")
				continue
			}
			r.check(refSite{path: childPath(p+".artifactFiles", name), consumer: i, allowed: []ValueType{TypeArtifact}, requireRef: true}, s.ArtifactFiles[name])
		}
	}
}

// checkShapeV2 enforces the structural rules of a kodama/v1alpha2 document that
// need no expression resolution: bounds, ports, probes, storage, edges and
// egress names. The parser runs it after the schema (syntax stage) and Validate
// runs it again for documents built in code. The doc must be normalized.
func checkShapeV2(doc Document, d *diagnostics) {
	sp := doc.Spec
	if sp.TimeoutSeconds < minRunSeconds || sp.TimeoutSeconds > maxRunSeconds {
		d.add(CodeInvalidTimeout, "$.spec.timeoutSeconds", "spec.timeoutSeconds must be between %d and %d", minRunSeconds, maxRunSeconds)
	}
	if rw := sp.RunWorkspace; rw != nil && (rw.SizeMiB < minRunWorkspaceMiB || rw.SizeMiB > maxRunWorkspaceMiB) {
		d.add(CodeInvalidResources, "$.spec.runWorkspace.sizeMiB", "runWorkspace.sizeMiB must be between %d and %d", minRunWorkspaceMiB, maxRunWorkspaceMiB)
	}
	if len(sp.Services) > maxServices {
		d.add(CodeLimitExceeded, "$.spec.services", "a pipeline may declare at most %d services", maxServices)
		return
	}
	builders := 0
	for _, name := range sortedKeys(sp.Services) {
		p := svcPath(name)
		if !serviceNameRE.MatchString(name) {
			d.add(CodeInvalidFormat, p, "service names are a lowercase letter followed by lowercase letters, digits or hyphens, at most 30 characters")
		}
		checkServiceShape(p, sp.Services[name], &builders, d)
	}
	checkEdges(sp, d)
	for i, s := range doc.Spec.Steps {
		if s.Kind == KindContainer {
			checkEgressNames(stepPath(i)+".egress", s.Egress, d)
			for _, name := range sortedKeys(s.Env) {
				if sc := s.Env[name].Scalar; sc != nil {
					checkScalarTypeV2(childPath(stepPath(i)+".env", name), sc, d)
				}
			}
		}
	}
}

func checkEgressNames(p string, names []string, d *diagnostics) {
	if len(names) > maxEgress {
		d.add(CodeInvalidEgress, p, "at most %d egress profile names are allowed", maxEgress)
	}
	seen := map[string]bool{}
	for k, n := range names {
		ep := p + "[" + strconv.Itoa(k) + "]"
		switch {
		case !egressNameRE.MatchString(n):
			d.add(CodeInvalidEgress, ep, "an egress entry names a profile; it is never a host, CIDR or port")
		case seen[n]:
			d.add(CodeInvalidEgress, ep, "the egress profile is listed more than once")
		}
		seen[n] = true
	}
}

func checkServiceShape(p string, s RunService, builders *int, d *diagnostics) {
	switch s.Role {
	case RoleGeneral:
	case RoleBuilder:
		*builders++
		if *builders > 1 {
			d.add(CodeInvalidRole, p+".role", "at most one builder service is allowed")
		}
	default:
		d.add(CodeInvalidRole, p+".role", "the service role must be general or builder")
	}
	if s.Command != nil && len(s.Command) == 0 {
		d.add(CodeInvalidValue, p+".command", "command must have at least one element; omit it to run the image defaults")
	}
	if len(s.Command) > maxServiceCommand {
		d.add(CodeInvalidValue, p+".command", "command may have at most %d elements", maxServiceCommand)
	}
	if len(s.Args) > maxServiceArgs {
		d.add(CodeInvalidValue, p+".args", "args may have at most %d elements", maxServiceArgs)
	}
	if len(s.Env) > maxServiceEnv {
		d.add(CodeLimitExceeded, p+".env", "a service may have at most %d environment entries", maxServiceEnv)
	}
	if len(s.SecretFiles) > maxServiceSecretFiles {
		d.add(CodeLimitExceeded, p+".secretFiles", "a service may mount at most %d secret files", maxServiceSecretFiles)
	}
	checkPorts(p, s, d)
	checkResources(p, s.Resources, d)
	checkStorage(p, s.Storage, d)
	checkReadiness(p, s, d)
	checkEgressNames(p+".egress", s.Egress, d)
}

func checkPorts(p string, s RunService, d *diagnostics) {
	if len(s.Ports) == 0 || len(s.Ports) > maxServicePorts {
		d.add(CodeInvalidValue, p+".ports", "a service declares between 1 and %d ports", maxServicePorts)
	}
	byNumber := map[int64]bool{}
	for _, name := range sortedKeys(s.Ports) {
		pp := childPath(p+".ports", name)
		port := s.Ports[name]
		if !portNameRE.MatchString(name) {
			d.add(CodeInvalidFormat, pp, "port names are a lowercase letter followed by lowercase letters, digits or hyphens, at most 15 characters")
		}
		switch {
		case port.Port < 1 || port.Port > 65535:
			d.add(CodeInvalidValue, pp+".port", "a port number is between 1 and 65535")
		case byNumber[port.Port]:
			d.add(CodeDuplicatePort, pp+".port", "port numbers are unique per service")
		}
		byNumber[port.Port] = true
		if port.Protocol != ProtocolTCP {
			d.add(CodeInvalidValue, pp+".protocol", "the only protocol is TCP")
		}
	}
}

func checkResources(p string, r ServiceResources, d *diagnostics) {
	rng := func(name string, v, lo, hi int64) {
		if v < lo || v > hi {
			d.add(CodeInvalidResources, p+".resources."+name, "%s must be between %d and %d", name, lo, hi)
		}
	}
	rng("cpuMillis", r.CPUMillis, 50, 16000)
	rng("memoryMiB", r.MemoryMiB, 16, 65536)
	rng("ephemeralMiB", r.EphemeralMiB, 16, 262144)
	if r.ShmMiB != nil {
		rng("shmMiB", *r.ShmMiB, 0, 4096)
	}
}

// validStoragePath is the lexical rule for a service volume path: absolute,
// normalized, no trailing slash, no control characters, not the root and none at
// or under a reserved platform path.
func validStoragePath(p string) bool {
	if p == "" || utf8.RuneCountInString(p) > maxStoragePathChars || !utf8.ValidString(p) || !strings.HasPrefix(p, "/") || p == "/" || strings.HasSuffix(p, "/") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	if path.Clean(p) != p {
		return false
	}
	for _, f := range storageForbiddenRoots {
		if p == f || strings.HasPrefix(p, f+"/") {
			return false
		}
	}
	return true
}

func checkStorage(p string, storage []ServiceStorage, d *diagnostics) {
	sp := p + ".storage"
	if len(storage) > maxServiceStorage {
		d.add(CodeInvalidStorage, sp, "a service may declare at most %d volumes", maxServiceStorage)
	}
	var valid []string
	for k, st := range storage {
		ep := sp + "[" + strconv.Itoa(k) + "]"
		if !validStoragePath(st.Path) {
			d.add(CodeInvalidStorage, ep+".path", "a storage path is an absolute normalized path outside /workspace, /kodama, /proc, /sys, /dev and /run/secrets")
		} else {
			valid = append(valid, st.Path)
		}
		if st.SizeMiB < 1 || st.SizeMiB > 262144 {
			d.add(CodeInvalidStorage, ep+".sizeMiB", "sizeMiB must be between 1 and 262144")
		}
	}
	if storagePathsOverlap(valid) {
		d.add(CodeInvalidStorage, sp, "storage paths must be distinct and must not overlap")
	}
}

// storagePathsOverlap compares every pair of valid, normalized paths on path
// component boundaries: equal, or one an ancestor of the other. Sorting and
// comparing neighbors is not enough, because an unrelated path can sort between
// an ancestor and its descendant (/var/data, /var/data-other, /var/data/sub).
func storagePathsOverlap(paths []string) bool {
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			a, b := paths[i], paths[j]
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return true
			}
		}
	}
	return false
}

// checkScalarTypeV2 rejects a typed Scalar whose discriminator is not string,
// boolean or integer. The canonical serializer renders an unknown type as a
// string, so letting one through would register a document whose canonical form
// is not the document that was checked.
func checkScalarTypeV2(path string, sc *Scalar, d *diagnostics) bool {
	switch sc.Type {
	case TypeString, TypeBoolean, TypeInteger:
		return true
	}
	d.add(CodeInvalidType, path, "a scalar is a string, boolean or integer")
	return false
}

func checkReadiness(p string, s RunService, d *diagnostics) {
	rp := p + ".readiness"
	r := s.Readiness
	kinds := 0
	if r.TCP != "" {
		kinds++
	}
	if r.HTTP != nil {
		kinds++
	}
	if r.Exec != nil {
		kinds++
	}
	if kinds != 1 {
		d.add(CodeInvalidProbe, rp, "readiness declares exactly one of tcp, http or exec")
	}
	if r.TCP != "" {
		if _, ok := s.Ports[r.TCP]; !ok {
			d.add(CodeUnknownPort, rp+".tcp", "the probe names a port the service does not declare")
		}
	}
	if h := r.HTTP; h != nil {
		if _, ok := s.Ports[h.Port]; !ok {
			d.add(CodeUnknownPort, rp+".http.port", "the probe names a port the service does not declare")
		}
		if !validProbePath(h.Path) {
			d.add(CodeInvalidProbe, rp+".http.path", "an http probe path begins with / and has no whitespace, control characters or fragment")
		}
	}
	if r.Exec != nil {
		if len(r.Exec) < 1 || len(r.Exec) > maxProbeExecArgs {
			d.add(CodeInvalidProbe, rp+".exec", "an exec probe has between 1 and %d elements", maxProbeExecArgs)
		}
		for k, a := range r.Exec {
			if utf8.RuneCountInString(a) > maxProbeExecChars {
				d.add(CodeInvalidProbe, rp+".exec["+strconv.Itoa(k)+"]", "an exec probe element has at most %d characters", maxProbeExecChars)
			}
		}
	}
	rng := func(name string, v, lo, hi int64) {
		if v < lo || v > hi {
			d.add(CodeInvalidProbe, rp+"."+name, "%s must be between %d and %d", name, lo, hi)
		}
	}
	rng("timeoutSeconds", r.TimeoutSeconds, 5, 600)
	rng("periodSeconds", r.PeriodSeconds, 1, 60)
	rng("probeTimeoutSeconds", r.ProbeTimeoutSeconds, 1, 60)
	rng("lossSeconds", r.LossSeconds, 5, 600)
	if r.ProbeTimeoutSeconds > r.TimeoutSeconds {
		d.add(CodeInvalidProbe, rp+".probeTimeoutSeconds", "probeTimeoutSeconds must not exceed timeoutSeconds")
	}
}

func validProbePath(p string) bool {
	if p == "" || utf8.RuneCountInString(p) > maxProbePathChars || !utf8.ValidString(p) || p[0] != '/' {
		return false
	}
	for _, r := range p {
		if r == '#' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Z, r) {
			return false
		}
	}
	return true
}

func checkEdges(sp Spec, d *diagnostics) {
	if sp.Network == nil {
		return
	}
	edges := sp.Network.Edges
	if len(edges) > maxEdges {
		d.add(CodeInvalidEdge, "$.spec.network.edges", "at most %d edges may be declared", maxEdges)
	}
	type pair struct{ from, to string }
	seen := map[pair]bool{}
	for i, e := range edges {
		ep := "$.spec.network.edges[" + strconv.Itoa(i) + "]"
		_, fromOK := sp.Services[e.From]
		to, toOK := sp.Services[e.To]
		if !fromOK {
			d.add(CodeUnknownService, ep+".from", "the edge names a service that is not declared")
		}
		if !toOK {
			d.add(CodeUnknownService, ep+".to", "the edge names a service that is not declared")
		}
		if fromOK && toOK && e.From == e.To {
			d.add(CodeInvalidEdge, ep, "an edge connects two different services")
		}
		if len(e.Ports) == 0 {
			d.add(CodeInvalidEdge, ep+".ports", "an edge lists at least one port of the target service")
		}
		ports := map[string]bool{}
		for k, name := range e.Ports {
			pp := ep + ".ports[" + strconv.Itoa(k) + "]"
			if ports[name] {
				d.add(CodeInvalidEdge, pp, "the edge lists the port more than once")
			}
			ports[name] = true
			if toOK {
				if _, ok := to.Ports[name]; !ok {
					d.add(CodeUnknownPort, pp, "the edge names a port the target service does not declare")
				}
			}
		}
		k := pair{e.From, e.To}
		if seen[k] {
			d.add(CodeInvalidEdge, ep, "a (from, to) pair is declared once")
		}
		seen[k] = true
	}
}

// edgeIndex maps (from, to) to the ports of the declared edge.
func edgeIndex(sp Spec) map[[2]string]map[string]bool {
	out := map[[2]string]map[string]bool{}
	if sp.Network == nil {
		return out
	}
	for _, e := range sp.Network.Edges {
		ports := map[string]bool{}
		for _, p := range e.Ports {
			ports[p] = true
		}
		out[[2]string{e.From, e.To}] = ports
	}
	return out
}

// checkServicesV2 applies the semantic rules of the services: images, argv and
// environment strings, expressions, secret bindings and the edge rule.
func (v *Validator) checkServicesV2(doc Document, r *resolver) {
	d := r.d
	edges := edgeIndex(doc.Spec)
	for _, name := range sortedKeys(doc.Spec.Services) {
		s := doc.Spec.Services[name]
		p := svcPath(name)
		if len(s.Image) > maxImageBytes || !imageRE.MatchString(s.Image) {
			d.add(CodeMutableImage, p+".image", "images must be pinned by sha256 digest, for example registry.example/name@sha256:<64 hex digits>")
		}
		for k, c := range s.Command {
			cp := p + ".command[" + strconv.Itoa(k) + "]"
			v.checkString(cp, c, d)
			r.noExpression(cp, c)
			v.noSecretLiteral(cp, c, d)
		}
		for k, a := range s.Args {
			ap := p + ".args[" + strconv.Itoa(k) + "]"
			v.checkString(ap, a, d)
			r.noExpression(ap, a)
			v.noSecretLiteral(ap, a, d)
		}
		if h := s.Readiness.HTTP; h != nil {
			r.noExpression(p+".readiness.http.path", h.Path)
		}
		for k, st := range s.Storage {
			r.noExpression(p+".storage["+strconv.Itoa(k)+"].path", st.Path)
		}
		for k, a := range s.Readiness.Exec {
			ap := p + ".readiness.exec[" + strconv.Itoa(k) + "]"
			v.checkString(ap, a, d)
			r.noExpression(ap, a)
			v.noSecretLiteral(ap, a, d)
		}
		for _, en := range sortedKeys(s.Env) {
			ep := childPath(p+".env", en)
			if !envNameRE.MatchString(en) {
				d.add(CodeInvalidFormat, ep, "environment names are letters, digits and underscores, not starting with a digit")
			}
			val := s.Env[en]
			switch {
			case val.Scalar != nil && val.SecretRef != "":
				d.add(CodeInvalidEnv, ep, "an environment entry is either a scalar or a secretRef")
			case val.Scalar == nil && val.SecretRef == "":
				d.add(CodeInvalidEnv, ep, "an environment entry needs a value")
			case val.SecretRef != "":
				if secretRefRE.FindStringSubmatch(val.SecretRef) == nil {
					d.add(CodeInvalidSecretRef, ep+".secretRef", "a secret reference is secret://project/NAME")
				}
			case !checkScalarTypeV2(ep, val.Scalar, d):
			case val.Scalar.Type == TypeString:
				v.checkString(ep, val.Scalar.Str, d)
				v.noSecretLiteral(ep, val.Scalar.Str, d)
				v.checkServiceEnvRef(doc, edges, name, ep, val.Scalar.Str, d)
			case val.Scalar.Type == TypeInteger && (val.Scalar.Int < -domain.MaxSafeInteger || val.Scalar.Int > domain.MaxSafeInteger):
				d.add(CodeInvalidValue, ep, "integers must be within +/-%d", int64(domain.MaxSafeInteger))
			}
		}
		for _, fn := range sortedKeys(s.SecretFiles) {
			fp := childPath(p+".secretFiles", fn)
			if !fileNameRE.MatchString(fn) {
				d.add(CodeInvalidFormat, fp, "secret file names are letters, digits, dots, underscores and hyphens, starting with a letter or digit")
			}
			if secretRefRE.FindStringSubmatch(s.SecretFiles[fn]) == nil {
				d.add(CodeInvalidSecretRef, fp, "a secret reference is secret://project/NAME")
			}
		}
		if s.HasSecrets() && s.Command == nil {
			d.add(CodeServiceSecretRequiresCommand, p+".command", "a service with a secret binding must declare command")
		}
	}
}

// checkServiceEnvRef validates the string value of a service environment entry:
// a literal, a whole-scalar input reference, or an endpoint reference of another
// service this service has a declared edge to. Step references are refused.
func (v *Validator) checkServiceEnvRef(doc Document, edges map[[2]string]map[string]bool, self, path, s string, d *diagnostics) {
	if !strings.Contains(s, exprMarker) {
		return
	}
	if ep, ok := parseEndpoint(s); ok {
		svc, known := doc.Spec.Services[ep.service]
		switch {
		case ep.service == self:
			d.add(CodeServiceSelfReference, path, "a service cannot reference its own endpoint")
		case !known:
			d.add(CodeUnknownService, path, "the referenced service is not declared")
		default:
			if !ep.isHost() {
				if _, ok := svc.Ports[ep.port]; !ok {
					d.add(CodeUnknownPort, path, "the referenced port is not declared by the service")
					return
				}
			}
			ports, edge := edges[[2]string{self, ep.service}]
			if !edge || (!ep.isHost() && !ports[ep.port]) {
				d.add(CodeEdgeUndeclared, path, "the service has no declared network edge to the referenced endpoint")
			}
		}
		return
	}
	ref, isExpr, valid := parseReference(s)
	switch {
	case !isExpr || !valid:
		d.add(CodeInterpolation, path, "service environment values accept only a whole-scalar input or endpoint reference, never interpolation")
	case !ref.isInput():
		d.add(CodeServiceStepReference, path, "a service cannot reference a step value")
	default:
		in, ok := doc.Spec.Inputs[ref.input]
		switch {
		case !ok:
			d.add(CodeUnknownInput, path, "the input is not declared")
		case in.Type == TypeArtifact:
			d.add(CodeTypeMismatch, path, "the referenced value has type artifact, which this field does not accept")
		}
	}
}

// checkServiceLookups runs the secret reference policy for service bindings.
// Like checkLookups it runs only for a document that passed every local rule.
func (v *Validator) checkServiceLookups(ctx context.Context, scope domain.Scope, actor Actor, doc Document, d *diagnostics) error {
	secrets := map[string]error{}
	check := func(p, ref string, use SecretUse) error {
		m := secretRefRE.FindStringSubmatch(ref)
		err := v.lookupReference(secrets, "service\x00"+string(use)+"\x00"+m[1], func() error {
			if v.refs == nil {
				return Unavailablef(nil, "no reference policy is configured")
			}
			return v.refs.CheckSecret(ctx, scope, actor, m[1], use)
		})
		if err != nil {
			if !isRefusal(err) {
				return err
			}
			d.add(CodeSecretDenied, p, "the secret does not exist or cannot be used in this way")
		}
		return nil
	}
	for _, name := range sortedKeys(doc.Spec.Services) {
		s := doc.Spec.Services[name]
		p := svcPath(name)
		for _, en := range sortedKeys(s.Env) {
			if ref := s.Env[en].SecretRef; ref != "" {
				if err := check(childPath(p+".env", en)+".secretRef", ref, SecretUseEnv); err != nil {
					return err
				}
			}
		}
		for _, fn := range sortedKeys(s.SecretFiles) {
			if err := check(childPath(p+".secretFiles", fn), s.SecretFiles[fn], SecretUseFile); err != nil {
				return err
			}
		}
	}
	return nil
}
