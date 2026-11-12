// Token derivation, verification and tenant-key API pinned by these tests.
//
// The declared values and canonical MAC input live in types.go; this file pins
// the derivation behavior without redefining the encoding:
//
//	func DeriveExecutionToken(key []byte, ExecutionClaims) (string, error)
//	func DeriveServiceToken(key []byte, ServiceClaims) (string, error)
//	func VerifyExecutionToken(key []byte, presented string, ExecutionClaims, time.Time) error
//	func VerifyServiceToken(key []byte, presented string, ServiceClaims, time.Time) error
//
//	type TenantKeyMaterial struct {
//	    Ciphertext []byte
//	    WrappedKey []byte
//	    WrapNonce  []byte
//	    KeyRef     string
//	    KeyVersion uint64
//	}
//	func NewTenantKey(id.TenantID, uint64, TenantKeyMaterial) (TenantKey, error)
//	func NextTenantKey(TenantKey, TenantKeyMaterial) (TenantKeyWrite, error)
//
// Rules pinned by these tests (S05 §3):
//   - The canonical binary MAC input and base64url-no-padding token strings are
//     exactly the golden vectors in test/contracts/domain/testdata.
//   - A token of the wrong prefix/domain/kind, wrong tenant/key, wrong
//     principal id, missing/short nonce, malformed base64, truncated MAC or
//     reached expiry is refused. The id in the token is only a lookup hint
//     until the MAC check succeeds.
//   - The tenant derivation key has at least 256 bits and is never stored in
//     plaintext; TenantKey maps to the structured tenants.derivation_key JSON
//     object and a rotation advances the key version by exactly one.
package token_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/token"
)

const goldenVectorPath = "../../../test/contracts/domain/testdata/tokens.json"

type tokenVectorCase struct {
	Kind        string `json:"kind"`
	TenantID    string `json:"tenant_id"`
	PrincipalID string `json:"principal_id"`
	NonceHex    string `json:"nonce_hex"`
	ExpiryUnix  int64  `json:"expiry_unix"`
	MACInputHex string `json:"mac_input_hex"`
	Token       string `json:"token"`
}

type tokenVectorFile struct {
	TenantKeyHex string            `json:"tenant_key_hex"`
	Cases        []tokenVectorCase `json:"cases"`
}

func loadVectors(t *testing.T) ([]byte, tokenVectorFile) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenVectorPath))
	if err != nil {
		t.Fatalf("read golden token vectors: %v", err)
	}
	var vectors tokenVectorFile
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("decode golden token vectors: %v", err)
	}
	key, err := hex.DecodeString(vectors.TenantKeyHex)
	if err != nil {
		t.Fatalf("decode golden tenant key: %v", err)
	}
	if len(key) != token.MinNonceBytes {
		t.Fatalf("golden tenant key = %d bytes; want %d", len(key), token.MinNonceBytes)
	}
	if len(vectors.Cases) != 2 {
		t.Fatalf("golden token cases = %d; want 2", len(vectors.Cases))
	}
	return key, vectors
}

func executionClaimsFrom(vector tokenVectorCase, nonce []byte) token.ExecutionClaims {
	return token.ExecutionClaims{
		TenantID:    id.TenantID(vector.TenantID),
		ExecutionID: id.ExecutionID(vector.PrincipalID),
		Nonce:       nonce,
		Expiry:      time.Unix(vector.ExpiryUnix, 0).UTC(),
	}
}

func serviceClaimsFrom(vector tokenVectorCase, nonce []byte) token.ServiceClaims {
	return token.ServiceClaims{
		TenantID:  id.TenantID(vector.TenantID),
		ServiceID: service.ServiceID(vector.PrincipalID),
		Nonce:     nonce,
		Expiry:    time.Unix(vector.ExpiryUnix, 0).UTC(),
	}
}

func testKey() []byte {
	return bytes.Repeat([]byte{0x5a}, token.MinNonceBytes)
}

func otherKey() []byte {
	return bytes.Repeat([]byte{0xa5}, token.MinNonceBytes)
}

func testNonce() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

