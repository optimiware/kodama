// Session schema vectors.
//
// The durable human session file is schema version 2 so a login can keep a
// rotating refresh token and bind the session to one issuer/client/audience/API
// URL. A legacy v1 file is still loaded as an access-token-only session. The
// login/refresh path persists through ReplaceFile only, at mode 0600, and
// logout deletes through the same injected filesystem seam.
package publiccontract

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestSessionRecordShape pins the v2 session member set and JSON names. The
// access token keeps the stable `token` name; refresh_token is additive.
func TestSessionRecordShape(t *testing.T) {
	want := map[string]string{
		"Version":            "version",
		"Kind":               "kind",
		"Token":              "token",
		"ExpiresAt":          "expires_at",
		"RefreshToken":       "refresh_token",
		"Issuer":             "issuer",
		"ClientID":           "client_id",
		"Audience":           "audience",
		"APIURL":             "api_url",
		"TokenEndpoint":      "token_endpoint",
		"RevocationEndpoint": "revocation_endpoint",
	}
	typ := reflect.TypeOf(cli.SessionRecord{})
	seen := make(map[string]bool)
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			t.Errorf("session field %s has no public JSON name", field.Name)
			continue
		}
		seen[name] = true
		if want[field.Name] != name {
			t.Errorf("session field %s JSON name = %q; want %q", field.Name, name, want[field.Name])
		}
	}
	for name := range seen {
		if name == "access_token" {
			t.Error("the access token keeps the documented `token` JSON name")
		}
		if name == "code_verifier" || name == "nonce" || name == "state" {
			t.Errorf("session file must not persist transient OAuth value %q", name)
		}
	}
	if cli.SessionFileVersion != 2 {
		t.Fatalf("SessionFileVersion = %d; want 2", cli.SessionFileVersion)
	}
}

// TestSessionRecordLegacyLoad pins the v1 compatibility rule: a legacy file is
// an access-token-only session, usable until its expiry, with no refresh token.
func TestSessionRecordLegacyLoad(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	legacy := []byte(`{"kind":"session","token":"AT-LEGACY","expires_at":"2026-02-03T06:05:06Z"}`)
	record, err := cli.ParseSessionRecord(legacy)
	if err != nil {
		t.Fatalf("ParseSessionRecord(v1): %v", err)
	}
	if record.Version != 1 {
		t.Errorf("legacy record version = %d; want 1", record.Version)
	}
	if !record.Legacy() {
		t.Error("legacy v1 record is not reported as legacy")
	}
	if record.RefreshToken != "" {
		t.Errorf("legacy record refresh token = %q; want empty", record.RefreshToken)
	}
	if record.Token != "AT-LEGACY" || !record.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("legacy record = %+v", record)
	}
	if !record.Usable(now) {
		t.Error("legacy record is not usable before expiry")
	}
	if record.Usable(now.Add(time.Hour)) {
		t.Error("legacy record is usable at its expiry")
	}
	if record.Usable(now.Add(2 * time.Hour)) {
		t.Error("legacy record is usable after its expiry")
	}
}

// TestSessionRecordStrictDecode pins strict parsing: unknown members, trailing
// data, an unsupported version, a non-session kind, missing v2 binding fields
// and an oversize file are refused.
func TestSessionRecordStrictDecode(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	record := loginRecord(now)
	record.ExpiresAt = now.Add(time.Hour)
	valid := mustMarshalSession(t, record)
	parsed, err := cli.ParseSessionRecord(valid)
	if err != nil {
		t.Fatalf("ParseSessionRecord(v2): %v", err)
	}
	if parsed.Version != 2 || parsed.RefreshToken != record.RefreshToken || parsed.Issuer != record.Issuer ||
		parsed.ClientID != record.ClientID || parsed.Audience != record.Audience ||
		parsed.APIURL != record.APIURL || parsed.TokenEndpoint != record.TokenEndpoint ||
		parsed.RevocationEndpoint != record.RevocationEndpoint {
		t.Fatalf("parsed v2 record = %+v; want %+v", parsed, record)
	}
	if parsed.Legacy() {
		t.Error("v2 record reported as legacy")
	}

	refused := map[string][]byte{
		"unknown member":            sessionWithMember(t, record, "extra", "1"),
		"trailing data":             append(append([]byte(nil), valid...), []byte(`{}`)...),
		"non object":                []byte(`[]`),
		"version 3":                 sessionWithMember(t, record, "version", "3"),
		"zero version":              sessionWithMember(t, record, "version", "0"),
		"service kind":              sessionWithMember(t, record, "kind", `"service"`),
		"missing refresh_token":     sessionWithoutMember(t, record, "refresh_token"),
		"missing issuer":            sessionWithoutMember(t, record, "issuer"),
		"missing client_id":         sessionWithoutMember(t, record, "client_id"),
		"missing audience":          sessionWithoutMember(t, record, "audience"),
		"missing api_url":           sessionWithoutMember(t, record, "api_url"),
		"missing token_endpoint":    sessionWithoutMember(t, record, "token_endpoint"),
		"missing revoke_endpoint":   sessionWithoutMember(t, record, "revocation_endpoint"),
		"empty token":               sessionWithMember(t, record, "token", `""`),
		"wrong expires_at":          sessionWithMember(t, record, "expires_at", `"tomorrow"`),
		"foreign token endpoint":    sessionWithMember(t, record, "token_endpoint", `"https://attacker.example/token"`),
		"foreign revoke endpoint":   sessionWithMember(t, record, "revocation_endpoint", `"https://attacker.example/revoke"`),
		"downgraded token endpoint": sessionWithMember(t, record, "token_endpoint", `"http://auth.example.com/token"`),
	}
	for name, data := range refused {
		if _, err := cli.ParseSessionRecord(data); err == nil {
			t.Errorf("ParseSessionRecord accepted %s: %s", name, data)
		}
	}

	// The access token may be empty only if the record is otherwise unusable;
	// the v2 schema still refuses it.
	if _, err := cli.ParseSessionRecord(sessionWithMember(t, record, "token", `""`)); err == nil {
		t.Error("v2 session with an empty access token accepted")
	}
}

