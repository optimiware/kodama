package run

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

// Local bounds on what the CLI buffers. The server bounds its own output; these
// only stop a misbehaving peer from exhausting memory.
const (
	maxLogLineBytes = 1 << 20
	maxStepPages    = 50
)

var resourceKeys = regexp.MustCompile(`^(preparation|service\.[a-z][a-z0-9-]{0,29})$`)

var logSources = map[string]bool{"auto": true, "live": true, "wrapper": true, "controller": true}

// logMeta, logChunk and logSummary hold the members the CLI validates. Unknown
// members are accepted and preserved in the rendered JSON (additive evolution).
type logMeta struct {
	Type         string   `json:"type"`
	RunID        string   `json:"runId"`
	AttemptID    string   `json:"attemptId"`
	StepID       string   `json:"stepId"`
	Origin       string   `json:"origin"`
	Completeness string   `json:"completeness"`
	Reasons      []string `json:"reasons"`
}

type logSummary struct {
	Lines     json.Number
	Bytes     json.Number
	Truncated bool
	Stop      string
}

// knownStops are the stop reasons of api/logs.yaml. An unrecognised value is
// refused rather than reported as a successful retrieval.
var knownStops = map[string]bool{
	"end": true, "byte_limit": true, "line_limit": true, "duration_limit": true, "scan_limit": true,
	"interrupted": true, "authorization_changed": true, "unavailable": true,
}

// decodeMeta validates the members the CLI relies on. Other members, including
// ones added later, are preserved untouched in the rendered document.
func decodeMeta(rec map[string]any) (logMeta, bool) {
	var m logMeta
	m.Type = "meta"
	for _, f := range []struct {
		key string
		dst *string
	}{{"runId", &m.RunID}, {"attemptId", &m.AttemptID}, {"stepId", &m.StepID}, {"origin", &m.Origin}, {"completeness", &m.Completeness}} {
		v, ok := rec[f.key].(string)
		if !ok || v == "" {
			return logMeta{}, false
		}
		*f.dst = v
	}
	reasons, ok := rec["reasons"].([]any)
	if !ok {
		return logMeta{}, false
	}
	m.Reasons = make([]string, 0, len(reasons))
	for _, r := range reasons {
		s, ok := r.(string)
		if !ok {
			return logMeta{}, false
		}
		m.Reasons = append(m.Reasons, s)
	}
	return m, true
}

func decodeChunk(rec map[string]any) (text string, truncated bool, ok bool) {
	text, ok = rec["text"].(string)
	if !ok {
		return "", false, false
	}
	truncated, ok = rec["lineTruncated"].(bool)
	return text, truncated, ok
}

// counter reports whether v is a non-negative integer literal.
func counter(v any) (json.Number, bool) {
	n, ok := v.(json.Number)
	if !ok || !integerLiteral.MatchString(n.String()) || strings.HasPrefix(n.String(), "-") {
		return "", false
	}
	return n, true
}

func decodeSummary(rec map[string]any) (logSummary, bool) {
	var s logSummary
	var ok bool
	if s.Lines, ok = counter(rec["lines"]); !ok {
		return logSummary{}, false
	}
	if s.Bytes, ok = counter(rec["bytes"]); !ok {
		return logSummary{}, false
	}
	if s.Truncated, ok = rec["truncated"].(bool); !ok {
		return logSummary{}, false
	}
	if s.Stop, ok = rec["stop"].(string); !ok || s.Stop == "" {
		return logSummary{}, false
	}
	return s, true
}

type attemptRef struct {
	ID      string `json:"id"`
	Ordinal int    `json:"ordinal"`
	Status  string `json:"status"`
}

type stepRef struct {
	ID       string       `json:"id"`
	Attempts []attemptRef `json:"attempts"`
}

type stepPage struct {
	Items      []stepRef `json:"items"`
	NextCursor *string   `json:"nextCursor"`
}

