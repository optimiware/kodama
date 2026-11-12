package registry_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// unauthorizedResponse answers a manifest request with the given
// WWW-Authenticate challenge values.
func unauthorizedResponse(request *http.Request, challenges ...string) *http.Response {
	header := make(http.Header)
	for _, challenge := range challenges {
		header.Add("WWW-Authenticate", challenge)
	}
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Status:     "401 Unauthorized",
		Header:     header,
		Body:       http.NoBody,
		Request:    request,
	}
}

func tokenResponse(request *http.Request, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func manifestResponse(request *http.Request) *http.Response {
	return registryResponse(request, http.StatusOK, ociIndexMediaType, ociIndexBody, digestOf(ociIndexBody))
}

// bearerRealmChallenge is the challenge shape Docker Hub and the distribution
// reference registry send.
func bearerRealmChallenge(realm string) string {
	return `Bearer realm="` + realm + `",service="registry.example.com",scope="repository:team/image:pull"`
}

// capturedHeader returns the Authorization header recorded for the request at
// the given index.
func capturedHeader(captured []capturedRequest, index int) string {
	return captured[index].Header.Get("Authorization")
}

func TestResolveBearerHandshakeAnonymous(t *testing.T) {
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "auth.example.com":
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("anonymous challenge sent %q to the realm", got)
			}
			return tokenResponse(request, `{"token":"tok-abc"}`), nil
		case "registry.example.com":
			if request.Header.Get("Authorization") == "Bearer tok-abc" {
				return manifestResponse(request), nil
			}
			return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
		default:
			t.Fatalf("unexpected request to %s", request.URL.Host)
			return nil, nil
		}
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})
	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireResolved(t, image, repository, digestOf(ociIndexBody))

	captured := transport.captured()
	if len(captured) != 3 {
		t.Fatalf("resolution made %d requests, want 3 (challenge, token, retry): %v", len(captured), captured)
	}
	if got := captured[0].URL; got != manifestURL("https", registryOrigin, repositoryPath, imageTag) {
		t.Fatalf("first request = %s", got)
	}
	if capturedHeader(captured, 0) != "" || capturedHeader(captured, 1) != "" {
		t.Fatalf("the challenge or token request carried a credential")
	}
	if got := capturedHeader(captured, 2); got != "Bearer tok-abc" {
		t.Fatalf("retry Authorization = %q, want the realm token", got)
	}
	tokenURL, err := url.Parse(captured[1].URL)
	if err != nil {
		t.Fatalf("token URL: %v", err)
	}
	if tokenURL.Scheme != "https" || tokenURL.Host != "auth.example.com" || tokenURL.Path != "/token" {
		t.Fatalf("token request = %s", tokenURL)
	}
	query := tokenURL.Query()
	if got := query.Get("service"); got != "registry.example.com" {
		t.Fatalf("service = %q", got)
	}
	if got := query["scope"]; len(got) != 1 || got[0] != "repository:team/image:pull" {
		t.Fatalf("scope = %v", got)
	}
}

func TestResolveBearerCredentialConfinement(t *testing.T) {
	newTransport := func(tokenAuth *string) *fakeTransport {
		return &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			switch request.URL.Host {
			case "auth.example.com":
				*tokenAuth = request.Header.Get("Authorization")
				return tokenResponse(request, `{"token":"tok-1"}`), nil
			case "registry.example.com":
				if request.Header.Get("Authorization") == "Bearer tok-1" {
					return manifestResponse(request), nil
				}
				return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
			default:
				t.Fatalf("unexpected request to %s", request.URL.Host)
				return nil, nil
			}
		}}
	}

	t.Run("registry credential is not offered to a foreign realm", func(t *testing.T) {
		var realmAuth string
		transport := newTransport(&realmAuth)
		source := &staticCredentialSource{
			origin:     "https://registry.example.com",
			credential: registry.Credential{Scheme: registry.CredentialBasic, Username: "registry-user", Password: "registry-secret"},
		}
		resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})
		if _, err := resolveWith(t, resolver, tagReference); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if realmAuth != "" {
			t.Fatalf("the registry credential reached the foreign realm: %q", realmAuth)
		}
		requireNoOrigin(t, source, "other.example.com")
	})

	t.Run("realm credential is offered to the realm's own origin", func(t *testing.T) {
		var realmAuth string
		transport := newTransport(&realmAuth)
		source := &staticCredentialSource{
			origin:     "https://auth.example.com",
			credential: registry.Credential{Scheme: registry.CredentialBasic, Username: "realm-user", Password: "realm-pw"},
		}
		resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})
		if _, err := resolveWith(t, resolver, tagReference); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("realm-user:realm-pw"))
		if realmAuth != want {
			t.Fatalf("realm Authorization = %q, want %q", realmAuth, want)
		}
		found := false
		for _, origin := range source.seenOrigins() {
			if origin == "https://auth.example.com" {
				found = true
			}
		}
		if !found {
			t.Fatalf("credential source origins = %v; the realm's own origin was not consulted", source.seenOrigins())
		}
	})

	t.Run("credential for an unrelated origin is not offered", func(t *testing.T) {
		var realmAuth string
		transport := newTransport(&realmAuth)
		source := &staticCredentialSource{
			origin:     "https://other.example.com",
			credential: registry.Credential{Scheme: registry.CredentialBasic, Username: "other-user", Password: "other-pw"},
		}
		resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})
		if _, err := resolveWith(t, resolver, tagReference); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if realmAuth != "" {
			t.Fatalf("an unrelated credential reached the realm: %q", realmAuth)
		}
	})

	t.Run("challenge token replaces an installation bearer credential", func(t *testing.T) {
		var realmAuth string
		transport := newTransport(&realmAuth)
		source := &staticCredentialSource{
			origin:     "https://registry.example.com",
			credential: registry.Credential{Scheme: registry.CredentialBearer, Token: "installation-token"},
		}
		resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})
		if _, err := resolveWith(t, resolver, tagReference); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		captured := transport.captured()
		if got := capturedHeader(captured, 0); got != "Bearer installation-token" {
			t.Fatalf("first request Authorization = %q", got)
		}
		if got := capturedHeader(captured, len(captured)-1); got != "Bearer tok-1" {
			t.Fatalf("retry Authorization = %q, want the challenge token", got)
		}
	})
}