func testScope() (tenantID id.TenantID, executionID id.ExecutionID, serviceID service.ServiceID) {
	return id.TenantID("tenant-a"), id.ExecutionID("exec-1"), service.ServiceID("svc-1")
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func requireRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; want refusal")
	}
}

// TestDeriveExecutionTokenGoldenVector pins the exact execution token string
// and canonical MAC input from the golden vector file.
func TestDeriveExecutionTokenGoldenVector(t *testing.T) {
	key, vectors := loadVectors(t)
	vector := vectors.Cases[0]
	if vector.Kind != "execution" {
		t.Fatalf("first vector kind = %q; want execution", vector.Kind)
	}
	nonce, err := hex.DecodeString(vector.NonceHex)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	claims := executionClaimsFrom(vector, nonce)

	input, err := token.ExecutionMACInput(claims)
	if err != nil {
		t.Fatalf("ExecutionMACInput: %v", err)
	}
	if got := hex.EncodeToString(input); got != vector.MACInputHex {
		t.Fatalf("MAC input = %s; want %s", got, vector.MACInputHex)
	}

	presented, err := token.DeriveExecutionToken(key, claims)
	if err != nil {
		t.Fatalf("DeriveExecutionToken: %v", err)
	}
	if presented != vector.Token {
		t.Fatalf("token = %q; want %q", presented, vector.Token)
	}
	if !strings.HasPrefix(presented, token.ExecutionPrefix) {
		t.Fatalf("token = %q; missing execution prefix", presented)
	}
	if strings.Contains(presented, "=") {
		t.Fatalf("token = %q; base64url must be unpadded", presented)
	}
	if err := token.VerifyExecutionToken(key, presented, claims, time.Unix(vector.ExpiryUnix-1, 0).UTC()); err != nil {
		t.Fatalf("VerifyExecutionToken(current): %v", err)
	}
}

// TestDeriveServiceTokenGoldenVector pins the exact service token string and
// canonical MAC input from the golden vector file.
func TestDeriveServiceTokenGoldenVector(t *testing.T) {
	key, vectors := loadVectors(t)
	vector := vectors.Cases[1]
	if vector.Kind != "service" {
		t.Fatalf("second vector kind = %q; want service", vector.Kind)
	}
	nonce, err := hex.DecodeString(vector.NonceHex)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	claims := serviceClaimsFrom(vector, nonce)

	input, err := token.ServiceMACInput(claims)
	if err != nil {
		t.Fatalf("ServiceMACInput: %v", err)
	}
	if got := hex.EncodeToString(input); got != vector.MACInputHex {
		t.Fatalf("MAC input = %s; want %s", got, vector.MACInputHex)
	}

	presented, err := token.DeriveServiceToken(key, claims)
	if err != nil {
		t.Fatalf("DeriveServiceToken: %v", err)
	}
	if presented != vector.Token {
		t.Fatalf("token = %q; want %q", presented, vector.Token)
	}
	if !strings.HasPrefix(presented, token.ServicePrefix) {
		t.Fatalf("token = %q; missing service prefix", presented)
	}
	if strings.Contains(presented, "=") {
		t.Fatalf("token = %q; base64url must be unpadded", presented)
	}
	if err := token.VerifyServiceToken(key, presented, claims, time.Unix(vector.ExpiryUnix-1, 0).UTC()); err != nil {
		t.Fatalf("VerifyServiceToken(current): %v", err)
	}
}