func bindLogs(fs *flag.FlagSet) root.RunFunc {
	step := fs.String("step", "", "step ID whose attempt log to read")
	attempt := fs.String("attempt", "", "attempt ID; required when a step has several attempts")
	tail := fs.String("tail", "", "number of trailing lines (the server bounds and validates it)")
	follow := fs.Bool("follow", false, "keep a live log open until it ends; output is buffered until then")
	source := fs.String("source", "", "auto, live, wrapper or controller")
	resource := fs.String("resource", "", "read the log of a run_shared run's preparation (preparation) or of a support service (service.NAME) instead of an attempt")
	return func(ctx context.Context, inv *root.Invocation, args []string) (root.Result, error) {
		runID, err := oneArg(args, "the run ID")
		if err != nil {
			return root.Result{}, err
		}
		set := explicit(fs)
		q := url.Values{}
		if set["resource"] {
			if !resourceKeys.MatchString(*resource) {
				return root.Result{}, root.Usagef("--resource must be preparation or service.NAME")
			}
			// A resource has no attempt, step, source or live stream: only the tail
			// combines with it.
			if set["step"] || set["attempt"] || set["follow"] || set["source"] {
				return root.Result{}, root.Usagef("--resource excludes --step, --attempt, --follow and --source")
			}
		}
		if set["tail"] {
			if !limitPattern.MatchString(*tail) {
				return root.Result{}, root.Usagef("--tail must be a positive integer")
			}
			q.Set("tail", *tail)
		}
		if set["follow"] && *follow {
			q.Set("follow", "true")
		}
		if set["source"] {
			if !logSources[*source] {
				return root.Result{}, root.Usagef("--source must be auto, live, wrapper or controller")
			}
			q.Set("source", *source)
		}
		if set["step"] && *step == "" || set["attempt"] && *attempt == "" {
			return root.Result{}, root.Usagef("--step and --attempt must not be empty")
		}
		c, scope, err := connect(inv)
		if err != nil {
			return root.Result{}, err
		}
		if set["resource"] {
			q.Set("resource", *resource)
			resp, err := c.Do(ctx, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"runs", runID, "logs"}, Query: q}, nil)
			if err != nil {
				return root.Result{}, err
			}
			doc, text, err := parseResourceLogs(resp.Body, runID, *resource)
			if err != nil {
				return root.Result{}, err
			}
			return root.Result{Data: doc, Text: text}, nil
		}
		attemptID, err := resolveAttempt(ctx, c, scope, runID, *step, *attempt)
		if err != nil {
			return root.Result{}, err
		}
		if attemptID != "" {
			q.Set("attemptId", attemptID)
		}
		resp, err := c.Do(ctx, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"runs", runID, "logs"}, Query: q}, nil)
		if err != nil {
			return root.Result{}, err
		}
		doc, text, err := parseLogs(resp.Body, runID, attemptID, *step)
		if err != nil {
			return root.Result{}, err
		}
		return root.Result{Data: doc, Text: text}, nil
	}
}

// resolveAttempt maps --step to an attempt ID by reading the run's steps. It
// never picks among several attempts: that is the caller's decision.
func resolveAttempt(ctx context.Context, c *client.Client, scope client.Scope, runID, step, attempt string) (string, error) {
	if step == "" {
		return attempt, nil
	}
	cursor := ""
	for page := 0; page < maxStepPages; page++ {
		q := url.Values{"limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var steps stepPage
		if _, err := c.Do(ctx, client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"runs", runID, "steps"}, Query: q}, &steps); err != nil {
			return "", err
		}
		for _, s := range steps.Items {
			if s.ID != step {
				continue
			}
			return pickAttempt(s, attempt)
		}
		if steps.NextCursor == nil || *steps.NextCursor == "" {
			return "", root.Usagef("run %s has no step %q", runID, step)
		}
		cursor = *steps.NextCursor
	}
	return "", root.NewError(root.ExitFailure, "steps_too_many_pages", "the run has more step pages than the CLI will read; pass --attempt directly")
}

