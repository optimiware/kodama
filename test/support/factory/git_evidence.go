package factory

// Read-only evidence helpers for the host Git composition.
//
// Everything here observes durable state: the journal files the product writes,
// the scoped verification rows the served Shell committed, the linked event and
// a raw credential-leak scan. Nothing injects authority, approval or a
// verification row.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// VerificationRow is one durable verification record. The schema carries no
// source-event column on verifications: the report identity lives only on the
// journal entry and the outbox event.
type VerificationRow struct {
	ID          string
	ServiceID   string
	ActorID     string
	ExecutionID string
	ActionID    string
	BindingID   string
	ResourceID  string
	Operation   string
	Result      string
	Approved    bool
	// Subject is the raw JSONB subject text. The canonical empty subject is
	// `{"fields":[]}`.
	Subject string
}

// EventRow is one durable lifecycle/report event.
type EventRow struct {
	ID              string
	Type            string
	ReportType      string
	SubjectID       string
	CorrelationID   string
	CausationID     string
	SourceKind      string
	SourcePrincipal string
}

// JournalEntries returns every durable journal entry, ordered by source event.
func (fixture *GitFixture) JournalEntries(t testing.TB) []git.JournalEntry {
	t.Helper()
	directory, err := os.ReadDir(fixture.JournalDirectory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read Git journal directory: %v", err)
	}
	var entries []git.JournalEntry
	for _, item := range directory {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(fixture.JournalDirectory, item.Name()))
		if err != nil {
			t.Fatalf("read Git journal entry %q: %v", item.Name(), err)
		}
		var entry git.JournalEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			t.Fatalf("decode Git journal entry %q: %v", item.Name(), err)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].SourceEventID < entries[right].SourceEventID
	})
	return entries
}