// TestDeriveTokenRejectsInvalidClaims proves ambiguous inputs are refused
// before any MAC is produced: short nonce, non-positive expiry, malformed ids
// and a tenant key shorter than 256 bits.
func TestDeriveTokenRejectsInvalidClaims(t *testing.T) {
	tenantID, executionID, serviceID := testScope()
	expiry := time.Unix(1_700_000_000, 0).UTC()

	validExecution := token.ExecutionClaims{
		TenantID:    tenantID,
		ExecutionID: executionID,
		Nonce:       testNonce(),
		Expiry:      expiry,
	}
	validService := token.ServiceClaims{
		TenantID:  tenantID,
		ServiceID: serviceID,
		Nonce:     testNonce(),
		Expiry:    expiry,
	}

	cases := []struct {
		name string
		key  []byte
		exec *token.ExecutionClaims
		svc  *token.ServiceClaims
	}{
		{name: "short execution nonce", key: testKey(), exec: new(token.ExecutionClaims{TenantID: tenantID, ExecutionID: executionID, Nonce: []byte("short"), Expiry: expiry})},
		{name: "zero execution expiry", key: testKey(), exec: new(token.ExecutionClaims{TenantID: tenantID, ExecutionID: executionID, Nonce: testNonce()})},
		{name: "invalid execution tenant", key: testKey(), exec: new(token.ExecutionClaims{TenantID: "../tenant", ExecutionID: executionID, Nonce: testNonce(), Expiry: expiry})},
		{name: "invalid execution id", key: testKey(), exec: new(token.ExecutionClaims{TenantID: tenantID, ExecutionID: "../exec", Nonce: testNonce(), Expiry: expiry})},
		{name: "short execution key", key: bytes.Repeat([]byte{0x01}, token.MinNonceBytes-1), exec: &validExecution},
		{name: "short service nonce", key: testKey(), svc: new(token.ServiceClaims{TenantID: tenantID, ServiceID: serviceID, Nonce: []byte("short"), Expiry: expiry})},
		{name: "zero service expiry", key: testKey(), svc: new(token.ServiceClaims{TenantID: tenantID, ServiceID: serviceID, Nonce: testNonce()})},
		{name: "invalid service id", key: testKey(), svc: new(token.ServiceClaims{TenantID: tenantID, ServiceID: service.ServiceID("../svc"), Nonce: testNonce(), Expiry: expiry})},
		{name: "short service key", key: bytes.Repeat([]byte{0x01}, token.MinNonceBytes-1), svc: &validService},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.exec != nil {
				_, err := token.DeriveExecutionToken(testCase.key, *testCase.exec)
				requireClass(t, err, fault.InvalidInput)
				return
			}
			_, err := token.DeriveServiceToken(testCase.key, *testCase.svc)
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

// TestVerifyExecutionTokenRejectsWrongTypeTenantMACAndExpiry pins every
// authentication refusal around the constant-time MAC check.
func TestVerifyExecutionTokenRejectsWrongTypeTenantMACAndExpiry(t *testing.T) {
	tenantID, executionID, serviceID := testScope()
	expiry := time.Unix(1_700_000_000, 0).UTC()
	claims := token.ExecutionClaims{TenantID: tenantID, ExecutionID: executionID, Nonce: testNonce(), Expiry: expiry}
	presented, err := token.DeriveExecutionToken(testKey(), claims)
	if err != nil {
		t.Fatalf("DeriveExecutionToken: %v", err)
	}
	now := expiry.Add(-time.Minute)

	if err := token.VerifyExecutionToken(testKey(), presented, claims, now); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}

	wrongType, err := token.DeriveServiceToken(testKey(), token.ServiceClaims{TenantID: tenantID, ServiceID: serviceID, Nonce: testNonce(), Expiry: expiry})
	if err != nil {
		t.Fatalf("DeriveServiceToken: %v", err)
	}
	requireRefusal(t, token.VerifyExecutionToken(testKey(), wrongType, claims, now))

	wrongTenant := claims
	wrongTenant.TenantID = id.TenantID("tenant-b")
	requireRefusal(t, token.VerifyExecutionToken(testKey(), presented, wrongTenant, now))

	wrongPrincipal := claims
	wrongPrincipal.ExecutionID = id.ExecutionID("exec-2")
	requireRefusal(t, token.VerifyExecutionToken(testKey(), presented, wrongPrincipal, now))

	requireRefusal(t, token.VerifyExecutionToken(otherKey(), presented, claims, now))

	tampered := flipLastCharacter(presented)
	requireRefusal(t, token.VerifyExecutionToken(testKey(), tampered, claims, now))

	// The expiry boundary is inclusive: at the expiry instant the credential is
	// no longer current.
	requireRefusal(t, token.VerifyExecutionToken(testKey(), presented, claims, expiry))
	requireRefusal(t, token.VerifyExecutionToken(testKey(), presented, claims, expiry.Add(time.Second)))
}

// TestVerifyServiceTokenRejectsWrongTypeTenantMACAndExpiry is the service
// counterpart of the execution refusal matrix.
func TestVerifyServiceTokenRejectsWrongTypeTenantMACAndExpiry(t *testing.T) {
	tenantID, executionID, serviceID := testScope()
	expiry := time.Unix(1_700_000_000, 0).UTC()
	claims := token.ServiceClaims{TenantID: tenantID, ServiceID: serviceID, Nonce: testNonce(), Expiry: expiry}
	presented, err := token.DeriveServiceToken(testKey(), claims)
	if err != nil {
		t.Fatalf("DeriveServiceToken: %v", err)
	}
	now := expiry.Add(-time.Minute)

	if err := token.VerifyServiceToken(testKey(), presented, claims, now); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}

	wrongType, err := token.DeriveExecutionToken(testKey(), token.ExecutionClaims{TenantID: tenantID, ExecutionID: executionID, Nonce: testNonce(), Expiry: expiry})
	if err != nil {
		t.Fatalf("DeriveExecutionToken: %v", err)
	}
	requireRefusal(t, token.VerifyServiceToken(testKey(), wrongType, claims, now))

	wrongTenant := claims
	wrongTenant.TenantID = id.TenantID("tenant-b")
	requireRefusal(t, token.VerifyServiceToken(testKey(), presented, wrongTenant, now))

	wrongPrincipal := claims
	wrongPrincipal.ServiceID = service.ServiceID("svc-2")
	requireRefusal(t, token.VerifyServiceToken(testKey(), presented, wrongPrincipal, now))

	requireRefusal(t, token.VerifyServiceToken(otherKey(), presented, claims, now))
	requireRefusal(t, token.VerifyServiceToken(testKey(), flipLastCharacter(presented), claims, now))
	requireRefusal(t, token.VerifyServiceToken(testKey(), presented, claims, expiry))
}