// TestSessionFileBound pins the session-file size bound exactly.
func TestSessionFileBound(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	record := loginRecord(now)
	record.ExpiresAt = now.Add(time.Hour)
	base := mustMarshalSession(t, record)
	if cli.MaxSessionFileBytes <= len(base) {
		t.Fatalf("MaxSessionFileBytes = %d; the fixture is %d bytes", cli.MaxSessionFileBytes, len(base))
	}
	exact := sessionPaddedTo(t, base, cli.MaxSessionFileBytes)
	if _, err := cli.ParseSessionRecord(exact); err != nil {
		t.Fatalf("session file of exactly %d bytes refused: %v", len(exact), err)
	}
	over := sessionPaddedTo(t, base, cli.MaxSessionFileBytes+1)
	if _, err := cli.ParseSessionRecord(over); err == nil {
		t.Fatalf("session file of %d bytes accepted", len(over))
	}
}

// TestSessionRecordBinding pins that a persisted session is bound to the issuer,
// public client, audience and API URL that minted it.
func TestSessionRecordBinding(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	record := loginRecord(now)
	discovery := loginDiscovery()
	if err := record.BoundTo(discovery, discovery.APIURL); err != nil {
		t.Fatalf("matching binding refused: %v", err)
	}

	mutations := map[string]func(*cli.SessionRecord){
		"issuer":   func(record *cli.SessionRecord) { record.Issuer = "https://other.example.com" },
		"client":   func(record *cli.SessionRecord) { record.ClientID = "other-client" },
		"audience": func(record *cli.SessionRecord) { record.Audience = "other-api" },
		"api url":  func(record *cli.SessionRecord) { record.APIURL = "https://other.example.com" },
	}
	for name, mutate := range mutations {
		altered := record
		mutate(&altered)
		if err := altered.BoundTo(discovery, discovery.APIURL); err == nil {
			t.Errorf("BoundTo accepted a session with a foreign %s", name)
		}
	}
	if err := record.BoundTo(discovery, "https://other.example.com"); err == nil {
		t.Error("BoundTo accepted a session issued for another configured API URL")
	}
}

// TestFileSessionStoreRecordPersistence pins the durable v2 rules: SaveRecord
// writes through ReplaceFile at 0600, creates missing parents 0700 without
// changing an existing directory, LoadRecord reads the record back, and
// DeleteRecord removes the file.
func TestFileSessionStoreRecordPersistence(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	files := newReferenceFileSystem()
	files.dirs["/home"] = 0o755
	path := "/home/engineer/.config/kodama/session.json"
	store := cli.FileSessionStore{Path: path, Files: files, Now: func() time.Time { return now }}
	record := loginRecord(now)
	record.ExpiresAt = now.Add(time.Hour)

	if err := store.SaveRecord(context.Background(), record); err != nil {
		t.Fatalf("SaveRecord: %v", err)
	}
	if len(files.tempModes) == 0 {
		t.Fatal("SaveRecord did not persist through ReplaceFile")
	}
	for _, mode := range files.tempModes {
		if mode != 0o600 {
			t.Errorf("temporary session file mode = %o; want 600", mode)
		}
	}
	file, ok := files.files[path]
	if !ok {
		t.Fatal("SaveRecord did not create the session file")
	}
	if file.mode != 0o600 {
		t.Errorf("session file mode = %o; want 600", file.mode)
	}
	for _, dir := range []string{"/home/engineer/.config", "/home/engineer/.config/kodama"} {
		if mode := files.dirs[dir]; mode != 0o700 {
			t.Errorf("created session parent %s mode = %o; want 700", dir, mode)
		}
	}
	if mode := files.dirs["/home"]; mode != 0o755 {
		t.Errorf("existing parent mode = %o; want 755 unchanged", mode)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(file.data, &stored); err != nil {
		t.Fatalf("stored session is not JSON: %v", err)
	}
	if string(stored["version"]) != "2" {
		t.Errorf("stored version = %s; want 2", stored["version"])
	}
	var storedFields map[string]json.RawMessage
	if err := json.Unmarshal(file.data, &storedFields); err != nil {
		t.Fatalf("stored session is not JSON: %v", err)
	}
	var storedRefresh string
	if err := json.Unmarshal(storedFields["refresh_token"], &storedRefresh); err != nil || storedRefresh != "RT-CANARY" {
		t.Errorf("stored refresh token = %q (%v); want RT-CANARY", storedRefresh, err)
	}

	loaded, err := store.LoadRecord(context.Background())
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}
	if loaded.Token != record.Token || loaded.RefreshToken != record.RefreshToken ||
		loaded.Issuer != record.Issuer || loaded.APIURL != record.APIURL {
		t.Fatalf("loaded record = %+v; want %+v", loaded, record)
	}

	if err := store.DeleteRecord(context.Background()); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if _, ok := files.files[path]; ok {
		t.Fatal("DeleteRecord left the session file in place")
	}
	if err := store.DeleteRecord(context.Background()); err != nil {
		t.Fatalf("DeleteRecord(missing) is not idempotent: %v", err)
	}
}