func TestResolveBearerRealmRefused(t *testing.T) {
	cases := map[string]struct {
		realm     string
		insecure  bool
		wantClass fault.Class
	}{
		"plain http":      {realm: "http://auth.example.com/token", wantClass: fault.InvalidInput},
		"userinfo":        {realm: "https://realm-user:realm-pw@auth.example.com/token", wantClass: fault.InvalidInput},
		"empty username":  {realm: "https://:realm-pw@auth.example.com/token", wantClass: fault.InvalidInput},
		"fragment":        {realm: "https://auth.example.com/token#section", wantClass: fault.InvalidInput},
		"bare fragment":   {realm: "https://auth.example.com/token#", wantClass: fault.InvalidInput},
		"missing host":    {realm: "https:///token", wantClass: fault.InvalidInput},
		"unsupported":     {realm: "ftp://auth.example.com/token", wantClass: fault.InvalidInput},
		"scheme relative": {realm: "//auth.example.com/token", wantClass: fault.Unauthenticated},
		"relative name":   {realm: "kodama-test", wantClass: fault.Unauthenticated},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			realmCalls := 0
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				if request.URL.Host == "auth.example.com" {
					realmCalls++
					return tokenResponse(request, `{"token":"tok-1"}`), nil
				}
				return unauthorizedResponse(request, bearerRealmChallenge(tc.realm)), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport, AllowInsecureHTTP: tc.insecure})
			image, err := resolveWith(t, resolver, tagReference)
			requireFault(t, err, tc.wantClass)
			requireNoDigest(t, image)
			if realmCalls != 0 {
				t.Fatalf("a refused realm was contacted %d times", realmCalls)
			}
			requireNoLeak(t, err, "realm-user", "realm-pw")
		})
	}

	t.Run("http realm is allowed only by the explicit test flag", func(t *testing.T) {
		var realmAuth string
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			switch request.URL.Host {
			case "auth.example.com":
				realmAuth = request.Header.Get("Authorization")
				return tokenResponse(request, `{"token":"tok-http"}`), nil
			case "registry.example.com":
				if request.Header.Get("Authorization") == "Bearer tok-http" {
					return manifestResponse(request), nil
				}
				return unauthorizedResponse(request, bearerRealmChallenge("http://auth.example.com/token")), nil
			}
			t.Fatalf("unexpected request to %s", request.URL.Host)
			return nil, nil
		}}
		source := &staticCredentialSource{
			origin:     "http://auth.example.com",
			credential: registry.Credential{Scheme: registry.CredentialBasic, Username: "u", Password: "p"},
		}
		resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source, AllowInsecureHTTP: true})
		if _, err := resolveWith(t, resolver, tagReference); err != nil {
			t.Fatalf("Resolve with the insecure test flag: %v", err)
		}
		if realmAuth == "" {
			t.Fatal("the http realm did not receive its own configured credential")
		}
	})
}

func TestResolveBearerRealmRedirectNotFollowed(t *testing.T) {
	evilCalls := 0
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "auth.example.com":
			header := make(http.Header)
			header.Set("Location", "https://evil.example.com/token")
			return &http.Response{
				StatusCode: http.StatusFound,
				Status:     "302 Found",
				Header:     header,
				Body:       http.NoBody,
				Request:    request,
			}, nil
		case "evil.example.com":
			evilCalls++
			return tokenResponse(request, `{"token":"evil-token"}`), nil
		case "registry.example.com":
			return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
		}
		t.Fatalf("unexpected request to %s", request.URL.Host)
		return nil, nil
	}}
	source := &staticCredentialSource{
		origin:     "https://auth.example.com",
		credential: registry.Credential{Scheme: registry.CredentialBasic, Username: "realm-user", Password: "realm-pw"},
	}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})
	_, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.InvalidInput)
	if evilCalls != 0 {
		t.Fatalf("the token redirect was followed %d times", evilCalls)
	}
	requireNoLeak(t, err, "realm-user", "realm-pw", "evil-token")
}