func pickAttempt(s stepRef, attempt string) (string, error) {
	if attempt != "" {
		for _, a := range s.Attempts {
			if a.ID == attempt {
				return attempt, nil
			}
		}
		return "", root.Usagef("attempt %q does not belong to step %q", attempt, s.ID)
	}
	switch len(s.Attempts) {
	case 0:
		return "", root.Usagef("step %q has no attempts yet", s.ID)
	case 1:
		return s.Attempts[0].ID, nil
	}
	var list []string
	for _, a := range s.Attempts {
		list = append(list, fmt.Sprintf("#%d %s (%s)", a.Ordinal, a.ID, a.Status))
	}
	return "", root.Usagef("step %q has %d attempts; choose one with --attempt: %s", s.ID, len(s.Attempts), strings.Join(list, ", "))
}

// parseLogs validates the NDJSON protocol meta, chunk*, summary and returns one
// deterministic document and its human rendering. Anything else - a missing
// summary, an unexpected record, a record after the summary, an identity that
// differs from the request - fails with nothing rendered, so a partial read is
// never mistaken for a complete one.
func parseLogs(body []byte, runID, attemptID, step string) (map[string]any, string, error) {
	rd := bufio.NewReaderSize(bytes.NewReader(body), 64<<10)
	var (
		meta, summary map[string]any
		m             logMeta
		sum           logSummary
		chunks        = []any{} // an empty log renders as [], not null
		texts         []string
		truncs        []bool
	)
	for {
		line, err := readRecord(rd)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", logFailure("logs_malformed", "the log response contains a record the CLI cannot read", nil)
		}
		if summary != nil {
			return nil, "", logFailure("logs_malformed", "the log response has records after its summary", nil)
		}
		v, err := root.NormalizeJSON(line)
		rec, ok := v.(map[string]any)
		if err != nil || !ok {
			return nil, "", logFailure("logs_malformed", "the log response contains a record that is not a JSON object", nil)
		}
		kind, _ := rec["type"].(string)
		switch {
		case kind == "meta" && meta == nil && len(chunks) == 0:
			parsed, ok := decodeMeta(rec)
			if !ok {
				return nil, "", logFailure("logs_malformed", "the log metadata record is incomplete or has a member of the wrong type", nil)
			}
			m, meta = parsed, rec
		case kind == "chunk" && meta != nil:
			text, truncated, ok := decodeChunk(rec)
			if !ok {
				return nil, "", logFailure("logs_malformed", "a log chunk record is incomplete or has a member of the wrong type", nil)
			}
			// Additive members survive; only the discriminator is dropped.
			entry := make(map[string]any, len(rec))
			for k, v := range rec {
				if k != "type" {
					entry[k] = v
				}
			}
			chunks = append(chunks, entry)
			texts = append(texts, text)
			truncs = append(truncs, truncated)
		case kind == "summary" && meta != nil:
			parsed, ok := decodeSummary(rec)
			if !ok {
				return nil, "", logFailure("logs_malformed", "the log summary record is incomplete or has a member of the wrong type", nil)
			}
			if !knownStops[parsed.Stop] {
				return nil, "", logFailure("logs_malformed", "the log summary names a stop reason the CLI does not recognise; the retrieval is not treated as successful", nil)
			}
			sum, summary = parsed, rec
		default:
			return nil, "", logFailure("logs_malformed", "the log response is out of order or has an unknown record", nil)
		}
	}
	if meta == nil {
		return nil, "", logFailure("logs_incomplete", "the log response was empty: no metadata was received", nil)
	}
	if m.RunID != runID || (attemptID != "" && m.AttemptID != attemptID) || (step != "" && m.StepID != step) {
		return nil, "", logFailure("logs_mismatch", "the log response describes another run, step or attempt than requested; nothing was rendered", nil)
	}
	details := map[string]any{"runId": m.RunID, "attemptId": m.AttemptID, "stepId": m.StepID, "origin": m.Origin, "completeness": m.Completeness}
	if summary == nil {
		e := root.NewError(root.ExitFailure, "logs_incomplete", "the log response ended without a summary: the retrieval was interrupted. This says nothing about the run's outcome; retry `kodama run logs`")
		e.Details = details
		return nil, "", e
	}
	switch sum.Stop {
	case "authorization_changed":
		e := root.NewError(root.ExitAuth, "authorization_changed", "access to the log was revoked or lapsed while it was being read; nothing was rendered")
		e.Details = details
		return nil, "", e
	case "unavailable", "interrupted":
		e := root.NewError(root.ExitFailure, "logs_"+sum.Stop, "the log read ended %s upstream (stop %q); nothing was rendered. This says nothing about the run's outcome", map[string]string{"unavailable": "because a dependency was unavailable", "interrupted": "because the source was interrupted"}[sum.Stop], sum.Stop)
		e.Details = details
		return nil, "", e
	}
	doc := map[string]any{"meta": meta, "lines": chunks, "summary": summary}
	return doc, logsText(m, sum, texts, truncs), nil
}