// Verifications returns the scoped verification rows through the runtime role.
func (fixture *GitFixture) Verifications(ctx context.Context) ([]VerificationRow, error) {
	if fixture == nil || fixture.h == nil {
		return nil, fmt.Errorf("git fixture has no harness")
	}
	rows, err := pgtest.ScopedQuery(ctx, fixture.h.runtimeDB, fixture.h.Scope, 11,
		`SELECT id, service_id, actor_id,
		        coalesce(execution_id, ''), coalesce(action_id, ''),
		        binding_id, resource_id, operation, result, approved, subject::text
		   FROM verifications
		  ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	verifications := make([]VerificationRow, 0, len(rows))
	for _, row := range rows {
		if len(row) != 11 {
			return nil, fmt.Errorf("verification row has %d columns; want 11", len(row))
		}
		verifications = append(verifications, VerificationRow{
			ID:          row[0],
			ServiceID:   row[1],
			ActorID:     row[2],
			ExecutionID: row[3],
			ActionID:    row[4],
			BindingID:   row[5],
			ResourceID:  row[6],
			Operation:   row[7],
			Result:      row[8],
			Approved:    row[9] == "true",
			Subject:     row[10],
		})
	}
	return verifications, nil
}

// WaitFor polls one named condition with a bounded deadline. On timeout it
// fails with a secret-free evidence summary: the journal listing with entry
// states and count, the newest verification rows and the registered resource
// identities. A bare "condition was not satisfied" is not attributable; this
// makes a live failure say which durable fact was missing.
func (fixture *GitFixture) WaitFor(t testing.TB, name string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: condition was not satisfied within %s\n%s", name, timeout, fixture.DiagnosticSummary())
}

// DiagnosticSummary renders secret-free evidence about this fixture's durable
// Git facts. Journal entries carry no credential and verification rows are
// rendered without subject bytes.
func (fixture *GitFixture) DiagnosticSummary() string {
	var summary strings.Builder
	summarizeJournalDirectory(&summary, fixture.JournalDirectory)
	summarizeVerifications(&summary, fixture)
	fmt.Fprintf(&summary, "resources: admin=%s project=%s deploy=%s\n",
		fixture.AdminResourceID, fixture.ProjectResourceID, fixture.DeployResourceID)
	return summary.String()
}

// summarizeJournalDirectory lists every durable journal file with its decoded
// state and identity, then the exact entry count.
func summarizeJournalDirectory(summary *strings.Builder, directory string) {
	fmt.Fprintf(summary, "journal directory: %s\n", directory)
	items, err := os.ReadDir(directory)
	if err != nil {
		fmt.Fprintf(summary, "journal listing: %v\n", err)
		return
	}
	count := 0
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		count++
		data, err := os.ReadFile(filepath.Join(directory, item.Name()))
		if err != nil {
			fmt.Fprintf(summary, "  %s: read error: %v\n", item.Name(), err)
			continue
		}
		var entry git.JournalEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			fmt.Fprintf(summary, "  %s: decode error: %v\n", item.Name(), err)
			continue
		}
		fmt.Fprintf(summary, "  %s: state=%s repository=%s ref=%s candidate=%s verification=%s\n",
			item.Name(), entry.State, entry.Repository, entry.Ref, entry.Candidate, entry.VerificationID)
	}
	fmt.Fprintf(summary, "journal entries: %d\n", count)
}

// summarizeVerifications renders the newest verification rows. A query error is
// reported as text: a diagnostic must never fail the test that already failed.
func summarizeVerifications(summary *strings.Builder, fixture *GitFixture) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := fixture.Verifications(ctx)
	if err != nil {
		fmt.Fprintf(summary, "verifications: query error: %v\n", err)
		return
	}
	fmt.Fprintf(summary, "verifications: %d rows\n", len(rows))
	start := 0
	if len(rows) > 12 {
		start = len(rows) - 12
	}
	for _, row := range rows[start:] {
		fmt.Fprintf(summary, "  %s service=%s actor=%s operation=%s result=%s approved=%t resource=%s binding=%s\n",
			row.ID, row.ServiceID, row.ActorID, row.Operation, row.Result, row.Approved, row.ResourceID, row.BindingID)
	}
}

// ReportEvent returns the verified-report event caused by one verification.
func (fixture *GitFixture) ReportEvent(ctx context.Context, verificationID string) (EventRow, bool, error) {
	if fixture == nil || fixture.h == nil {
		return EventRow{}, false, fmt.Errorf("git fixture has no harness")
	}
	rows, err := pgtest.ScopedQuery(ctx, fixture.h.runtimeDB, fixture.h.Scope, 8,
		`SELECT id, type, coalesce(report_type, ''), subject_id,
		        coalesce(correlation_id, ''), coalesce(causation_id, ''),
		        source_kind, source_principal
		   FROM events
		  WHERE causation_id = $1 AND type = 'resource.verified'
		  ORDER BY at, id`, verificationID)
	if err != nil {
		return EventRow{}, false, err
	}
	if len(rows) == 0 {
		return EventRow{}, false, nil
	}
	if len(rows) > 1 {
		return EventRow{}, false, fmt.Errorf("verification %s caused %d report events", verificationID, len(rows))
	}
	row := rows[0]
	if len(row) != 8 {
		return EventRow{}, false, fmt.Errorf("event row has %d columns; want 8", len(row))
	}
	return EventRow{
		ID:              row[0],
		Type:            row[1],
		ReportType:      row[2],
		SubjectID:       row[3],
		CorrelationID:   row[4],
		CausationID:     row[5],
		SourceKind:      row[6],
		SourcePrincipal: row[7],
	}, true, nil
}

// ScanCredentialLeaks returns every path under the repository storage, the
// receive journal or the composition log whose bytes contain one of the given
// values. Credential files themselves are not scanned: the mounted file is the
// one intended copy.
func (fixture *GitFixture) ScanCredentialLeaks(t testing.TB, values ...string) []string {
	t.Helper()
	needles := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			needles = append(needles, value)
		}
	}
	if len(needles) == 0 {
		return nil
	}
	var hits []string
	for _, root := range []string{fixture.StorageRoot, fixture.JournalDirectory} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() > 4<<20 {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, needle := range needles {
				if strings.Contains(string(data), needle) {
					hits = append(hits, path)
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s for credentials: %v", root, err)
		}
	}
	if fixture.h != nil {
		logs := fixture.h.Logs()
		for _, needle := range needles {
			if strings.Contains(logs, needle) {
				hits = append(hits, "composition-log")
				break
			}
		}
	}
	return hits
}