func TestResolveBearerSingleTokenFetchAndRetry(t *testing.T) {
	t.Run("a second challenge is not retried", func(t *testing.T) {
		manifestCalls := 0
		tokenCalls := 0
		secondRealmCalls := 0
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			switch request.URL.Host {
			case "auth.example.com":
				tokenCalls++
				return tokenResponse(request, `{"token":"tok-1"}`), nil
			case "auth2.example.com":
				secondRealmCalls++
				return tokenResponse(request, `{"token":"tok-2"}`), nil
			case "registry.example.com":
				manifestCalls++
				if manifestCalls == 1 {
					return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
				}
				return unauthorizedResponse(request, bearerRealmChallenge("https://auth2.example.com/token")), nil
			}
			t.Fatalf("unexpected request to %s", request.URL.Host)
			return nil, nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		_, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.Unauthenticated)
		if tokenCalls != 1 || secondRealmCalls != 0 {
			t.Fatalf("token fetches = %d (second realm %d), want exactly one and no second realm", tokenCalls, secondRealmCalls)
		}
		if manifestCalls != 2 {
			t.Fatalf("manifest requests = %d, want 2 (challenge plus one retry)", manifestCalls)
		}
	})

	t.Run("challenge without a retryable token is not fetched", func(t *testing.T) {
		tokenCalls := 0
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Host == "auth.example.com" {
				tokenCalls++
				return tokenResponse(request, `{"token":"tok-1"}`), nil
			}
			return unauthorizedResponse(request, `Basic realm="registry.example.com"`), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		_, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.Unauthenticated)
		if tokenCalls != 0 {
			t.Fatalf("Basic challenge triggered %d token requests", tokenCalls)
		}
	})
}

func TestResolveBearerTokenFetchCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.example.com" {
			cancel()
			return nil, context.Canceled
		}
		return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})
	_, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: tagReference})
	requireFault(t, err, fault.DeadlineExpired)
	if got := len(transport.captured()); got != 2 {
		t.Fatalf("canceled token fetch made %d requests, want 2 (no retry)", got)
	}
}

type failingBody struct{ err error }

func (body failingBody) Read([]byte) (int, error) { return 0, body.err }

func TestResolveBearerTokenBodyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.example.com" {
			cancel()
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{},
				Body:       io.NopCloser(failingBody{err: context.Canceled}),
				Request:    request,
			}, nil
		}
		return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})
	_, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: tagReference})
	requireFault(t, err, fault.DeadlineExpired)
}

func TestResolveBearerTokenResponseBoundAndShape(t *testing.T) {
	padded := func(size int) string {
		const prefix = `{"token":"`
		const suffix = `"}`
		return prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix
	}

	cases := map[string]struct {
		body      string
		wantToken string
		wantClass fault.Class
	}{
		"token field":                  {body: `{"token":"tok-token"}`, wantToken: "tok-token"},
		"access token field":           {body: `{"access_token":"tok-access"}`, wantToken: "tok-access"},
		"token field preferred":        {body: `{"token":"tok-primary","access_token":"tok-secondary"}`, wantToken: "tok-primary"},
		"expires_in ignored":           {body: `{"token":"tok-exp","expires_in":300}`, wantToken: "tok-exp"},
		"exactly at the bound":         {body: padded(64 << 10), wantToken: strings.Repeat("a", (64<<10)-len(`{"token":""}`))},
		"one byte over the bound":      {body: padded(64<<10 + 1), wantClass: fault.InvalidInput},
		"empty object":                 {body: `{}`, wantClass: fault.InvalidInput},
		"empty token":                  {body: `{"token":""}`, wantClass: fault.InvalidInput},
		"empty access token":           {body: `{"access_token":""}`, wantClass: fault.InvalidInput},
		"malformed json":               {body: `{"token":`, wantClass: fault.InvalidInput},
		"non-string token":             {body: `{"token":5}`, wantClass: fault.InvalidInput},
		"token with a space":           {body: `{"token":"tok with space"}`, wantClass: fault.InvalidInput},
		"token with a tab":             {body: "{\"token\":\"tok\\tvalue\"}", wantClass: fault.InvalidInput},
		"token with a newline":         {body: `{"token":"tok\nvalue"}`, wantClass: fault.InvalidInput},
		"token with a carriage return": {body: `{"token":"tok\rvalue"}`, wantClass: fault.InvalidInput},
		"token with a NUL":             {body: "{\"token\":\"tok\\u0000value\"}", wantClass: fault.InvalidInput},
		"token with a control byte":    {body: "{\"token\":\"tok\u0001value\"}", wantClass: fault.InvalidInput},
		"token with DEL":               {body: "{\"token\":\"tok\\u007fvalue\"}", wantClass: fault.InvalidInput},
		"token with a non-ASCII rune":  {body: "{\"token\":\"tok\\u0080value\"}", wantClass: fault.InvalidInput},
		"token with a trailing space":  {body: `{"token":"tok "}`, wantClass: fault.InvalidInput},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			manifestCalls := 0
			tokenCalls := 0
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				if request.URL.Host == "auth.example.com" {
					tokenCalls++
					return tokenResponse(request, tc.body), nil
				}
				manifestCalls++
				if tc.wantToken != "" && request.Header.Get("Authorization") == "Bearer "+tc.wantToken {
					return manifestResponse(request), nil
				}
				return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})
			image, err := resolveWith(t, resolver, tagReference)
			if tc.wantClass != "" {
				requireFault(t, err, tc.wantClass)
				requireNoDigest(t, image)
				if tokenCalls != 1 {
					t.Fatalf("token requests = %d, want exactly 1", tokenCalls)
				}
				if manifestCalls != 1 {
					t.Fatalf("manifest requests = %d, want 1 (a refused token is never retried)", manifestCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			requireResolved(t, image, repository, digestOf(ociIndexBody))
			if tokenCalls != 1 || manifestCalls != 2 {
				t.Fatalf("requests: token %d, manifest %d; want 1 and 2", tokenCalls, manifestCalls)
			}
		})
	}
}