// TestVerifyTokenRejectsAbsentNonceAndMalformedEncoding proves missing fields
// and malformed token framing are refusals, not ambiguous MAC inputs.
func TestVerifyTokenRejectsAbsentNonceAndMalformedEncoding(t *testing.T) {
	tenantID, executionID, _ := testScope()
	expiry := time.Unix(1_700_000_000, 0).UTC()
	claims := token.ExecutionClaims{TenantID: tenantID, ExecutionID: executionID, Nonce: testNonce(), Expiry: expiry}
	presented, err := token.DeriveExecutionToken(testKey(), claims)
	if err != nil {
		t.Fatalf("DeriveExecutionToken: %v", err)
	}
	now := expiry.Add(-time.Minute)

	absent := claims
	absent.Nonce = nil
	requireRefusal(t, token.VerifyExecutionToken(testKey(), presented, absent, now))

	malformed := []string{
		"",
		"kodex.exec-1",
		"kodex.exec-1.",
		"kodex.exec-1.!!!!",
		"kodex.exec-1." + strings.Repeat("A", 43) + ".extra",
		"kodsvc.exec-1." + strings.Repeat("A", 43),
	}
	for _, candidate := range malformed {
		requireRefusal(t, token.VerifyExecutionToken(testKey(), candidate, claims, now))
	}
}

// TestVerifyTokenRejectsTruncatedOrExtendedMAC proves the MAC is compared as a
// fixed-size value: a truncated or padded MAC never authenticates.
func TestVerifyTokenRejectsTruncatedOrExtendedMAC(t *testing.T) {
	tenantID, executionID, _ := testScope()
	expiry := time.Unix(1_700_000_000, 0).UTC()
	claims := token.ExecutionClaims{TenantID: tenantID, ExecutionID: executionID, Nonce: testNonce(), Expiry: expiry}
	presented, err := token.DeriveExecutionToken(testKey(), claims)
	if err != nil {
		t.Fatalf("DeriveExecutionToken: %v", err)
	}
	now := expiry.Add(-time.Minute)

	parts := strings.Split(presented, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q; want three segments", presented)
	}
	requireRefusal(t, token.VerifyExecutionToken(testKey(), "kodex."+parts[1]+"."+parts[2][:8], claims, now))
	requireRefusal(t, token.VerifyExecutionToken(testKey(), presented+"A", claims, now))
}

