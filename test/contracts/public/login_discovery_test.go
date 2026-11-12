// Unauthenticated discovery vectors.
//
// The kodactl login flow is configured with the Kodama API URL only. It reads
// one unauthenticated public discovery document from the fixed
// /.well-known/kodama-configuration path, outside the 51-operation registry.
// The document is strictly decoded, bounded, validated and carries no secret
// or JWKS material.
package publiccontract

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
)

// loginDiscoveryJSON is the canonical valid discovery document body.
const loginDiscoveryJSON = `{
  "issuer": "https://auth.example.com",
  "client_id": "kodactl",
  "audience": "kodama-api",
  "authorization_endpoint": "https://auth.example.com/authorize",
  "token_endpoint": "https://auth.example.com/token",
  "device_authorization_endpoint": "https://auth.example.com/device",
  "revocation_endpoint": "https://auth.example.com/revoke",
  "scopes": ["openid", "profile", "email", "offline_access"],
  "api_url": "https://api.example.com"
}`

// TestDiscoveryDocumentShape pins the public discovery member set and its
// snake_case JSON names. No member may carry secret or JWKS material.
func TestDiscoveryDocumentShape(t *testing.T) {
	want := map[string]string{
		"Issuer":                      "issuer",
		"ClientID":                    "client_id",
		"Audience":                    "audience",
		"AuthorizationEndpoint":       "authorization_endpoint",
		"TokenEndpoint":               "token_endpoint",
		"DeviceAuthorizationEndpoint": "device_authorization_endpoint",
		"RevocationEndpoint":          "revocation_endpoint",
		"Scopes":                      "scopes",
		"APIURL":                      "api_url",
	}
	typ := reflect.TypeOf(api.DiscoveryDocument{})
	seen := make(map[string]bool)
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			t.Errorf("discovery field %s has no public JSON name", field.Name)
			continue
		}
		seen[name] = true
		if want[field.Name] != name {
			t.Errorf("discovery field %s JSON name = %q; want %q", field.Name, name, want[field.Name])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("discovery document exposes %d members; want %d", len(seen), len(want))
	}
	for name := range seen {
		if name == "client_secret" || name == "secret" || name == "jwks" || name == "jwks_uri" {
			t.Errorf("discovery document must not expose secret/JWKS member %q", name)
		}
	}

	encoded, err := json.Marshal(api.DiscoveryDocument{})
	if err != nil {
		t.Fatalf("marshal discovery document: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("discovery document is not a JSON object: %v", err)
	}
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, name)
	}
	sort.Strings(names)
	wantNames := make([]string, 0, len(want))
	for _, name := range want {
		wantNames = append(wantNames, name)
	}
	sort.Strings(wantNames)
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("discovery JSON members = %v; want %v", names, wantNames)
	}

	scopes, ok := typ.FieldByName("Scopes")
	if !ok || scopes.Type.Kind() != reflect.Slice || scopes.Type.Elem().Kind() != reflect.String {
		t.Errorf("Scopes member = %v; want a string slice", scopes.Type)
	}
}

