package runprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Status is the outcome of one check.
type Status string

const (
	Pass          Status = "pass"
	Fail          Status = "fail"
	Blocked       Status = "blocked"        // a prerequisite outside this task's authority is missing
	NotAdvertised Status = "not_advertised" // capability exists nowhere in the profile and is not claimed
	NotRun        Status = "not_run"
)

// Check is one qualification obligation with its evidence.
type Check struct {
	ID         string   `json:"id"`
	Capability string   `json:"capability"`
	Required   bool     `json:"required"`
	Status     Status   `json:"status"`
	Detail     string   `json:"detail,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
	Command    string   `json:"command,omitempty"`
	Handoff    string   `json:"handoff,omitempty"`
}

// RequiredChecks is the complete inventory every qualification must contain.
// A missing entry is recorded as not_run, never ignored.
//
// The O-30 profile: storage, ko and Buildah in the exact step role, PostgreSQL
// and the tenant network model. Cleanup is verified separately and gates the
// verdict. BuildKit and browser are out of scope and are not checks.
var RequiredChecks = []string{
	"inventory", "storage.rwop_sequential", "storage.rwop_exclusive", "storage.idmapped_ownership",
	"storage.release_delete", "fixture.registry", "build.ko", "step.role_shape.ko", "step.role_shape", "psa.builder_pod",
	"build.buildah_vfs", "service.postgres", "network.matrix",
}

// Cleanup is the bounded, ownership-checked teardown inventory.
type Cleanup struct {
	Namespaces []CleanupItem `json:"namespaces"`
	Leftovers  []string      `json:"leftovers"`
	Verified   bool          `json:"verified"`
	Evidence   []string      `json:"evidence,omitempty"`
}

// CleanupItem is one object the run created and removed.
type CleanupItem struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	Deleted bool   `json:"deleted"`
	Detail  string `json:"detail,omitempty"`
}

// Report is the retained qualification result.
type Report struct {
	Schema     string                       `json:"schema"`
	Target     string                       `json:"target"`
	RunID      string                       `json:"runId"`
	Commit     string                       `json:"commit,omitempty"`
	Started    time.Time                    `json:"started"`
	Finished   time.Time                    `json:"finished"`
	Pins       map[string]string            `json:"pins"`
	ImageIDs   map[string]string            `json:"observedImageIds"`
	Inventory  map[string]any               `json:"inventory"`
	Checks     []Check                      `json:"checks"`
	Cleanup    Cleanup                      `json:"cleanup"`
	Verdict    string                       `json:"verdict"`
	Matrix     []MatrixRow                  `json:"supportMatrix"`
	Frozen     map[string]map[string]string `json:"frozenSettings,omitempty"`
	Handoffs   []string                     `json:"installationHandoffs,omitempty"`
	Limitation string                       `json:"limitation"`
}

// MatrixRow is one entry of the support matrix: only combinations with a
// passing check are marked qualified.
type MatrixRow struct {
	Combination string `json:"combination"`
	Status      string `json:"status"` // qualified | unqualified | blocked | not_advertised
	Checks      string `json:"checks"`
}

const reportSchema = "kodama.run-resource-profile-report/v1"

// Add appends a check.
func (r *Report) Add(c Check) { r.Checks = append(r.Checks, c) }

// Find returns the check with id.
func (r *Report) Find(id string) *Check {
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return &r.Checks[i]
		}
	}
	return nil
}

// Finalize computes the verdict and the support matrix. Qualified means every
// required check passed and cleanup was verified; nothing else qualifies.
func (r *Report) Finalize() {
	r.Schema = reportSchema
	r.Finished = time.Now().UTC()
	for _, id := range RequiredChecks {
		if r.Find(id) == nil {
			r.Add(Check{ID: id, Capability: id, Required: true, Status: NotRun,
				Detail: "required check absent: it was never recorded (phase skipped, failed early or interrupted)"})
		}
	}
	failed, blocked, missing := 0, 0, 0
	for _, c := range r.Checks {
		if !c.Required {
			continue
		}
		switch c.Status {
		case Pass:
		case Blocked:
			blocked++
		case NotRun:
			missing++
		default:
			failed++
		}
	}
	switch {
	case failed > 0:
		r.Verdict = "failed"
	case blocked > 0 || missing > 0:
		r.Verdict = "blocked"
	case !r.Cleanup.Verified || len(r.Cleanup.Namespaces) == 0:
		r.Verdict = "failed"
	default:
		r.Verdict = "qualified"
	}
	r.Matrix = r.matrix()
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if c.Handoff != "" && !seen[c.Handoff] {
			seen[c.Handoff] = true
			r.Handoffs = append(r.Handoffs, c.Handoff)
		}
	}
	sort.Strings(r.Handoffs)
	r.Limitation = "Bounded probes of the deployable profile on the named target only. This is not product runtime behavior, a stage rollout approval or a qualification of any combination not marked qualified."
}

func (r *Report) matrix() []MatrixRow {
	rows := []struct{ name, ids string }{
		{"RWOP run storage shared sequentially across images and UIDs (Scaleway SBS ext4, idmapped, Delete reclaim)", "storage.rwop_sequential,storage.rwop_exclusive,storage.idmapped_ownership,storage.release_delete"},
		{"ko build+push in the exact step role to authenticated TLS registry (Go dependency fetch)", "step.role_shape.ko,build.ko"},
		{"Buildah build+push in the exact step role (VFS, chroot, RUN, multi-UID, dependency fetch, authenticated push, digest match)", "step.role_shape,psa.builder_pod,build.buildah_vfs"},
		{"Buildah overlay/FUSE", "build.buildah_overlay_fuse"},
		{"PostgreSQL non-root readiness and data roundtrip", "service.postgres"},
		{"Tenant network model (same-tenant other-run reachable; foreign tenant, Kubernetes API and metadata denied; public HTTPS open)", "network.matrix"},
	}
	var out []MatrixRow
	for _, row := range rows {
		status := "qualified"
		for _, id := range strings.Split(row.ids, ",") {
			c := r.Find(id)
			switch {
			case c == nil:
				status = "unqualified"
			case c.Status == Pass:
			case c.Status == Blocked:
				if status != "unqualified" {
					status = "blocked"
				}
			case c.Status == NotAdvertised:
				if status == "qualified" {
					status = "not_advertised"
				}
			default:
				status = "unqualified"
			}
		}
		out = append(out, MatrixRow{Combination: row.name, Status: status, Checks: row.ids})
	}
	return out
}

// Write stores report.json and report.md in dir.
func (r *Report) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.Markdown()), 0o644)
}

// Markdown renders the human report.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Run-resource profile qualification `%s`\n\n", r.RunID)
	fmt.Fprintf(&b, "Target: `%s`. Verdict: **%s**. %s to %s.\n\n", r.Target, r.Verdict, r.Started.Format(time.RFC3339), r.Finished.Format(time.RFC3339))
	b.WriteString("## Support matrix\n\n| Combination | Status | Checks |\n| --- | --- | --- |\n")
	for _, m := range r.Matrix {
		fmt.Fprintf(&b, "| %s | %s | `%s` |\n", m.Combination, m.Status, m.Checks)
	}
	b.WriteString("\n## Checks\n\n| Check | Required | Status | Detail |\n| --- | --- | --- | --- |\n")
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "| `%s` | %t | %s | %s |\n", c.ID, c.Required, c.Status, oneLine(c.Detail))
	}
	if len(r.Handoffs) > 0 {
		b.WriteString("\n## Installation handoffs\n\n")
		for _, h := range r.Handoffs {
			fmt.Fprintf(&b, "- %s\n", h)
		}
	}
	fmt.Fprintf(&b, "\n## Cleanup\n\nVerified: %t. Leftovers: %v.\n", r.Cleanup.Verified, r.Cleanup.Leftovers)
	for _, n := range r.Cleanup.Namespaces {
		fmt.Fprintf(&b, "- namespace `%s` uid `%s` deleted=%t %s\n", n.Name, n.UID, n.Deleted, n.Detail)
	}
	fmt.Fprintf(&b, "\n%s\n", r.Limitation)
	return b.String()
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	if len(s) > 280 {
		s = s[:280] + "…"
	}
	return s
}