// TestFileSessionStoreRecordRefusals pins that SaveRecord refuses a legacy
// record, an expired record and a record missing its binding.
func TestFileSessionStoreRecordRefusals(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	valid := loginRecord(now)
	valid.ExpiresAt = now.Add(time.Hour)
	cases := map[string]cli.SessionRecord{
		"legacy version": func() cli.SessionRecord {
			record := valid
			record.Version = 1
			return record
		}(),
		"expired": func() cli.SessionRecord {
			record := valid
			record.ExpiresAt = now.Add(-time.Second)
			return record
		}(),
		"missing issuer": func() cli.SessionRecord {
			record := valid
			record.Issuer = ""
			return record
		}(),
		"missing refresh token": func() cli.SessionRecord {
			record := valid
			record.RefreshToken = ""
			return record
		}(),
	}
	for name, record := range cases {
		files := newReferenceFileSystem()
		path := "/home/engineer/.kodama/session.json"
		store := cli.FileSessionStore{Path: path, Files: files, Now: func() time.Time { return now }}
		if err := store.SaveRecord(context.Background(), record); err == nil {
			t.Errorf("SaveRecord accepted %s", name)
		}
		if _, ok := files.files[path]; ok {
			t.Errorf("SaveRecord wrote the file for %s", name)
		}
		if len(files.tempModes) != 0 {
			t.Errorf("SaveRecord used ReplaceFile for %s", name)
		}
	}
}

// TestFileSessionStoreLoadRecordLegacy pins that the file-backed LoadRecord
// accepts a legacy v1 file for compatibility.
func TestFileSessionStoreLoadRecordLegacy(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	files := newReferenceFileSystem()
	path := "/home/engineer/.kodama/session.json"
	files.files[path] = referenceFile{
		data: []byte(`{"kind":"session","token":"AT-LEGACY","expires_at":"2026-02-03T06:05:06Z"}`),
		mode: 0o600,
	}
	store := cli.FileSessionStore{Path: path, Files: files, Now: func() time.Time { return now }}
	record, err := store.LoadRecord(context.Background())
	if err != nil {
		t.Fatalf("LoadRecord(v1): %v", err)
	}
	if !record.Legacy() || record.Token != "AT-LEGACY" || record.RefreshToken != "" {
		t.Fatalf("legacy record = %+v", record)
	}
	if len(files.tempModes) != 0 {
		t.Error("LoadRecord rewrote the legacy v1 file instead of leaving it for a new login")
	}
}

func mustMarshalSession(t *testing.T, record cli.SessionRecord) []byte {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal session record: %v", err)
	}
	return data
}

func sessionWithMember(t *testing.T, record cli.SessionRecord, name, value string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshalSession(t, record), &fields); err != nil {
		t.Fatalf("unmarshal session record: %v", err)
	}
	fields[name] = json.RawMessage(value)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal altered session record: %v", err)
	}
	return data
}

func sessionWithoutMember(t *testing.T, record cli.SessionRecord, name string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshalSession(t, record), &fields); err != nil {
		t.Fatalf("unmarshal session record: %v", err)
	}
	delete(fields, name)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal altered session record: %v", err)
	}
	return data
}

func sessionPaddedTo(t *testing.T, base []byte, size int) []byte {
	t.Helper()
	padding := size - len(base)
	if padding < 0 {
		t.Fatalf("requested size %d is smaller than the fixture", size)
	}
	body := append([]byte{'{'}, bytes.Repeat([]byte(" "), padding)...)
	body = append(body, base[1:]...)
	if len(body) != size {
		t.Fatalf("padded session body is %d bytes; want %d", len(body), size)
	}
	return body
}