// TestResolveBearerTokenBodyReadBounded pins that the token reply is bounded at
// the read itself, not only by the size check that follows it: a hostile token
// endpoint that streams an endless body must be cut off after the documented
// bound. Replacing the bounded read with an unbounded one still refuses an
// oversize body, so the exact-bound pair alone could not catch that; this
// byte-count assertion can.
func TestResolveBearerTokenBodyReadBounded(t *testing.T) {
	// maxTokenResponse is 64 KiB; the slack is one read buffer beyond the
	// bound+1 bytes the bounded read may consume.
	const tokenBound = int64(64 << 10)
	const slack = int64(64 << 10)
	body := newEndlessBody(1 << 20)
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.example.com" {
			return responseWithBody(request, http.StatusOK, "application/json", body), nil
		}
		return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})
	image, err := resolveWith(t, resolver, tagReference)

	if allowed := tokenBound + 1 + slack; body.read > allowed {
		t.Fatalf("token body read consumed %d bytes, want at most %d: the read is not bounded", body.read, allowed)
	}
	requireFault(t, err, fault.InvalidInput)
	requireNoDigest(t, image)
	if got := len(transport.captured()); got != 2 {
		t.Fatalf("resolution made %d requests, want 2 (challenge, token; a refused token is never retried)", got)
	}
}

