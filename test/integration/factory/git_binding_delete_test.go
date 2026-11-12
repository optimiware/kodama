//go:build integration

package factorytest

// Regression: a verified resource binding is deletable.
//
// The schema carries no foreign key from verifications to bindings, so
// resource.binding.delete is not refused once the binding has verification
// rows, and verifications.binding_id stays as durable evidence. This test
// proves the public deletion succeeds, the historical verification IDs and
// values survive, the journal and report evidence survive, and an operation
// that requires the deleted binding is still refused without moving a ref.

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

func TestFactoryDeletesVerifiedBinding(t *testing.T) {
	fixture := startGitHarness(t)
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "verified-binding")

	// The clone itself records a real served git.read verification against the
	// exact binding this test deletes.
	read := fixture.waitVerifications(t, "git.read")
	if read.BindingID != fixture.git.ProjectBindingID || read.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("git.read verification names %s/%s; want %s/%s",
			read.BindingID, read.ResourceID, fixture.git.ProjectBindingID, fixture.git.ProjectResourceID)
	}

	// One accepted push adds a journaled git.ref.update verification and its
	// reported resource.verified event to the evidence that must survive.
	commit := fixture.commit(t, directory, "verified binding delete")
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+gitWorkRef); code != 0 {
		t.Fatalf("control push: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitWorkRef); remote != commit {
		t.Fatalf("control ref = %q; want %q", remote, commit)
	}
	entry := fixture.waitJournal(t, gitWorkRef, commit)
	report := fixture.waitReportEvent(t, entry.VerificationID)
	update := fixture.verificationByID(t, entry.VerificationID)

	binding := fixture.readBinding(t, fixture.git.ProjectBindingID)
	before := fixture.verificationsForBinding(t, fixture.git.ProjectBindingID)
	if len(before) < 2 {
		t.Fatalf("binding %s has %d verification rows; want at least the clone read and the push update",
			fixture.git.ProjectBindingID, len(before))
	}
	var sawRead, sawUpdate bool
	for _, row := range before {
		switch row.ID {
		case read.ID:
			sawRead = true
		case update.ID:
			sawUpdate = true
		}
	}
	if !sawRead || !sawUpdate {
		t.Fatalf("captured verification rows %v do not include the read %s and update %s", before, read.ID, update.ID)
	}

	// The binding route is id-only, so use the project-scoped credential and
	// quote the current version in the envelope.
	deleted := fixture.MustREST(fixture.ProjectToken, "resource.binding.delete", api.ResourceBindingDeleteRequest{
		BindingID: fixture.git.ProjectBindingID,
	}, &binding.Version, "delete-"+fixture.git.ProjectBindingID)
	var result api.ResourceBindingDeleteResult
	fixture.Decode(deleted, &result)
	if result.Version != binding.Version+1 {
		t.Fatalf("deletion version = %d; want %d", result.Version, binding.Version+1)
	}

	// The public read is a scoped miss, not a stale success.
	_, publicError := fixture.REST(fixture.ProjectToken, "resource.binding.read", api.ResourceBindingReadRequest{
		BindingID: fixture.git.ProjectBindingID,
	}, nil, "")
	if publicError.Code != api.CodeNotFound {
		t.Fatalf("resource.binding.read after deletion = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeNotFound)
	}

	// Every historical verification row survives byte-for-byte, including the
	// deleted binding identity and the operation values.
	after := fixture.verificationsForBinding(t, fixture.git.ProjectBindingID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("verification evidence changed across binding deletion:\nbefore %+v\nafter  %+v", before, after)
	}
	readAfter := fixture.verificationByID(t, read.ID)
	if readAfter != read {
		t.Fatalf("git.read verification changed: before %+v after %+v", read, readAfter)
	}
	updateAfter := fixture.verificationByID(t, update.ID)
	if updateAfter != update {
		t.Fatalf("git.ref.update verification changed: before %+v after %+v", update, updateAfter)
	}

	// The file-backed journal and the outbox/report event survive too.
	journalIntact := false
	for _, item := range fixture.git.JournalEntries(t) {
		if item.Ref == gitWorkRef && item.Candidate == commit &&
			item.State == git.JournalReported && item.VerificationID == entry.VerificationID {
			journalIntact = true
		}
	}
	if !journalIntact {
		t.Fatalf("journal entry for %s at %s did not survive the binding deletion", gitWorkRef, commit)
	}
	reportAfter, ok, err := fixture.git.ReportEvent(context.Background(), entry.VerificationID)
	if err != nil {
		t.Fatalf("read report event after deletion: %v", err)
	}
	if !ok {
		t.Fatalf("resource.verified event for verification %s did not survive the binding deletion", entry.VerificationID)
	}
	if reportAfter != report {
		t.Fatalf("report event changed across binding deletion: before %+v after %+v", report, reportAfter)
	}

	// A subsequent operation that requires the deleted binding is still
	// refused by the current-authority check, before any ref or journal effect.
	refused := fixture.commit(t, directory, "deleted binding")
	stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/deleted-binding")
	requireRefusal(t, "deleted binding", stderr, code)
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/deleted-binding"); remote != "" {
		t.Fatalf("ref moved after the binding was deleted: %q", remote)
	}
	requireNoAcceptedJournal(t, fixture, "refs/heads/work/deleted-binding", refused)
}

// verificationsForBinding returns every durable verification row naming one
// binding, in the fixture's stable order.
func (fixture *gitHarness) verificationsForBinding(t *testing.T, bindingID string) []factory.VerificationRow {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	var matching []factory.VerificationRow
	for _, row := range rows {
		if row.BindingID == bindingID {
			matching = append(matching, row)
		}
	}
	return matching
}
