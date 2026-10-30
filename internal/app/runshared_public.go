package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/logs"
	logshttp "github.com/optimiware/kodama/internal/logs/http"
	"github.com/optimiware/kodama/internal/secrets"
)

// The public face of the run_shared model: the additive members of a run read
// (`runModel`, `deadlineAt`, `executionOrder`, `effectiveAccess`, `resources`,
// `cleanupReason`) and the bounded log of a preparation or service
// (`GET …/runs/{id}/logs?resource=preparation|service.NAME`). Both are served
// only when run_shared is composed, under the same authenticate → throttle →
// authorize chain as every public route and with current authorization on every
// call: the run view is added to the module's own authorized 200 answer from
// committed state, and the resource log runs on a second router built from the
// same authenticator and rule table. The execution and log HTTP modules stay
// unchanged, so an installation without run_shared serves exactly what it
// served before.

// SharedPublicInputs are the collaborators of the run_shared public routes.
type SharedPublicInputs struct {
	Log           *slog.Logger
	Authenticator httpapi.Authenticator
	Authority     identity.Authority
	// Reader authorizes the log read against current membership (run:read and
	// evidence:read), as the attempt log route does.
	Reader logs.Authority
	// Runs reads the committed run; Resources reads the bounded tail of a
	// preparation or service of the run's own backend.
	Runs      sharedRuns
	Resources execution.RunResources
	// Secrets supplies the values to redact: every secret the run disclosed to a
	// step or a service.
	Secrets   secrets.VersionSource
	RateLimit int
	Logs      LogsTuning
}