// readRecord reads one non-terminating-newline-bounded NDJSON line.
func readRecord(rd *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := rd.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLogLineBytes {
			return nil, errFrameTooLarge
		}
		switch {
		case err == nil:
			line = bytes.TrimRight(line, "\r\n")
			if len(line) == 0 {
				return nil, errBadFrame
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			// A final line without its newline is a truncated record.
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

func logFailure(code, msg string, cause error) error {
	e := root.NewError(root.ExitFailure, code, "%s", msg)
	if cause != nil {
		return e.WithCause(cause)
	}
	return e
}

func logsText(m logMeta, sum logSummary, texts []string, truncs []bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "== run %s step %s attempt %s: origin %s, completeness %s", m.RunID, m.StepID, m.AttemptID, m.Origin, m.Completeness)
	if len(m.Reasons) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(m.Reasons, ", "))
	}
	b.WriteString("\n")
	for i, t := range texts {
		b.WriteString(t)
		if truncs[i] {
			b.WriteString(" [line truncated]")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "== end: %s line(s), %s byte(s), stop %s", sum.Lines, sum.Bytes, sum.Stop)
	if sum.Truncated {
		b.WriteString(", output truncated by a server bound")
	}
	if m.Completeness != "complete" {
		b.WriteString("; the capture is not known to be complete")
	}
	b.WriteString(". This is the attempt's log, not the run's outcome.\n")
	return b.String()
}

// parseResourceLogs validates the NDJSON protocol of a resource log: the same
// meta, chunk*, summary sequence as an attempt's log, with `resource` in the meta
// record in place of the attempt and step. The same rules hold: a missing
// summary, an unexpected or late record, an identity other than the requested
// one or a stop the CLI does not recognise renders nothing, so a partial read is
// never mistaken for a complete one.
func parseResourceLogs(body []byte, runID, resource string) (map[string]any, string, error) {
	rd := bufio.NewReaderSize(bytes.NewReader(body), 64<<10)
	var (
		meta, summary map[string]any
		sum           logSummary
		chunks        = []any{}
		texts         []string
		truncs        []bool
	)
	for {
		line, err := readRecord(rd)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", logFailure("logs_malformed", "the log response contains a record the CLI cannot read", nil)
		}
		if summary != nil {
			return nil, "", logFailure("logs_malformed", "the log response has records after its summary", nil)
		}
		v, err := root.NormalizeJSON(line)
		rec, ok := v.(map[string]any)
		if err != nil || !ok {
			return nil, "", logFailure("logs_malformed", "the log response contains a record that is not a JSON object", nil)
		}
		kind, _ := rec["type"].(string)
		switch {
		case kind == "meta" && meta == nil && len(chunks) == 0:
			if !resourceMetaOK(rec) {
				return nil, "", logFailure("logs_malformed", "the log metadata record is incomplete or has a member of the wrong type", nil)
			}
			meta = rec
		case kind == "chunk" && meta != nil:
			text, truncated, ok := decodeChunk(rec)
			if !ok {
				return nil, "", logFailure("logs_malformed", "a log chunk record is incomplete or has a member of the wrong type", nil)
			}
			entry := make(map[string]any, len(rec))
			for k, v := range rec {
				if k != "type" {
					entry[k] = v
				}
			}
			chunks = append(chunks, entry)
			texts = append(texts, text)
			truncs = append(truncs, truncated)
		case kind == "summary" && meta != nil:
			parsed, ok := decodeSummary(rec)
			if !ok {
				return nil, "", logFailure("logs_malformed", "the log summary record is incomplete or has a member of the wrong type", nil)
			}
			if !knownStops[parsed.Stop] {
				return nil, "", logFailure("logs_malformed", "the log summary names a stop reason the CLI does not recognise; the retrieval is not treated as successful", nil)
			}
			sum, summary = parsed, rec
		default:
			return nil, "", logFailure("logs_malformed", "the log response is out of order or has an unknown record", nil)
		}
	}
	if meta == nil {
		return nil, "", logFailure("logs_incomplete", "the log response was empty: no metadata was received", nil)
	}
	if meta["runId"] != runID || meta["resource"] != resource {
		return nil, "", logFailure("logs_mismatch", "the log response describes another run or resource than requested; nothing was rendered", nil)
	}
	details := map[string]any{"runId": runID, "resource": resource, "completeness": meta["completeness"]}
	if summary == nil {
		e := root.NewError(root.ExitFailure, "logs_incomplete", "the log response ended without a summary: the retrieval was interrupted. This says nothing about the run's outcome; retry `kodama run logs`")
		e.Details = details
		return nil, "", e
	}
	switch sum.Stop {
	case "authorization_changed":
		e := root.NewError(root.ExitAuth, "authorization_changed", "access to the log was revoked or lapsed while it was being read; nothing was rendered")
		e.Details = details
		return nil, "", e
	case "unavailable", "interrupted":
		e := root.NewError(root.ExitFailure, "logs_"+sum.Stop, "the log read ended %s upstream (stop %q); nothing was rendered. This says nothing about the run's outcome", map[string]string{"unavailable": "because a dependency was unavailable", "interrupted": "because the source was interrupted"}[sum.Stop], sum.Stop)
		e.Details = details
		return nil, "", e
	}
	var b strings.Builder
	completeness, _ := meta["completeness"].(string)
	origin, _ := meta["origin"].(string)
	fmt.Fprintf(&b, "== run %s resource %s: origin %s, completeness %s", runID, resource, origin, completeness)
	if reasons, _ := meta["reasons"].([]any); len(reasons) > 0 {
		names := make([]string, 0, len(reasons))
		for _, r := range reasons {
			if s, ok := r.(string); ok {
				names = append(names, s)
			}
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(names, ", "))
	}
	b.WriteString("\n")
	for i, t := range texts {
		b.WriteString(t)
		if truncs[i] {
			b.WriteString(" [line truncated]")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "== end: %s line(s), %s byte(s), stop %s", sum.Lines, sum.Bytes, sum.Stop)
	if sum.Truncated {
		b.WriteString(", output truncated by a server bound")
	}
	b.WriteString(". This is the resource's log, not the run's outcome.\n")
	return map[string]any{"meta": meta, "lines": chunks, "summary": summary}, b.String(), nil
}

// resourceMetaOK validates the members the CLI relies on in a resource meta record.
func resourceMetaOK(rec map[string]any) bool {
	for _, key := range []string{"runId", "resource", "origin", "completeness"} {
		if v, ok := rec[key].(string); !ok || v == "" {
			return false
		}
	}
	reasons, ok := rec["reasons"].([]any)
	if !ok {
		return false
	}
	for _, r := range reasons {
		if _, ok := r.(string); !ok {
			return false
		}
	}
	return true
}