// TestTenantKeyCreateAndStructuredMapping pins the structured encrypted key
// object and its JSON member names, which map to tenants.derivation_key jsonb.
func TestTenantKeyCreateAndStructuredMapping(t *testing.T) {
	tenantID := id.TenantID("tenant-a")
	material := token.TenantKeyMaterial{
		Ciphertext: []byte("encrypted-derivation-key"),
		WrappedKey: []byte("wrapped-data-key"),
		WrapNonce:  []byte("wrap-nonce-1"),
		KeyRef:     "kms/tenant-a",
		KeyVersion: 2,
	}
	key, err := token.NewTenantKey(tenantID, 1, material)
	if err != nil {
		t.Fatalf("NewTenantKey: %v", err)
	}
	if key.TenantID != tenantID || key.Version != 1 {
		t.Fatalf("key identity = %s/%d; want tenant-a/1", key.TenantID, key.Version)
	}
	if err := key.Validate(); err != nil {
		t.Fatalf("NewTenantKey produced an invalid key: %v", err)
	}

	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshal tenant key: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("tenant key is not a JSON object: %v", err)
	}
	for _, member := range []string{"tenant_id", "version", "ciphertext", "wrapped_key", "wrap_nonce", "key_ref", "key_version"} {
		if _, ok := object[member]; !ok {
			t.Errorf("tenant key JSON is missing member %q", member)
		}
	}
	for _, forbidden := range []string{"key", "plaintext", "derivation_key", "token", "token_hash"} {
		if _, ok := object[forbidden]; ok {
			t.Errorf("tenant key JSON must not carry member %q", forbidden)
		}
	}
	if bytes.Contains(encoded, []byte("plaintext-derivation-key")) {
		t.Fatal("tenant key JSON contains plaintext key material")
	}
}