func TestResolveBearerChallengeParsing(t *testing.T) {
	cases := map[string]struct {
		headers      []string
		wantQuery    map[string]string
		wantClass    fault.Class
		wantFetched  bool
		wantResolved bool
	}{
		"basic before bearer in one header": {
			headers:      []string{`Basic realm="basic.example", Bearer realm="https://auth.example.com/token",service="registry.example.com"`},
			wantFetched:  true,
			wantResolved: true,
		},
		"bearer before basic in one header": {
			headers:      []string{`Bearer realm="https://auth.example.com/token",service="registry.example.com", Basic realm="basic.example"`},
			wantFetched:  true,
			wantResolved: true,
		},
		"basic and bearer header values": {
			headers:      []string{`Basic realm="basic.example"`, `Bearer realm="https://auth.example.com/token",service="registry.example.com"`},
			wantFetched:  true,
			wantResolved: true,
		},
		"lowercase scheme": {
			headers:      []string{`bearer realm="https://auth.example.com/token",service="registry.example.com"`},
			wantFetched:  true,
			wantResolved: true,
		},
		"comma inside a quoted parameter": {
			headers:      []string{`Bearer realm="https://auth.example.com/token?audience=a,b",service="registry.example.com"`},
			wantQuery:    map[string]string{"audience": "a,b"},
			wantFetched:  true,
			wantResolved: true,
		},
		"unknown parameters ignored": {
			headers:      []string{`Bearer realm="https://auth.example.com/token",service="registry.example.com",error="insufficient_scope",detail="a,b"`},
			wantFetched:  true,
			wantResolved: true,
		},
		"missing realm": {
			headers:   []string{`Bearer service="registry.example.com"`},
			wantClass: fault.Unauthenticated,
		},
		"basic only": {
			headers:   []string{`Basic realm="https://auth.example.com/token"`},
			wantClass: fault.Unauthenticated,
		},
		"bearer without parameters": {
			headers:   []string{`Bearer`},
			wantClass: fault.Unauthenticated,
		},
		"unknown scheme": {
			headers:   []string{`Digest realm="https://auth.example.com/token"`},
			wantClass: fault.Unauthenticated,
		},
		"empty header": {
			headers:   []string{``},
			wantClass: fault.Unauthenticated,
		},
		"escaped quote and comma in a quoted service": {
			headers:      []string{`Bearer realm="https://auth.example.com/token",service="reg\"istry,example"`},
			wantQuery:    map[string]string{"service": `reg"istry,example`},
			wantFetched:  true,
			wantResolved: true,
		},
		"escaped backslash in a quoted service": {
			headers:      []string{`Bearer realm="https://auth.example.com/token",service="reg\\istry"`},
			wantQuery:    map[string]string{"service": `reg\istry`},
			wantFetched:  true,
			wantResolved: true,
		},
		"escaped quote and backslash in the realm": {
			headers:      []string{`Bearer realm="https://auth.example.com/token?q=re\"al\\m"`},
			wantQuery:    map[string]string{"q": `re"al\m`},
			wantFetched:  true,
			wantResolved: true,
		},
		"escaped comma in a quoted scope": {
			headers:      []string{`Bearer realm="https://auth.example.com/token",scope="repository:team/image:pull\,extra"`},
			wantQuery:    map[string]string{"scope": `repository:team/image:pull,extra`},
			wantFetched:  true,
			wantResolved: true,
		},
		"trailing escaped backslash in a quoted service": {
			headers:      []string{`Bearer realm="https://auth.example.com/token",service="reg\\"`},
			wantQuery:    map[string]string{"service": `reg\`},
			wantFetched:  true,
			wantResolved: true,
		},
		"trailing lone backslash leaves the quote open": {
			headers:      []string{`Bearer realm="https://auth.example.com/token",service="reg\"`},
			wantQuery:    map[string]string{"service": ""},
			wantFetched:  true,
			wantResolved: true,
		},
		"control byte in a quoted service": {
			headers:      []string{"Bearer realm=\"https://auth.example.com/token\",service=\"reg\u0001istry\""},
			wantQuery:    map[string]string{"service": ""},
			wantFetched:  true,
			wantResolved: true,
		},
		"DEL in a quoted service": {
			headers:      []string{"Bearer realm=\"https://auth.example.com/token\",service=\"reg\u007fistry\""},
			wantQuery:    map[string]string{"service": ""},
			wantFetched:  true,
			wantResolved: true,
		},
		"control byte in the realm": {
			headers:     []string{"Bearer realm=\"https://auth.example.com/tok\u0001en\""},
			wantClass:   fault.Unauthenticated,
			wantFetched: false,
		},
		// A bare (unquoted) value is judged by the same visible-ASCII rule as a
		// quoted one: a control byte or DEL is not carried into the token query.
		"control byte in a bare service": {
			headers:      []string{"Bearer realm=\"https://auth.example.com/token\",service=reg\u0001istry"},
			wantQuery:    map[string]string{"service": ""},
			wantFetched:  true,
			wantResolved: true,
		},
		"DEL in a bare service": {
			headers:      []string{"Bearer realm=\"https://auth.example.com/token\",service=reg\u007fistry"},
			wantQuery:    map[string]string{"service": ""},
			wantFetched:  true,
			wantResolved: true,
		},
		"control byte in a bare scope": {
			headers:      []string{"Bearer realm=\"https://auth.example.com/token\",scope=repository:team/image:pull\u0001"},
			wantQuery:    map[string]string{"scope": ""},
			wantFetched:  true,
			wantResolved: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tokenCalls := 0
			var tokenQuery url.Values
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				if request.URL.Host == "auth.example.com" {
					tokenCalls++
					tokenQuery = request.URL.Query()
					return tokenResponse(request, `{"token":"tok-parsed"}`), nil
				}
				if request.Header.Get("Authorization") == "Bearer tok-parsed" {
					return manifestResponse(request), nil
				}
				return unauthorizedResponse(request, tc.headers...), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})
			image, err := resolveWith(t, resolver, tagReference)
			if tc.wantClass != "" {
				requireFault(t, err, tc.wantClass)
			} else if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := tokenCalls > 0; got != tc.wantFetched {
				t.Fatalf("token fetched = %v, want %v", got, tc.wantFetched)
			}
			if tc.wantResolved && err == nil {
				requireResolved(t, image, repository, digestOf(ociIndexBody))
			}
			for key, want := range tc.wantQuery {
				if got := tokenQuery.Get(key); got != want {
					t.Fatalf("token query %s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestResolveBearerScopeEncoding(t *testing.T) {
	var tokenQuery url.Values
	var tokenRawQuery string
	challenge := `Bearer realm="https://auth.example.com/token?client_id=kodama&service=evil",service="registry.example.com&debug=1",scope="repository:team/image:pull&admin=1 repository:other:pull",scope="repository:third:pull"`
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.example.com" {
			tokenQuery = request.URL.Query()
			tokenRawQuery = request.URL.RawQuery
			return tokenResponse(request, `{"token":"tok-scope"}`), nil
		}
		if request.Header.Get("Authorization") == "Bearer tok-scope" {
			return manifestResponse(request), nil
		}
		return unauthorizedResponse(request, challenge), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})
	if _, err := resolveWith(t, resolver, tagReference); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got := tokenQuery.Get("client_id"); got != "kodama" {
		t.Fatalf("client_id = %q; the realm's own query was not preserved", got)
	}
	if got := tokenQuery["service"]; len(got) != 1 || got[0] != "registry.example.com&debug=1" {
		t.Fatalf("service = %v; injection was not encoded into one value", got)
	}
	wantScopes := []string{"repository:team/image:pull&admin=1", "repository:other:pull", "repository:third:pull"}
	gotScopes := tokenQuery["scope"]
	if len(gotScopes) != len(wantScopes) {
		t.Fatalf("scope = %v, want %v", gotScopes, wantScopes)
	}
	for index := range wantScopes {
		if gotScopes[index] != wantScopes[index] {
			t.Fatalf("scope[%d] = %q, want %q", index, gotScopes[index], wantScopes[index])
		}
	}
	if tokenQuery.Get("admin") != "" || tokenQuery.Get("debug") != "" {
		t.Fatalf("a scope or service value injected an extra parameter: %s", tokenRawQuery)
	}
	if !strings.Contains(tokenRawQuery, "scope=repository%3Ateam%2Fimage%3Apull%26admin%3D1") {
		t.Fatalf("scope was not percent-encoded: %s", tokenRawQuery)
	}
}

func TestResolveBearerErrorsDoNotLeakSecrets(t *testing.T) {
	t.Run("rejected token request", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Host == "auth.example.com" {
				return registryResponse(request, http.StatusUnauthorized, "", "", ""), nil
			}
			return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
		}}
		source := &staticCredentialSource{
			origin:     "https://registry.example.com",
			credential: registry.Credential{Scheme: registry.CredentialBasic, Username: "registry-user", Password: "registry-secret-pw"},
		}
		resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})
		_, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.Unauthenticated)
		requireNoLeak(t, err, "registry-user", "registry-secret-pw")
	})

	t.Run("retry failure after a token", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Host == "auth.example.com" {
				return tokenResponse(request, `{"token":"tok-secret-value"}`), nil
			}
			if request.Header.Get("Authorization") == "Bearer tok-secret-value" {
				return registryResponse(request, http.StatusInternalServerError, "", "", ""), nil
			}
			return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		_, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.DependencyUnavailable)
		requireNoLeak(t, err, "tok-secret-value")
	})

	t.Run("realm userinfo", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return unauthorizedResponse(request, bearerRealmChallenge("https://realm-user:realm-pw@auth.example.com/token")), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		_, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoLeak(t, err, "realm-user", "realm-pw")
	})

	t.Run("rejected token value", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Host == "auth.example.com" {
				return tokenResponse(request, `{"token":"tok with space"}`), nil
			}
			return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		_, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoLeak(t, err, "tok with space")
	})
}

func TestResolveBearerTokenStatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		location  string
		body      string
		wantClass fault.Class
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, wantClass: fault.DependencyUnavailable},
		{name: "server error", status: http.StatusInternalServerError, wantClass: fault.DependencyUnavailable},
		{name: "service unavailable", status: http.StatusServiceUnavailable, wantClass: fault.DependencyUnavailable},
		{name: "forbidden", status: http.StatusForbidden, wantClass: fault.Unauthenticated},
		{name: "redirect refused", status: http.StatusFound, location: "https://evil.example.com/token", wantClass: fault.InvalidInput},
		// Only a 200 carries a usable token. A 1xx, 2xx or 3xx status is
		// refused before its body is read, so a redirect or a "created" reply
		// that happens to carry a JSON token is never accepted.
		{name: "redirect with a JSON token body", status: http.StatusFound, location: "https://evil.example.com/token", body: `{"token":"tok-from-redirect"}`, wantClass: fault.InvalidInput},
		{name: "created with a JSON token body", status: http.StatusCreated, body: `{"token":"tok-created"}`, wantClass: fault.InvalidInput},
		{name: "no content with a JSON token body", status: http.StatusNoContent, body: `{"token":"tok-no-content"}`, wantClass: fault.InvalidInput},
		{name: "informational with a JSON token body", status: http.StatusEarlyHints, body: `{"token":"tok-early-hints"}`, wantClass: fault.InvalidInput},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tokenCalls := 0
			manifestCalls := 0
			redirectCalls := 0
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "auth.example.com":
					tokenCalls++
					header := make(http.Header)
					if testCase.location != "" {
						header.Set("Location", testCase.location)
					}
					body := io.NopCloser(strings.NewReader(testCase.body))
					if testCase.body == "" {
						body = http.NoBody
					}
					return &http.Response{
						StatusCode: testCase.status,
						Status:     fmt.Sprintf("%d %s", testCase.status, http.StatusText(testCase.status)),
						Header:     header,
						Body:       body,
						Request:    request,
					}, nil
				case "evil.example.com":
					redirectCalls++
					return tokenResponse(request, `{"token":"redirected-token"}`), nil
				default:
					manifestCalls++
					return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
				}
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})
			image, err := resolveWith(t, resolver, tagReference)
			requireFault(t, err, testCase.wantClass)
			requireNoDigest(t, image)
			if tokenCalls != 1 {
				t.Fatalf("token requests = %d, want exactly 1", tokenCalls)
			}
			if manifestCalls != 1 {
				t.Fatalf("manifest requests = %d, want 1 (a failed token fetch is never retried)", manifestCalls)
			}
			if redirectCalls != 0 {
				t.Fatalf("token redirect was followed %d times, want none", redirectCalls)
			}
			requireNoLeak(t, err, "redirected-token", "tok-from-redirect", "tok-created", "tok-no-content", "tok-early-hints")
		})
	}
}

// TestResolveBearerTokenRequestCarriesContext pins that the token request is
// bound to the resolve context like the manifest request: canceling a resolve
// blocked at the token endpoint returns promptly with the deadline class.
func TestResolveBearerTokenRequestCarriesContext(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.example.com" {
			once.Do(func() { close(started) })
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		image pipeline.ResolvedImage
		err   error
	}
	result := make(chan outcome, 1)
	go func() {
		image, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: tagReference})
		result <- outcome{image: image, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve never reached the token endpoint")
	}
	cancel()

	select {
	case got := <-result:
		requireFault(t, got.err, fault.DeadlineExpired)
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancellation fault lost context.Canceled: %v", got.err)
		}
		requireNoDigest(t, got.image)
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not return after cancellation while the token request was in flight")
	}
}