// TestDiscoveryDocumentStrictDecode pins the strict decoder: unknown members,
// trailing data, wrong JSON types, an oversize body and missing required
// members are refused.
func TestDiscoveryDocumentStrictDecode(t *testing.T) {
	document, err := api.DecodeDiscoveryDocument([]byte(loginDiscoveryJSON))
	if err != nil {
		t.Fatalf("canonical discovery document refused: %v", err)
	}
	if document.Issuer != "https://auth.example.com" || document.ClientID != "kodactl" ||
		document.Audience != "kodama-api" || document.APIURL != "https://api.example.com" {
		t.Fatalf("decoded discovery document = %+v", document)
	}
	if len(document.Scopes) != 4 || document.Scopes[3] != "offline_access" {
		t.Fatalf("decoded scopes = %v", document.Scopes)
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("canonical discovery document does not validate: %v", err)
	}

	refused := map[string]string{
		"unknown member":    strings.Replace(loginDiscoveryJSON, `"api_url"`, `"client_secret": "shh", "api_url"`, 1),
		"jwks member":       strings.Replace(loginDiscoveryJSON, `"api_url"`, `"jwks": {"keys": []}, "api_url"`, 1),
		"trailing data":     loginDiscoveryJSON + `{}`,
		"non object":        `[]`,
		"empty":             ``,
		"wrong issuer type": strings.Replace(loginDiscoveryJSON, `"issuer": "https://auth.example.com"`, `"issuer": 5`, 1),
		"wrong scope type":  strings.Replace(loginDiscoveryJSON, `["openid", "profile", "email", "offline_access"]`, `"openid"`, 1),
		"missing issuer":    strings.Replace(loginDiscoveryJSON, `"issuer": "https://auth.example.com",`, ``, 1),
		"missing client_id": strings.Replace(loginDiscoveryJSON, `"client_id": "kodactl",`, ``, 1),
		"missing audience":  strings.Replace(loginDiscoveryJSON, `"audience": "kodama-api",`, ``, 1),
		"missing endpoints": strings.Replace(loginDiscoveryJSON, `"token_endpoint": "https://auth.example.com/token",`, ``, 1),
		"missing scopes":    strings.Replace(loginDiscoveryJSON, `"scopes": ["openid", "profile", "email", "offline_access"],`, ``, 1),
		"missing api_url": strings.Replace(loginDiscoveryJSON, `,
  "api_url": "https://api.example.com"`, ``, 1),
		"empty client_id": strings.Replace(loginDiscoveryJSON, `"client_id": "kodactl"`, `"client_id": ""`, 1),
		"empty issuer":    strings.Replace(loginDiscoveryJSON, `"issuer": "https://auth.example.com"`, `"issuer": ""`, 1),
		"empty scopes":    strings.Replace(loginDiscoveryJSON, `["openid", "profile", "email", "offline_access"]`, `[]`, 1),
	}
	for name, body := range refused {
		if _, err := api.DecodeDiscoveryDocument([]byte(body)); err == nil {
			t.Errorf("DecodeDiscoveryDocument accepted %s: %s", name, body)
		}
	}
}

// TestDiscoveryDocumentEndpointValidation pins the transport origin rules: an
// endpoint must be https, except a loopback fixture on exactly 127.0.0.1 or
// [::1], and every endpoint must share the issuer's origin.
func TestDiscoveryDocumentEndpointValidation(t *testing.T) {
	replace := func(old, new string) string {
		t.Helper()
		if !strings.Contains(loginDiscoveryJSON, old) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return strings.Replace(loginDiscoveryJSON, old, new, 1)
	}
	refused := map[string]string{
		"http token endpoint":     replace(`"token_endpoint": "https://auth.example.com/token"`, `"token_endpoint": "http://auth.example.com/token"`),
		"http localhost endpoint": replace(`"token_endpoint": "https://auth.example.com/token"`, `"token_endpoint": "http://localhost:9000/token"`),
		"http issuer":             replace(`"issuer": "https://auth.example.com"`, `"issuer": "http://auth.example.com"`),
		"foreign token origin":    replace(`"token_endpoint": "https://auth.example.com/token"`, `"token_endpoint": "https://attacker.example/token"`),
		"foreign revoke origin":   replace(`"revocation_endpoint": "https://auth.example.com/revoke"`, `"revocation_endpoint": "https://attacker.example/revoke"`),
		"userinfo endpoint":       replace(`"token_endpoint": "https://auth.example.com/token"`, `"token_endpoint": "https://user:pass@auth.example.com/token"`),
		"opaque endpoint":         replace(`"token_endpoint": "https://auth.example.com/token"`, `"token_endpoint": "auth.example.com/token"`),
		"fragment endpoint":       replace(`"authorization_endpoint": "https://auth.example.com/authorize"`, `"authorization_endpoint": "https://auth.example.com/authorize#x"`),
		"http api_url":            replace(`"api_url": "https://api.example.com"`, `"api_url": "http://api.example.com"`),
	}
	for name, body := range refused {
		if _, err := api.DecodeDiscoveryDocument([]byte(body)); err == nil {
			t.Errorf("DecodeDiscoveryDocument accepted %s", name)
		}
	}

	// Loopback development fixtures may use plain HTTP on the literal numeric
	// loopback hosts only; a same-origin document validates.
	for _, origin := range []string{"http://127.0.0.1:9000", "http://[::1]:9000"} {
		body := `{
  "issuer": "` + origin + `",
  "client_id": "kodactl",
  "audience": "kodama-api",
  "authorization_endpoint": "` + origin + `/authorize",
  "token_endpoint": "` + origin + `/token",
  "device_authorization_endpoint": "` + origin + `/device",
  "revocation_endpoint": "` + origin + `/revoke",
  "scopes": ["openid", "offline_access"],
  "api_url": "` + origin + `"
}`
		if _, err := api.DecodeDiscoveryDocument([]byte(body)); err != nil {
			t.Errorf("loopback discovery fixture %s refused: %v", origin, err)
		}
	}

	// The API URL is a client configuration target, not an authorization
	// endpoint, so it may live on another https origin than the issuer.
	otherAPI := replace(`"api_url": "https://api.example.com"`, `"api_url": "https://api.example.net"`)
	if _, err := api.DecodeDiscoveryDocument([]byte(otherAPI)); err != nil {
		t.Errorf("discovery with a separate https api_url refused: %v", err)
	}
}