// TestTenantKeyCreateRejectsIncompleteMaterial proves an incomplete encrypted
// key is refused rather than committed with a NULL or plaintext member.
func TestTenantKeyCreateRejectsIncompleteMaterial(t *testing.T) {
	tenantID := id.TenantID("tenant-a")
	valid := token.TenantKeyMaterial{
		Ciphertext: []byte("ciphertext"),
		WrappedKey: []byte("wrapped"),
		WrapNonce:  []byte("nonce"),
		KeyRef:     "kms/tenant-a",
		KeyVersion: 1,
	}

	cases := []struct {
		name     string
		tenantID id.TenantID
		version  uint64
		material token.TenantKeyMaterial
	}{
		{name: "invalid tenant", tenantID: id.TenantID("../tenant"), version: 1, material: valid},
		{name: "zero key version", tenantID: tenantID, version: 0, material: valid},
		{name: "empty ciphertext", tenantID: tenantID, version: 1, material: withoutCiphertext(valid)},
		{name: "empty wrapped key", tenantID: tenantID, version: 1, material: withoutWrappedKey(valid)},
		{name: "empty wrap nonce", tenantID: tenantID, version: 1, material: withoutWrapNonce(valid)},
		{name: "empty key ref", tenantID: tenantID, version: 1, material: withoutKeyRef(valid)},
		{name: "zero provider version", tenantID: tenantID, version: 1, material: withoutProviderVersion(valid)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := token.NewTenantKey(testCase.tenantID, testCase.version, testCase.material)
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

func withoutCiphertext(m token.TenantKeyMaterial) token.TenantKeyMaterial {
	m.Ciphertext = nil
	return m
}
func withoutWrappedKey(m token.TenantKeyMaterial) token.TenantKeyMaterial {
	m.WrappedKey = nil
	return m
}
func withoutWrapNonce(m token.TenantKeyMaterial) token.TenantKeyMaterial { m.WrapNonce = nil; return m }
func withoutKeyRef(m token.TenantKeyMaterial) token.TenantKeyMaterial    { m.KeyRef = ""; return m }
func withoutProviderVersion(m token.TenantKeyMaterial) token.TenantKeyMaterial {
	m.KeyVersion = 0
	return m
}

// TestTenantKeyRotationAdvancesVersionAndRefusesStale pins the rotation write:
// the next key is exactly current.Version+1 with expected current.Version, and
// a replayed/stale write is refused.
func TestTenantKeyRotationAdvancesVersionAndRefusesStale(t *testing.T) {
	tenantID := id.TenantID("tenant-a")
	current, err := token.NewTenantKey(tenantID, 2, token.TenantKeyMaterial{
		Ciphertext: []byte("ciphertext-2"),
		WrappedKey: []byte("wrapped-2"),
		WrapNonce:  []byte("nonce-2"),
		KeyRef:     "kms/tenant-a",
		KeyVersion: 3,
	})
	if err != nil {
		t.Fatalf("NewTenantKey: %v", err)
	}

	write, err := token.NextTenantKey(current, token.TenantKeyMaterial{
		Ciphertext: []byte("ciphertext-3"),
		WrappedKey: []byte("wrapped-3"),
		WrapNonce:  []byte("nonce-3"),
		KeyRef:     "kms/tenant-a",
		KeyVersion: 4,
	})
	if err != nil {
		t.Fatalf("NextTenantKey: %v", err)
	}
	if write.TenantID != tenantID {
		t.Fatalf("write tenant = %s; want tenant-a", write.TenantID)
	}
	if write.ExpectedVersion != 2 {
		t.Fatalf("ExpectedVersion = %d; want 2", write.ExpectedVersion)
	}
	if write.Key.Version != 3 {
		t.Fatalf("next key version = %d; want 3", write.Key.Version)
	}
	if err := write.Validate(); err != nil {
		t.Fatalf("NextTenantKey produced an invalid write: %v", err)
	}

	stale := write
	stale.ExpectedVersion = 3
	requireClass(t, stale.Validate(), fault.InvalidInput)

	mismatched := write
	mismatched.TenantID = id.TenantID("tenant-b")
	requireClass(t, mismatched.Validate(), fault.InvalidInput)

	nonAdvancing := write
	nonAdvancing.Key.Version = 2
	requireClass(t, nonAdvancing.Validate(), fault.InvalidInput)
}

// TestTenantKeyRotationInvalidatesOldTokens proves a rotation to a new
// derivation key refuses every token derived under the old key, while a token
// derived under the new key verifies.
func TestTenantKeyRotationInvalidatesOldTokens(t *testing.T) {
	tenantID, executionID, _ := testScope()
	expiry := time.Unix(1_700_000_000, 0).UTC()
	claims := token.ExecutionClaims{TenantID: tenantID, ExecutionID: executionID, Nonce: testNonce(), Expiry: expiry}
	now := expiry.Add(-time.Minute)

	oldToken, err := token.DeriveExecutionToken(testKey(), claims)
	if err != nil {
		t.Fatalf("DeriveExecutionToken(old): %v", err)
	}
	if err := token.VerifyExecutionToken(testKey(), oldToken, claims, now); err != nil {
		t.Fatalf("old token did not verify under the old key: %v", err)
	}
	requireRefusal(t, token.VerifyExecutionToken(otherKey(), oldToken, claims, now))

	newToken, err := token.DeriveExecutionToken(otherKey(), claims)
	if err != nil {
		t.Fatalf("DeriveExecutionToken(new): %v", err)
	}
	if err := token.VerifyExecutionToken(otherKey(), newToken, claims, now); err != nil {
		t.Fatalf("new token did not verify under the new key: %v", err)
	}
	requireRefusal(t, token.VerifyExecutionToken(testKey(), newToken, claims, now))
}

func flipLastCharacter(value string) string {
	if value == "" {
		return "x"
	}
	last := value[len(value)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return value[:len(value)-1] + string(replacement)
}