// TestResolveDockerHubHandshakeEndToEnd pins the real Docker Hub flow: a
// docker.io name is served by registry-1.docker.io, whose Bearer challenge
// names auth.docker.io; the credential source is asked about the API origin and
// the realm's own origin, the retry carries the token, and the resolved image
// keeps the name the caller wrote.
func TestResolveDockerHubHandshakeEndToEnd(t *testing.T) {
	const (
		hubReference  = "docker.io/library/node:lts"
		hubRepository = "docker.io/library/node"
		hubChallenge  = `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/node:pull"`
	)
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "registry-1.docker.io":
			if request.URL.Path != "/v2/library/node/manifests/lts" {
				t.Fatalf("Docker Hub manifest path = %s", request.URL.Path)
			}
			if request.Header.Get("Authorization") == "Bearer hub-token" {
				return manifestResponse(request), nil
			}
			return unauthorizedResponse(request, hubChallenge), nil
		case "auth.docker.io":
			return tokenResponse(request, `{"token":"hub-token"}`), nil
		default:
			t.Fatalf("unexpected request to %s", request.URL.Host)
			return nil, nil
		}
	}}
	source := &staticCredentialSource{
		credential: registry.Credential{Scheme: registry.CredentialBasic, Username: "hub-user", Password: "hub-pw"},
	}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	image, err := resolveWith(t, resolver, hubReference)
	if err != nil {
		t.Fatalf("Resolve(docker.io): %v", err)
	}
	requireResolved(t, image, hubRepository, digestOf(ociIndexBody))

	origins := source.seenOrigins()
	wantOrigins := []string{"https://registry-1.docker.io", "https://auth.docker.io"}
	if len(origins) != len(wantOrigins) {
		t.Fatalf("credential source origins = %v, want %v", origins, wantOrigins)
	}
	for index := range wantOrigins {
		if origins[index] != wantOrigins[index] {
			t.Fatalf("credential source origins = %v, want %v", origins, wantOrigins)
		}
	}

	captured := transport.captured()
	if len(captured) != 3 {
		t.Fatalf("Docker Hub resolution made %d requests, want 3 (challenge, token, retry): %v", len(captured), captured)
	}
	wantManifest := "https://registry-1.docker.io/v2/library/node/manifests/lts"
	if captured[0].URL != wantManifest {
		t.Fatalf("first request = %q, want %q", captured[0].URL, wantManifest)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("hub-user:hub-pw"))
	if got := capturedHeader(captured, 0); got != wantBasic {
		t.Fatalf("first request Authorization = %q, want the Hub credential", got)
	}
	if got := capturedHeader(captured, 2); got != "Bearer hub-token" {
		t.Fatalf("retry Authorization = %q, want the challenge token", got)
	}
	tokenURL, err := url.Parse(captured[1].URL)
	if err != nil {
		t.Fatalf("token URL: %v", err)
	}
	if tokenURL.Host != "auth.docker.io" || tokenURL.Path != "/token" {
		t.Fatalf("token request = %s", tokenURL)
	}
	if got := tokenURL.Query().Get("service"); got != "registry.docker.io" {
		t.Fatalf("token service = %q", got)
	}
	if got := tokenURL.Query().Get("scope"); got != "repository:library/node:pull" {
		t.Fatalf("token scope = %q", got)
	}
}

// TestResolveDockerHubSingleSegmentRepositoryPath documents the current Docker
// Hub repository path rule: a single-segment Hub repository is requested
// without a "library/" prefix and the resolved reference keeps the name the
// caller wrote. Docker Hub serves its official images under library/, so a
// docker.io/node lookups needs special handling this resolver does not do.
func TestResolveDockerHubSingleSegmentRepositoryPath(t *testing.T) {
	const reference = "docker.io/node:lts"
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		if request.URL.Host != "registry-1.docker.io" {
			t.Fatalf("manifest request went to %s, want registry-1.docker.io", request.URL.Host)
		}
		if request.URL.Path != "/v2/node/manifests/lts" {
			t.Fatalf("manifest path = %s, want the single-segment path /v2/node/manifests/lts", request.URL.Path)
		}
		return manifestResponse(request), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})
	image, err := resolveWith(t, resolver, reference)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", reference, err)
	}
	requireResolved(t, image, "docker.io/node", digestOf(ociIndexBody))
}

// TestResolveMalformedHostPortRefused pins both malformed-host choices: a root
// dot after the port ("docker.io:443.") or a doubled port ("docker.io:443:443")
// is not the default-port spelling of the Docker Hub name, so it is never mapped
// and the request URL it would form is refused as invalid input. The host is
// refused before the credential source is consulted: no credential is looked up
// for an origin that can never be requested.
func TestResolveMalformedHostPortRefused(t *testing.T) {
	cases := []struct {
		name      string
		reference string
	}{
		{"root dot after the port", "docker.io:443./library/node:lts"},
		{"doubled port", "docker.io:443:443/library/node:lts"},
		{"root dot after the index port", "index.docker.io:443./library/node:lts"},
		{"doubled index port", "index.docker.io:443:443/library/node:lts"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{}
			source := &staticCredentialSource{}
			resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

			image, err := resolveWith(t, resolver, testCase.reference)
			requireFault(t, err, fault.InvalidInput)
			requireNoDigest(t, image)
			if requests := transport.captured(); len(requests) != 0 {
				t.Fatalf("a malformed host port issued %d registry requests", len(requests))
			}
			if origins := source.seenOrigins(); len(origins) != 0 {
				t.Fatalf("the credential source was consulted for %v before the malformed host was refused", origins)
			}
		})
	}
}