type sharedRuns interface {
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// SharedPublic decorates the public handler of a run_shared composition.
type SharedPublic struct {
	log       *slog.Logger
	runs      sharedRuns
	resources execution.RunResources
	reader    logs.Authority
	secrets   secrets.VersionSource
	limits    LogsTuning
	logRouter *httpapi.Router
}

// NewSharedPublic requires every collaborator: there is no permissive default.
func NewSharedPublic(in SharedPublicInputs) (*SharedPublic, error) {
	switch {
	case in.Authenticator == nil, in.Authority == nil, in.Reader == nil, in.Runs == nil, in.Resources == nil, in.Secrets == nil:
		return nil, errors.New("app: the run_shared public routes need an authenticator, an authority, a log reader, the run store, the resource port and the secret versions")
	case in.Logs.MaxBytes <= 0 || in.Logs.MaxLines <= 0:
		return nil, errors.New("app: the run_shared log bounds must be positive")
	}
	log := in.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	authz, err := identityhttp.NewAuthorizer(in.Authority, coreRules())
	if err != nil {
		return nil, err
	}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: in.Authenticator, Authorizer: authz, RateLimiter: newRateLimiter(in.RateLimit, nil), Prefix: APIPrefix, Logger: log,
	})
	if err != nil {
		return nil, err
	}
	s := &SharedPublic{log: log, runs: in.Runs, resources: in.Resources, reader: in.Reader, secrets: in.Secrets, limits: in.Logs, logRouter: router}
	if err := router.Handle(httpapi.Route{
		Method: http.MethodGet, Path: "/tenants/{tenantId}/projects/{projectId}/runs/{id}/logs", Action: logshttp.ActionLogs,
		ResourceKind: logshttp.ResourceRun, ResourceParam: "id", Handler: s.resourceLogs,
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// Wrap returns the public handler with the run_shared routes in front. Only an
// exact GET of the run or its logs is intercepted; every other request, a
// malformed path included, reaches the unchanged router.
func (s *SharedPublic) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if segs, ok := publicRunPath(r.URL.Path); ok {
				switch {
				case len(segs) == 6:
					s.serveRun(w, r, next, segs)
					return
				case len(segs) == 7 && segs[6] == "logs" && r.URL.Query().Has("resource"):
					s.logRouter.ServeHTTP(w, r)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// publicRunPath splits /api/v1/tenants/T/projects/P/runs/ID[/logs] into its
// segments, or reports false for any other path (an escaped or empty segment
// included).
func publicRunPath(path string) ([]string, bool) {
	rest, ok := strings.CutPrefix(path, APIPrefix+"/")
	if !ok {
		return nil, false
	}
	segs := strings.Split(rest, "/")
	if (len(segs) != 6 && len(segs) != 7) || segs[0] != "tenants" || segs[2] != "projects" || segs[4] != "runs" {
		return nil, false
	}
	for _, s := range segs {
		if s == "" {
			return nil, false
		}
	}
	return segs, true
}

// ---- the run view --------------------------------------------------------------

// bufferedResponse holds the module's answer until the view is added.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) replay(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.body.Bytes())
}

// resourceViewDTO is the safe projection of one run resource.
type resourceViewDTO struct {
	Kind      string  `json:"kind"`
	Key       string  `json:"key"`
	Phase     string  `json:"phase"`
	Reason    *string `json:"reason"`
	ReadyAt   *string `json:"readyAt"`
	StoppedAt *string `json:"stoppedAt"`
	RemovedAt *string `json:"removedAt"`
}

func stamp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// answeredVersion is the run version in the module's answer (-1 when absent).
func answeredVersion(body []byte) int64 {
	var v struct {
		Version *int64 `json:"version"`
	}
	if json.Unmarshal(body, &v) != nil || v.Version == nil {
		return -1
	}
	return *v.Version
}

// sharedMembers are the members added to the run of a run_shared run.
func sharedMembers(v *execution.SharedView) (map[string]any, error) {
	resources := make([]resourceViewDTO, 0, len(v.Resources))
	for _, r := range v.Resources {
		d := resourceViewDTO{Kind: string(r.Kind), Key: r.Key, Phase: string(r.Phase), ReadyAt: stamp(r.ReadyAt), StoppedAt: stamp(r.StoppedAt), RemovedAt: stamp(r.RemovedAt)}
		if r.Reason != "" {
			reason := string(r.Reason)
			d.Reason = &reason
		}
		resources = append(resources, d)
	}
	access := make(map[string]string, len(v.Access))
	for k, a := range v.Access {
		access[k] = string(a)
	}
	out := map[string]any{
		"runModel": v.RunModel, "deadlineAt": stamp(v.DeadlineAt), "executionOrder": append([]string{}, v.ExecutionOrder...),
		"effectiveAccess": access, "resources": resources,
	}
	if v.CleanupReason != "" {
		out["cleanupReason"] = string(v.CleanupReason)
	} else {
		out["cleanupReason"] = nil
	}
	return out, nil
}

// runReadAttempts bounds how often a run read is repeated when the run moved
// between the module's answer and the committed state read after it.
const runReadAttempts = 3

func (s *SharedPublic) serveRun(w http.ResponseWriter, r *http.Request, next http.Handler, segs []string) {
	scope := domain.Scope{TenantID: domain.TenantID(segs[1]), ProjectID: domain.ProjectID(segs[3])}
	var (
		rec *bufferedResponse
		agg *execution.Aggregate
	)
	// The module authorized and answered; what follows is committed state of the
	// same run. A run that cannot be read now is not answered with a view that
	// silently lacks its resources. The answer's status, version and ETag (the
	// module's read) and the resources and deadline (this read) must be of ONE
	// version of the run: the run row's version only grows, so equal versions
	// mean it did not move between the two reads. When it did, both are read
	// again; a run that keeps moving is not answered with a mixed view.
	for attempt := 0; ; attempt++ {
		rec = &bufferedResponse{header: http.Header{}}
		next.ServeHTTP(rec, r)
		if rec.status != http.StatusOK || !strings.HasPrefix(rec.header.Get("Content-Type"), "application/json") {
			rec.replay(w)
			return
		}
		var err error
		agg, err = s.runs.Get(r.Context(), scope, domain.RunID(segs[5]))
		switch {
		case err == nil && agg != nil && agg.Run != nil:
		case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
			httpapi.WriteError(w, r, httpapi.NotFoundError())
			return
		default:
			httpapi.WriteError(w, r, httpapi.UnavailableError(time.Second, err))
			return
		}
		if agg.SharedView() == nil {
			// Not a run_shared run: answered exactly as the module answered it.
			rec.replay(w)
			return
		}
		if answeredVersion(rec.body.Bytes()) == int64(agg.Run.Version) {
			break
		}
		if attempt+1 >= runReadAttempts {
			httpapi.WriteError(w, r, httpapi.UnavailableError(time.Second, errors.New("the run moved while it was read")))
			return
		}
	}
	view := agg.SharedView()
	if view == nil {
		rec.replay(w)
		return
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rec.body.Bytes(), &doc); err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	members, err := sharedMembers(view)
	if err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	for k, v := range members {
		raw, err := json.Marshal(v)
		if err != nil {
			httpapi.WriteError(w, r, httpapi.InternalServerError(err))
			return
		}
		doc[k] = raw
	}
	body, err := json.Marshal(doc)
	if err != nil {
		httpapi.WriteError(w, r, httpapi.InternalServerError(err))
		return
	}
	body = append(body, '\n')
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ---- the log of a preparation or service -------------------------------------------

var resourceKeyPattern = regexp.MustCompile(`^(preparation|service\.[a-z][a-z0-9-]{0,29})$`)

// defaultResourceTail is the number of trailing lines of a resource log.
const defaultResourceTail = 100

func (s *SharedPublic) resourceLogs(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		httpapi.WriteError(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	key, tail, err := s.parseResourceQuery(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	switch err := s.reader.AuthorizeRead(r.Context(), c.Principal.ID, scope); {
	case err == nil:
	case errors.Is(err, logs.ErrNotFound):
		httpapi.WriteError(w, r, httpapi.NotFoundError())
		return
	case errors.Is(err, logs.ErrDenied):
		httpapi.WriteError(w, r, httpapi.ForbiddenError())
		return
	default:
		httpapi.WriteError(w, r, httpapi.UnavailableError(time.Second, err))
		return
	}
	run := domain.RunID(r.PathValue("id"))
	agg, err := s.runs.Get(r.Context(), scope, run)
	switch {
	case err == nil && agg != nil && agg.Run != nil && agg.Run.Shared != nil:
	case err == nil, errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		// A run that is not run_shared has no resources: indistinguishable from none.
		httpapi.WriteError(w, r, httpapi.NotFoundError())
		return
	default:
		httpapi.WriteError(w, r, httpapi.UnavailableError(time.Second, err))
		return
	}
	res := agg.Resource(key)
	if res == nil || (res.Kind != execution.KindPreparation && res.Kind != execution.KindService) {
		httpapi.WriteError(w, r, httpapi.NotFoundError())
		return
	}

	var content string
	truncated := false
	reasons := []string{}
	if res.UID == "" {
		// Nothing was created for the resource yet: there is no log, and that is said.
		reasons = append(reasons, "workload_missing")
	} else {
		got, err := s.resources.Logs(r.Context(), res.Handle(), execution.LogBound{MaxBytes: s.limits.MaxBytes})
		if err != nil {
			httpapi.WriteError(w, r, httpapi.UnavailableError(time.Second, err))
			return
		}
		content, truncated = got.Content, got.Truncated
		// The disclosures are read AFTER the bytes: a begin can commit while the
		// backend is read, and a value it discloses can already be in the output.
		// What was committed before the bytes were read is committed now, so the
		// reloaded run is a superset of what the output can contain. A run that
		// cannot be reloaded (or no longer is what it was) serves nothing.
		fresh, ferr := s.runs.Get(r.Context(), scope, run)
		if ferr != nil || fresh == nil || fresh.Run == nil || fresh.Run.Shared == nil || fresh.Run.ID != agg.Run.ID {
			if ferr == nil {
				ferr = execution.ErrNotFound
			}
			httpapi.WriteError(w, r, httpapi.UnavailableError(time.Second, ferr))
			return
		}
		agg = fresh
	}
	content, err = s.redact(r.Context(), scope, agg, content)
	if err != nil {
		// Never serve output whose secrets cannot be removed.
		httpapi.WriteError(w, r, httpapi.UnavailableError(time.Second, err))
		return
	}
	s.writeResourceLog(w, c, run, key, content, truncated, reasons, tail, agg.Run.Status.Terminal())
}

func (s *SharedPublic) parseResourceQuery(r *http.Request) (key string, tail int, err error) {
	q := r.URL.Query()
	for name, vals := range q {
		if (name != "resource" && name != "tail") || len(vals) != 1 {
			return "", 0, httpapi.BadRequestError("a resource log accepts only one resource and one tail")
		}
	}
	key = q.Get("resource")
	if !resourceKeyPattern.MatchString(key) {
		return "", 0, httpapi.BadRequestError("resource must be preparation or service.NAME")
	}
	tail = defaultResourceTail
	if v := q.Get("tail"); q.Has("tail") {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > s.limits.MaxLines {
			return "", 0, httpapi.BadRequestError("tail is outside the permitted range")
		}
		tail = n
	}
	return key, tail, nil
}

// allRunBindings serves every binding the run disclosed, whichever attempt asks,
// to secrets.Disclosures: a resource log belongs to no attempt.
type allRunBindings struct {
	run      domain.RunID
	bindings []runtimev1.SecretBinding
}

var _ secrets.RunBindings = allRunBindings{}

func (a allRunBindings) PriorBindings(context.Context, domain.Scope, domain.AttemptID) (domain.RunID, []runtimev1.SecretBinding, error) {
	return a.run, a.bindings, nil
}

// redact removes every value the run disclosed to any step or service: the
// output of a service or of the preparation can carry a secret another workload
// of the run was given. A set that cannot be read fails the read (no partial
// redaction).
func (s *SharedPublic) redact(ctx context.Context, scope domain.Scope, agg *execution.Aggregate, content string) (string, error) {
	bindings, err := runDisclosed(agg, "")
	if err != nil {
		return "", err
	}
	if len(bindings) == 0 || content == "" {
		return content, nil
	}
	disclosures, err := secrets.NewDisclosures(allRunBindings{run: agg.Run.ID, bindings: bindings}, s.secrets)
	if err != nil {
		return "", err
	}
	values, err := disclosures.Prior(ctx, scope, domain.AttemptID("att_resource_log"))
	if err != nil {
		return "", err
	}
	if len(values) == 0 {
		return content, nil
	}
	redactor, err := logs.NewRedactor(values)
	if err != nil {
		return "", err
	}
	out := redactor.Write([]byte(content))
	out = append(out, redactor.Flush()...)
	return string(out), nil
}

// resourceLogRecord is one NDJSON line of a resource log: the representation of
// the attempt log (api/logs.yaml) with the resource key in place of the attempt
// and step.
type resourceLogMeta struct {
	Type         string         `json:"type"`
	RunID        domain.RunID   `json:"runId"`
	Resource     string         `json:"resource"`
	Origin       string         `json:"origin"`
	Completeness string         `json:"completeness"`
	Reasons      []string       `json:"reasons"`
	Tail         int            `json:"tail"`
	Follow       bool           `json:"follow"`
	Terminal     bool           `json:"terminal"`
	Limits       map[string]int `json:"limits"`
}

func (s *SharedPublic) writeResourceLog(w http.ResponseWriter, c *httpapi.Call, run domain.RunID, key, content string, truncated bool, reasons []string, tail int, terminal bool) {
	var lines []string
	if content != "" {
		lines = strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	}
	stop := "end"
	if truncated {
		stop = "byte_limit"
	}
	if len(lines) > tail {
		lines, truncated, stop = lines[len(lines)-tail:], true, "line_limit"
	}
	if truncated {
		reasons = append(reasons, "truncated")
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	_ = enc.Encode(resourceLogMeta{
		Type: "meta", RunID: run, Resource: key, Origin: "live", Completeness: "unknown", Reasons: reasons, Tail: tail, Terminal: terminal,
		Limits: map[string]int{"maxBytes": int(s.limits.MaxBytes), "maxLines": s.limits.MaxLines, "maxLineBytes": int(s.limits.MaxBytes), "maxDurationSeconds": int(s.limits.MaxDuration / time.Second)},
	})
	total := 0
	for _, l := range lines {
		total += len(l) + 1
		_ = enc.Encode(map[string]any{"type": "chunk", "text": strings.ToValidUTF8(l, "\uFFFD"), "lineTruncated": false})
	}
	_ = enc.Encode(map[string]any{"type": "summary", "lines": len(lines), "bytes": total, "truncated": truncated, "stop": stop})
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Request-ID", c.RequestID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}
