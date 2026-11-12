package domaincontract

// Verification reader scope vectors. Every neutral store seam (Executor.Read,
// Scanner.Query) requires a tenancy scope and PostgreSQL enforces forced
// row-level security, so a store-backed verify.VerificationReader cannot be
// written without one.
//
// The seam carries the report command's owner scope. It reads only inside
// that scope and a record outside it is uniformly not_found, never forbidden,
// so the read is not an existence oracle. These are declaration and
// value-level contract checks; no adapter, store or process is exercised.

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// scopedVerificationReader is the reference implementation of the seam: it
// resolves only inside the caller's scope and reports a record outside that
// scope with the same not_found class as an unknown identity.
type scopedVerificationReader struct {
	records map[verify.VerificationID]verify.Record
}

func (reader scopedVerificationReader) ReadVerification(
	_ context.Context,
	scope tenancy.Scope,
	verificationID verify.VerificationID,
) (verify.Record, error) {
	record, ok := reader.records[verificationID]
	if !ok || record.Scope != scope {
		return verify.Record{}, fault.New(fault.NotFound, "verification record is not visible in this scope")
	}
	return record, nil
}

var _ verify.VerificationReader = scopedVerificationReader{}

// TestVerificationReaderCarriesOwningScope pins the method set/shape: the
// single context-first read takes the owner scope between the context and the
// verification identity, so a store-backed implementation receives the scope
// instead of guessing it.
func TestVerificationReaderCarriesOwningScope(t *testing.T) {
	readerType := reflect.TypeOf((*verify.VerificationReader)(nil)).Elem()
	if readerType.Kind() != reflect.Interface {
		t.Fatalf("verify.VerificationReader is %v; want an interface", readerType.Kind())
	}
	if readerType.NumMethod() != 1 {
		t.Fatalf("verify.VerificationReader has %d methods; want exactly ReadVerification", readerType.NumMethod())
	}
	method, ok := readerType.MethodByName("ReadVerification")
	if !ok {
		t.Fatal("verify.VerificationReader is missing ReadVerification")
	}
	signature := method.Type
	wantIn := []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf(tenancy.Scope{}),
		reflect.TypeOf(verify.VerificationID("")),
	}
	wantOut := []reflect.Type{
		reflect.TypeOf(verify.Record{}),
		reflect.TypeOf((*error)(nil)).Elem(),
	}
	if signature.NumIn() != len(wantIn) {
		t.Fatalf("ReadVerification has %d parameters; want %d (context, scope, id)", signature.NumIn(), len(wantIn))
	}
	for index, want := range wantIn {
		if got := signature.In(index); got != want {
			t.Fatalf("ReadVerification parameter %d = %v; want %v", index, got, want)
		}
	}
	if signature.NumOut() != len(wantOut) {
		t.Fatalf("ReadVerification has %d results; want %d (record, error)", signature.NumOut(), len(wantOut))
	}
	for index, want := range wantOut {
		if got := signature.Out(index); got != want {
			t.Fatalf("ReadVerification result %d = %v; want %v", index, got, want)
		}
	}
}

// TestVerificationReaderOutsideScopeIsNotFound pins the scope containment rule:
// the reference reader resolves a record inside the caller's scope, and an
// unknown identity and a record outside the scope are the same not_found class.
// A foreign record is never forbidden, so the seam exposes no existence oracle.
func TestVerificationReaderOutsideScopeIsNotFound(t *testing.T) {
	scope := sampleScope()
	other := tenancy.Scope{TenantID: id.TenantID("tenant-2"), ProjectID: id.ProjectID("project-2")}
	record := verify.Record{ID: verify.VerificationID("verification-1"), Scope: scope}
	reader := scopedVerificationReader{records: map[verify.VerificationID]verify.Record{
		record.ID: record,
	}}
	ctx := context.Background()

	visible, err := reader.ReadVerification(ctx, scope, record.ID)
	if err != nil {
		t.Fatalf("in-scope read was refused: %v", err)
	}
	if visible.ID != record.ID || visible.Scope != scope {
		t.Fatalf("in-scope read = %#v; want %#v", visible, record)
	}

	_, outside := reader.ReadVerification(ctx, other, record.ID)
	if class, ok := fault.ClassOf(outside); !ok || class != fault.NotFound {
		t.Fatalf("out-of-scope read class = %v; want %q", class, fault.NotFound)
	}
	if class, ok := fault.ClassOf(outside); ok && class == fault.Forbidden {
		t.Fatal("out-of-scope read was forbidden instead of not_found; the seam is an existence oracle")
	}

	_, unknown := reader.ReadVerification(ctx, scope, verify.VerificationID("verification-2"))
	if class, ok := fault.ClassOf(unknown); !ok || class != fault.NotFound {
		t.Fatalf("unknown-identity read class = %v; want %q", class, fault.NotFound)
	}

	outsideClass, _ := fault.ClassOf(outside)
	unknownClass, _ := fault.ClassOf(unknown)
	if outsideClass != unknownClass {
		t.Fatalf("out-of-scope class %q differs from unknown-identity class %q", outsideClass, unknownClass)
	}
}
