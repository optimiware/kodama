package gitpushhttp_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	gitpushhttp "github.com/optimiware/kodama/internal/gitpush/http"
)

// jsonShape lists a struct's wire members with their nullability and element
// shape, so a generated type and the contract's can be compared.
func jsonShape(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[name] = kindOf(f.Type)
	}
	return out
}

func kindOf(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + kindOf(t.Elem())
	case reflect.Slice:
		return "[]" + kindOf(t.Elem())
	case reflect.Struct:
		if t == reflect.TypeOf(time.Time{}) {
			return "string" // format: date-time; the contract carries the Timestamp string
		}
		return "struct"
	}
	return t.Kind().String()
}

// Test_unit_gitpush_http_types_match_contract proves that the types generated
// from api/git-pushes.yaml stay identical in shape to the contract's.
func Test_unit_gitpush_http_types_match_contract(t *testing.T) {
	for name, pair := range map[string][2]reflect.Type{
		"PushEntry":      {reflect.TypeOf(gitv1.PushEntry{}), reflect.TypeOf(gitpushhttp.PushEntry{})},
		"PushRecord":     {reflect.TypeOf(gitv1.PushRecord{}), reflect.TypeOf(gitpushhttp.PushRecord{})},
		"PushRecordList": {reflect.TypeOf(gitv1.PushRecordList{}), reflect.TypeOf(gitpushhttp.PushRecordList{})},
		"Actor":          {reflect.TypeOf(gitv1.Actor{}), reflect.TypeOf(gitpushhttp.PushActor{})},
	} {
		want, got := jsonShape(pair[0]), jsonShape(pair[1])
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: contract %v, generated %v", name, want, got)
		}
	}
	// A contract record round-trips through the generated types without loss.
	rec := gitv1.PushRecord{PushID: "push_1", RepositoryID: "r_1", ReceiveID: "rcv_1", GrantID: "g_1", Status: gitv1.PushApplied, CreatedAt: "2026-10-30T10:00:00Z",
		Actor:   gitv1.Actor{Type: gitv1.ActorUser, ID: "u_1"},
		Entries: []gitv1.PushEntry{{Ref: "refs/heads/x", Old: strings.Repeat("1", 40), New: strings.Repeat("2", 40), Kind: gitv1.KindFastForward, State: gitv1.EntryApplied}}}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var gen gitpushhttp.PushRecord
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&gen); err != nil {
		t.Fatal(err)
	}
	back, _ := json.Marshal(gen)
	var a, b any
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("round trip:\n%s\n%s", raw, back)
	}
}