// TestDiscoveryDocumentBound pins the exact body bound: a valid document of
// exactly MaxDiscoveryBytes is accepted and one byte more is refused.
func TestDiscoveryDocumentBound(t *testing.T) {
	if api.MaxDiscoveryBytes <= len(loginDiscoveryJSON) {
		t.Fatalf("MaxDiscoveryBytes = %d; the canonical document is %d bytes", api.MaxDiscoveryBytes, len(loginDiscoveryJSON))
	}
	exact := discoveryPaddedTo(t, api.MaxDiscoveryBytes)
	if _, err := api.DecodeDiscoveryDocument(exact); err != nil {
		t.Fatalf("discovery document of exactly %d bytes refused: %v", len(exact), err)
	}
	over := discoveryPaddedTo(t, api.MaxDiscoveryBytes+1)
	if _, err := api.DecodeDiscoveryDocument(over); err == nil {
		t.Fatalf("discovery document of %d bytes accepted", len(over))
	}
}

// discoveryPaddedTo inflates the canonical document with JSON whitespace so the
// size bound is exercised without an unknown member or trailing data.
func discoveryPaddedTo(t *testing.T, size int) []byte {
	t.Helper()
	padding := size - len(loginDiscoveryJSON)
	if padding < 0 {
		t.Fatalf("requested size %d is smaller than the fixture", size)
	}
	body := "{" + strings.Repeat(" ", padding) + loginDiscoveryJSON[1:]
	if len(body) != size {
		t.Fatalf("padded discovery body is %d bytes; want %d", len(body), size)
	}
	return []byte(body)
}

// TestDiscoveryRouteIsOutsideOperationRegistry pins the unauthenticated
// discovery route: a fixed root path, not one of the 51 operations, not
// matchable by the operation router and not requiring a credential to decode.
func TestDiscoveryRouteIsOutsideOperationRegistry(t *testing.T) {
	if api.DiscoveryPath != "/.well-known/kodama-configuration" {
		t.Fatalf("api.DiscoveryPath = %q; want the fixed unauthenticated discovery path", api.DiscoveryPath)
	}
	if api.MaxDiscoveryBytes <= 0 {
		t.Fatalf("api.MaxDiscoveryBytes = %d; want a positive bound", api.MaxDiscoveryBytes)
	}

	codecs := api.OperationCodecs()
	if len(codecs) != 51 {
		t.Fatalf("OperationCodecs() has %d entries; want exactly the 51 registry operations", len(codecs))
	}
	for _, route := range api.Routes() {
		if route.Path == api.DiscoveryPath {
			t.Errorf("discovery path is registered as operation route %q", route.Operation)
		}
		if route.Operation == "discovery.read" || route.Operation == "configuration.read" {
			t.Errorf("discovery is registered as an operation %q", route.Operation)
		}
	}
	for _, codec := range codecs {
		if codec.InputSchema == "discovery" || codec.Operation == "discovery.read" {
			t.Errorf("discovery appears in the operation codec table: %+v", codec)
		}
	}
	if _, _, ok := api.Match(api.MethodGet, api.DiscoveryPath); ok {
		t.Error("api.Match routed the discovery path through the operation router")
	}
	if _, ok := api.LookupRoute("discovery.read"); ok {
		t.Error("api.LookupRoute knows a discovery operation")
	}

	// The decoder takes no credential: an unauthenticated caller with the
	// public document can decode it.
	if _, err := api.DecodeDiscoveryDocument([]byte(loginDiscoveryJSON)); err != nil {
		t.Fatalf("unauthenticated discovery decode refused: %v", err)
	}
	if bytes.Contains([]byte(loginDiscoveryJSON), []byte("client_secret")) {
		t.Fatal("discovery fixture unexpectedly carries a secret")
	}
}