// TestResolveBracketedNonIPHostRefused pins the bracketed-host decision: a
// bracketed value must be an IP literal, so "[docker.io]" (with or without the
// default port) is not a host at all. Without that rule
// net.SplitHostPort("[docker.io]:443") strips the brackets and the name maps to
// the Docker Hub API host. A bracketed value that is not an IP literal is never
// mapped, is refused as invalid input, and never reaches the network or the
// credential source.
func TestResolveBracketedNonIPHostRefused(t *testing.T) {
	cases := []struct {
		name      string
		reference string
	}{
		{"bracketed name with default port", "[docker.io]:443/library/node:lts"},
		{"bracketed name without port", "[docker.io]/library/node:lts"},
		{"bracketed index name with default port", "[index.docker.io]:443/library/node:lts"},
		{"bracketed name with another port", "[docker.io]:5000/library/node:lts"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{}
			source := &staticCredentialSource{}
			resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

			image, err := resolveWith(t, resolver, testCase.reference)
			requireFault(t, err, fault.InvalidInput)
			requireNoDigest(t, image)
			if requests := transport.captured(); len(requests) != 0 {
				t.Fatalf("a bracketed non-IP host issued %d registry requests", len(requests))
			}
			if origins := source.seenOrigins(); len(origins) != 0 {
				t.Fatalf("the credential source was consulted for %v before the bracketed host was refused", origins)
			}
		})
	}
}

// TestResolveBracketedIPLiteralHost pins that the bracketed rule only refuses
// non-IP values: an IPv6 literal registry still resolves, so the bracket
// handling never over-refuses a legitimate host.
func TestResolveBracketedIPLiteralHost(t *testing.T) {
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		if request.URL.Host != "[::1]:5000" {
			t.Fatalf("manifest request went to %s, want the IPv6 literal host", request.URL.Host)
		}
		return manifestResponse(request), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, "[::1]:5000/team/image:1.2")
	if err != nil {
		t.Fatalf("Resolve refused an IPv6 literal registry: %v", err)
	}
	requireResolved(t, image, "[::1]:5000/team/image", digestOf(ociIndexBody))
}

func TestResolveDockerHubAPIHostMapping(t *testing.T) {
	cases := []struct {
		name       string
		reference  string
		wantHost   string
		wantResult string
	}{
		{name: "docker.io", reference: "docker.io/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "docker.io/library/node"},
		{name: "index.docker.io", reference: "index.docker.io/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "index.docker.io/library/node"},
		{name: "registry-1.docker.io", reference: "registry-1.docker.io/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "registry-1.docker.io/library/node"},
		{name: "docker.io mixed case", reference: "Docker.io/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "Docker.io/library/node"},
		{name: "docker.io upper case", reference: "DOCKER.IO/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "DOCKER.IO/library/node"},
		{name: "index.docker.io mixed case", reference: "Index.Docker.IO/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "Index.Docker.IO/library/node"},
		{name: "docker.io default port", reference: "docker.io:443/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "docker.io:443/library/node"},
		{name: "index.docker.io default port", reference: "index.docker.io:443/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "index.docker.io:443/library/node"},
		{name: "docker.io trailing dot", reference: "docker.io./library/node:lts", wantHost: "registry-1.docker.io", wantResult: "docker.io./library/node"},
		{name: "docker.io non-default port is another registry", reference: "docker.io:5000/library/node:lts", wantHost: "docker.io:5000", wantResult: "docker.io:5000/library/node"},
		{name: "lookalike suffix", reference: "docker.io.evil.com/library/node:lts", wantHost: "docker.io.evil.com", wantResult: "docker.io.evil.com/library/node"},
		{name: "lookalike suffix with default port", reference: "docker.io.evil.com:443/library/node:lts", wantHost: "docker.io.evil.com:443", wantResult: "docker.io.evil.com:443/library/node"},
		{name: "lookalike prefix", reference: "notdocker.io/library/node:lts", wantHost: "notdocker.io", wantResult: "notdocker.io/library/node"},
		{name: "lookalike subdomain", reference: "foo.docker.io/library/node:lts", wantHost: "foo.docker.io", wantResult: "foo.docker.io/library/node"},
		{name: "lookalike trailing dot", reference: "docker.io.evil.com./library/node:lts", wantHost: "docker.io.evil.com.", wantResult: "docker.io.evil.com./library/node"},
		{name: "two trailing dots", reference: "docker.io../library/node:lts", wantHost: "docker.io..", wantResult: "docker.io../library/node"},
		{name: "two trailing dots with default port", reference: "docker.io..:443/library/node:lts", wantHost: "docker.io..:443", wantResult: "docker.io..:443/library/node"},
		{name: "trailing dot with default port", reference: "docker.io.:443/library/node:lts", wantHost: "registry-1.docker.io", wantResult: "docker.io.:443/library/node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				if request.URL.Host != tc.wantHost {
					t.Fatalf("manifest request went to %s, want %s", request.URL.Host, tc.wantHost)
				}
				if request.URL.Path != "/v2/library/node/manifests/lts" {
					t.Fatalf("manifest path = %s", request.URL.Path)
				}
				return manifestResponse(request), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})
			image, err := resolveWith(t, resolver, tc.reference)
			if err != nil {
				t.Fatalf("Resolve(%s): %v", tc.reference, err)
			}
			requireResolved(t, image, tc.wantResult, digestOf(ociIndexBody))
		})
	}
}
