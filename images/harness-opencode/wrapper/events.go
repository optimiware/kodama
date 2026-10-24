package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// redactor removes credentials from anything the wrapper retains or prints.
// It knows the exact runtime token values it has read (each distinct value the
// token file held, since the entrypoint renews it) and the shape of bearer
// credentials and signed tokens in general. Encodings of a value (base64, URL
// escapes) are not recognized; that is the same documented limit as the
// entrypoint's own log redaction.
type redactor struct {
	mu      sync.Mutex
	needles []string
}

var (
	bearerRE = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	jwtRE    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
)

const redacted = "[REDACTED]"

// Add registers an exact secret value (ignored when empty).
func (r *redactor) Add(v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.needles {
		if n == v {
			return
		}
	}
	r.needles = append(r.needles, v)
	// Longest first so a value that contains another is replaced whole.
	sort.Slice(r.needles, func(i, j int) bool { return len(r.needles[i]) > len(r.needles[j]) })
}

// String redacts s.
func (r *redactor) String(s string) string {
	r.mu.Lock()
	needles := append([]string(nil), r.needles...)
	r.mu.Unlock()
	for _, n := range needles {
		s = strings.ReplaceAll(s, n, redacted)
	}
	s = jwtRE.ReplaceAllString(s, redacted)
	return bearerRE.ReplaceAllString(s, "Bearer "+redacted)
}

// summary is the content-free account of one harness run. Nothing in it quotes
// model text, tool arguments or tool output, so it is safe for the retained
// command log even when transcript recording is disabled.
type summary struct {
	Lines      int
	Types      map[string]int
	Tools      map[string]int
	ToolErrors int
	Errors     []string
	// ErrorMessages are truncated provider/harness error texts. They can quote
	// remote content, so the caller logs them only when transcript recording is
	// authorized.
	ErrorMessages []string
	AutoRejected  []string
	StepsFinished int
	LastReason    string
}

func newSummary() *summary { return &summary{Types: map[string]int{}, Tools: map[string]int{}} }

type eventLine struct {
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Part *struct {
		Type   string `json:"type"`
		Tool   string `json:"tool"`
		Reason string `json:"reason"`
		State  *struct {
			Status string `json:"status"`
		} `json:"state"`
	} `json:"part"`
}

// observe folds one stdout line into the summary. A line that is not JSON is
// counted as such and otherwise ignored.
func (s *summary) observe(line []byte) {
	s.Lines++
	var e eventLine
	if err := json.Unmarshal(line, &e); err != nil || e.Type == "" {
		s.Types["unparsed"]++
		return
	}
	s.Types[e.Type]++
	switch e.Type {
	case "error":
		kind := "unknown"
		if e.Error != nil && e.Error.Type != "" {
			kind = e.Error.Type
		}
		s.Errors = append(s.Errors, kind)
		if e.Error != nil {
			m := e.Error.Message
			if len(m) > 300 {
				m = m[:300]
			}
			s.ErrorMessages = append(s.ErrorMessages, m)
		}
	case "tool_use":
		if e.Part != nil {
			s.Tools[e.Part.Tool]++
			if e.Part.State != nil && e.Part.State.Status == "error" {
				s.ToolErrors++
			}
		}
	case "step_finish":
		s.StepsFinished++
		if e.Part != nil {
			s.LastReason = e.Part.Reason
		}
	}
}

var autoRejectRE = regexp.MustCompile(`permission requested: (\S+) .*auto-rejecting`)

// observeStderr records a content-free note for OpenCode's non-interactive
// permission auto-rejection (measured: `opencode run` prints it and rejects the
// request rather than waiting for a human).
func (s *summary) observeStderr(line string) {
	if m := autoRejectRE.FindStringSubmatch(stripANSI(line)); m != nil {
		s.AutoRejected = append(s.AutoRejected, m[1])
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// pump reads r line by line (lines up to 16 MiB), calling fn with each line
// without its terminator, and returns when r ends.
func pump(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 {
			if len(line)+len(chunk) > 16<<20 {
				line = line[:0]
				fn([]byte(`{"type":"oversized"}`))
				for isPrefix { // discard the rest of the line
					_, isPrefix, err = br.ReadLine()
					if err != nil {
						break
					}
				}
				continue
			}
			line = append(line, chunk...)
		}
		if err != nil {
			if len(line) > 0 {
				fn(bytes.Clone(line))
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		if !isPrefix {
			fn(bytes.Clone(line))
			line = line[:0]
		}
	}
}
