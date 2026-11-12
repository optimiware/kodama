package registry_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
)

// TestResolveBearerCredential pins bearer presentation, origin scoping and the
// rule that a credential never becomes resolution content.
func TestResolveBearerCredential(t *testing.T) {
	const token = "installation-bearer-token"
	body := singleManifestBody
	source := &staticCredentialSource{
		origin:     "https://" + registryOrigin,
		credential: registry.Credential{Scheme: registry.CredentialBearer, Token: token},
	}
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a credentialed read: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	requests := transport.captured()
	if len(requests) != 1 {
		t.Fatalf("resolution issued %d requests, want exactly 1", len(requests))
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer "+token)
	}
	if strings.Contains(requests[0].URL, token) {
		t.Fatalf("credential leaked into the request URL %q", requests[0].URL)
	}
	if strings.Contains(image.Reference, token) || strings.Contains(image.Digest, token) {
		t.Fatalf("credential leaked into the resolved image %#v", image)
	}
	requireOrigins(t, source, "https://"+registryOrigin)
}

// TestResolveBasicCredential pins basic presentation without leaking the
// password anywhere outside the Authorization header.
func TestResolveBasicCredential(t *testing.T) {
	const (
		username = "kodama"
		password = "registry-password-do-not-leak"
	)
	body := singleManifestBody
	source := &staticCredentialSource{
		origin: "https://" + registryOrigin,
		credential: registry.Credential{
			Scheme:   registry.CredentialBasic,
			Username: username,
			Password: password,
		},
	}
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a basic-credential read: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	requests := transport.captured()
	if got := requests[0].Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if strings.Contains(requests[0].URL, password) {
		t.Fatalf("password leaked into the request URL %q", requests[0].URL)
	}
	requireOrigins(t, source, "https://"+registryOrigin)
}

// TestResolveAnonymousCredential pins that anonymous access sends no
// Authorization header, whether the source is explicit or absent.
func TestResolveAnonymousCredential(t *testing.T) {
	cases := []struct {
		name   string
		source registry.CredentialSource
	}{
		{"explicit anonymous", &staticCredentialSource{
			origin:     "https://" + registryOrigin,
			credential: registry.Credential{Scheme: registry.CredentialAnonymous},
		}},
		{"nil source", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := singleManifestBody
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport, Credentials: testCase.source})

			image, err := resolveWith(t, resolver, tagReference)
			if err != nil {
				t.Fatalf("Resolve refused anonymous access: %v", err)
			}
			requireResolved(t, image, repository, digestOf(body))

			if got := transport.captured()[0].Header.Get("Authorization"); got != "" {
				t.Fatalf("anonymous resolution sent Authorization %q", got)
			}
		})
	}
}

// TestResolveUnauthorizedWithCredential pins that a rejected credential is sent
// to the configured origin but never echoed into the error.
func TestResolveUnauthorizedWithCredential(t *testing.T) {
	const token = "rejected-bearer-token"
	source := &staticCredentialSource{
		origin:     "https://" + registryOrigin,
		credential: registry.Credential{Scheme: registry.CredentialBearer, Token: token},
	}
	body := `{"errors":[{"code":"UNAUTHORIZED","message":"token ` + token + ` is rejected"}]}`
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusUnauthorized, "application/json", body, ""), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	image, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.Unauthenticated)
	requireNoDigest(t, image)
	requireNoLeak(t, err, token, "is rejected")
	requireOrigins(t, source, "https://"+registryOrigin)
}

// TestResolveUnauthorizedWithoutCredential pins that an unauthenticated read
// without configured credentials sends no Authorization header.
func TestResolveUnauthorizedWithoutCredential(t *testing.T) {
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusUnauthorized, "application/json", "", ""), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.Unauthenticated)
	requireNoDigest(t, image)
	if got := transport.captured()[0].Header.Get("Authorization"); got != "" {
		t.Fatalf("uncredentialed read sent Authorization %q", got)
	}
}

// TestResolveSameOriginRedirectKeepsCredential pins that a redirect within the
// configured origin stays authenticated.
func TestResolveSameOriginRedirectKeepsCredential(t *testing.T) {
	const token = "same-origin-token"
	body := singleManifestBody
	redirectTarget := manifestURL("https", registryOrigin, "mirror/"+repositoryPath, imageTag)
	source := &staticCredentialSource{
		origin:     "https://" + registryOrigin,
		credential: registry.Credential{Scheme: registry.CredentialBearer, Token: token},
	}
	transport := &fakeTransport{handle: func(call int, request *http.Request) (*http.Response, error) {
		if call == 0 {
			return redirectResponse(request, redirectTarget), nil
		}
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a same-origin redirect: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	requests := transport.captured()
	if len(requests) != 2 {
		t.Fatalf("redirect chain issued %d requests, want 2", len(requests))
	}
	if requests[1].URL != redirectTarget {
		t.Fatalf("redirected request URL = %q, want %q", requests[1].URL, redirectTarget)
	}
	for index, request := range requests {
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("request %d Authorization = %q, want %q", index, got, "Bearer "+token)
		}
	}
	requireOrigins(t, source, "https://"+registryOrigin)
}

// TestResolveCrossOriginRedirectStripsCredential pins origin confinement: a
// redirect to another origin is followed without the installation credential,
// and the source is never consulted for that other origin.
func TestResolveCrossOriginRedirectStripsCredential(t *testing.T) {
	const token = "cross-origin-token"
	const cdnOrigin = "https://cdn.example.com"
	body := singleManifestBody
	redirectTarget := cdnOrigin + "/blobs/manifest.json"
	source := &staticCredentialSource{
		origin:     "https://" + registryOrigin,
		credential: registry.Credential{Scheme: registry.CredentialBearer, Token: token},
	}
	transport := &fakeTransport{handle: func(call int, request *http.Request) (*http.Response, error) {
		if call == 0 {
			return redirectResponse(request, redirectTarget), nil
		}
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a cross-origin redirect: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	requests := transport.captured()
	if len(requests) != 2 {
		t.Fatalf("redirect chain issued %d requests, want 2", len(requests))
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("origin request Authorization = %q, want %q", got, "Bearer "+token)
	}
	if requests[1].URL != redirectTarget {
		t.Fatalf("redirected request URL = %q, want %q", requests[1].URL, redirectTarget)
	}
	if got := requests[1].Header.Get("Authorization"); got != "" {
		t.Fatalf("cross-origin redirect forwarded Authorization %q", got)
	}
	requireNoOrigin(t, source, "cdn.example.com")
}

// TestResolveBearerRetryRedirectStripsToken pins that a challenge token is
// origin-bound exactly like an installation credential: when the bearer retry
// is answered with a cross-origin redirect (a CDN), the redirect target must be
// fetched without the Authorization header.
func TestResolveBearerRetryRedirectStripsToken(t *testing.T) {
	const (
		token     = "challenge-token"
		cdnOrigin = "https://cdn.example.com"
	)
	body := singleManifestBody
	redirectTarget := cdnOrigin + "/blobs/manifest.json"
	manifestCalls := 0
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "auth.example.com":
			return tokenResponse(request, `{"token":"`+token+`"}`), nil
		case registryOrigin:
			manifestCalls++
			if manifestCalls == 1 {
				return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
			}
			if got := request.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("bearer retry Authorization = %q, want the challenge token", got)
			}
			return redirectResponse(request, redirectTarget), nil
		case "cdn.example.com":
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
		default:
			t.Fatalf("unexpected request to %s", request.URL.Host)
			return nil, nil
		}
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused the redirected bearer retry: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	requests := transport.captured()
	if len(requests) != 4 {
		t.Fatalf("resolution made %d requests, want 4 (challenge, token, retry, redirect): %v", len(requests), requests)
	}
	if requests[3].URL != redirectTarget {
		t.Fatalf("redirected request URL = %q, want %q", requests[3].URL, redirectTarget)
	}
	if got := capturedHeader(requests, 3); got != "" {
		t.Fatalf("cross-origin redirect forwarded the challenge token as %q", got)
	}
}

// TestResolveBearerRealmOriginIsExact pins the realm credential origin: it is
// exactly "{scheme}://{host}:{port}" as the challenge wrote it. A credential
// configured for another origin that shares only the hostname (a different
// port, a subdomain, another IP-literal form, another letter case or a trailing
// dot) is neither asked for nor offered to the realm.
func TestResolveBearerRealmOriginIsExact(t *testing.T) {
	const (
		realmCredential = "realm-secret-credential"
		realmPath       = "/token"
	)
	cases := []struct {
		name            string
		configured      string
		realmHost       string
		wantRealmOrigin string
		wantCredential  bool
	}{
		{
			name:            "exact realm origin",
			configured:      "https://auth.example.com:8443",
			realmHost:       "auth.example.com:8443",
			wantRealmOrigin: "https://auth.example.com:8443",
			wantCredential:  true,
		},
		{
			name:            "realm port differs from the credential origin",
			configured:      "https://auth.example.com",
			realmHost:       "auth.example.com:8443",
			wantRealmOrigin: "https://auth.example.com:8443",
		},
		{
			name:            "credential port differs from the realm",
			configured:      "https://auth.example.com:8443",
			realmHost:       "auth.example.com",
			wantRealmOrigin: "https://auth.example.com",
		},
		{
			name:            "subdomain shares only the parent host",
			configured:      "https://auth.example.com",
			realmHost:       "sub.auth.example.com",
			wantRealmOrigin: "https://sub.auth.example.com",
		},
		{
			name:            "ipv4 literal port differs",
			configured:      "https://127.0.0.1",
			realmHost:       "127.0.0.1:5000",
			wantRealmOrigin: "https://127.0.0.1:5000",
		},
		{
			name:            "ipv6 literal port differs",
			configured:      "https://[::1]",
			realmHost:       "[::1]:5000",
			wantRealmOrigin: "https://[::1]:5000",
		},
		{
			name:            "letter case differs",
			configured:      "https://auth.example.com",
			realmHost:       "AUTH.example.com",
			wantRealmOrigin: "https://AUTH.example.com",
		},
		{
			name:            "trailing dot differs",
			configured:      "https://auth.example.com",
			realmHost:       "auth.example.com.:8443",
			wantRealmOrigin: "https://auth.example.com.:8443",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var realmAuth string
			realmCalls := 0
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case testCase.realmHost:
					realmCalls++
					realmAuth = request.Header.Get("Authorization")
					return tokenResponse(request, `{"token":"tok-realm"}`), nil
				case registryOrigin:
					if request.Header.Get("Authorization") == "Bearer tok-realm" {
						return manifestResponse(request), nil
					}
					return unauthorizedResponse(request, bearerRealmChallenge("https://"+testCase.realmHost+realmPath)), nil
				default:
					t.Fatalf("unexpected request to %s", request.URL.Host)
					return nil, nil
				}
			}}
			source := &staticCredentialSource{
				origin:     testCase.configured,
				credential: registry.Credential{Scheme: registry.CredentialBearer, Token: realmCredential},
			}
			resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})
			if _, err := resolveWith(t, resolver, tagReference); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if realmCalls != 1 {
				t.Fatalf("realm requests = %d, want 1", realmCalls)
			}
			want := ""
			if testCase.wantCredential {
				want = "Bearer " + realmCredential
			}
			if realmAuth != want {
				t.Fatalf("realm Authorization = %q, want %q", realmAuth, want)
			}
			origins := source.seenOrigins()
			if len(origins) != 2 {
				t.Fatalf("credential source origins = %v, want the registry origin and the realm origin", origins)
			}
			if origins[0] != "https://"+registryOrigin {
				t.Fatalf("first credential origin = %q, want the registry origin", origins[0])
			}
			if origins[1] != testCase.wantRealmOrigin {
				t.Fatalf("realm credential origin = %q, want %q", origins[1], testCase.wantRealmOrigin)
			}
		})
	}
}

// TestResolveForeignOriginUnauthorizedDoesNotFetchToken pins that the bearer
// handshake only runs for the configured registry origin: a 401 from a
// redirected-to foreign origin never triggers a token request, because a retry
// would carry no Authorization header there anyway.
func TestResolveForeignOriginUnauthorizedDoesNotFetchToken(t *testing.T) {
	const cdnOrigin = "https://cdn.example.com"
	redirectTarget := cdnOrigin + "/blobs/manifest.json"
	tokenCalls := 0
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "auth.example.com":
			tokenCalls++
			return tokenResponse(request, `{"token":"foreign-token"}`), nil
		case registryOrigin:
			return redirectResponse(request, redirectTarget), nil
		case "cdn.example.com":
			if request.Header.Get("Authorization") != "" {
				t.Fatalf("foreign origin received Authorization %q", request.Header.Get("Authorization"))
			}
			return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
		default:
			t.Fatalf("unexpected request to %s", request.URL.Host)
			return nil, nil
		}
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.Unauthenticated)
	requireNoDigest(t, image)
	if tokenCalls != 0 {
		t.Fatalf("a foreign-origin 401 triggered %d token requests, want none", tokenCalls)
	}
	if requests := transport.captured(); len(requests) != 2 {
		t.Fatalf("resolution issued %d requests, want 2 (origin, foreign 401)", len(requests))
	}
	requireNoLeak(t, err, "foreign-token")
}

// TestResolveTokenFetchDoesNotConsumeRedirectHop pins that the token handshake
// is not a redirect: with the default cap, three manifest redirects still work
// after a challenge token fetch, exactly as they do without one.
func TestResolveTokenFetchDoesNotConsumeRedirectHop(t *testing.T) {
	body := singleManifestBody
	newTransport := func(authenticated bool) (*fakeTransport, *int) {
		manifestCalls := 0
		redirects := 0
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Host == "auth.example.com" {
				return tokenResponse(request, `{"token":"tok-redirect"}`), nil
			}
			manifestCalls++
			if authenticated && manifestCalls == 1 {
				if got := request.Header.Get("Authorization"); got != "" {
					t.Fatalf("the challenge request carried Authorization %q", got)
				}
				return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
			}
			if authenticated {
				if got := request.Header.Get("Authorization"); got != "Bearer tok-redirect" {
					t.Fatalf("authenticated retry Authorization = %q", got)
				}
			}
			if redirects < registry.DefaultMaxRedirects {
				redirects++
				return redirectResponse(request, "https://"+registryOrigin+"/v2/mirror/manifests/hop-"+fmt.Sprint(redirects)), nil
			}
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
		}}
		return transport, &manifestCalls
	}

	t.Run("three redirects without a token", func(t *testing.T) {
		transport, manifestCalls := newTransport(false)
		resolver := newResolver(t, registry.Config{Transport: transport})
		image, err := resolveWith(t, resolver, tagReference)
		if err != nil {
			t.Fatalf("Resolve refused three redirects: %v", err)
		}
		requireResolved(t, image, repository, digestOf(body))
		if *manifestCalls != 4 {
			t.Fatalf("manifest requests = %d, want 4 (three redirects and the answer)", *manifestCalls)
		}
	})

	t.Run("three redirects after a token fetch", func(t *testing.T) {
		transport, manifestCalls := newTransport(true)
		resolver := newResolver(t, registry.Config{Transport: transport})
		image, err := resolveWith(t, resolver, tagReference)
		if err != nil {
			t.Fatalf("Resolve refused three redirects after a token fetch: %v", err)
		}
		requireResolved(t, image, repository, digestOf(body))
		if *manifestCalls != 5 {
			t.Fatalf("authenticated manifest requests = %d, want 5 (challenge, three redirects and the answer)", *manifestCalls)
		}
	})
}

// TestResolveRedirectCapAfterTokenFetch pins the redirect budget across the
// token handshake: fetching a token never extends the budget and never resets
// it. The baseline is exact with and without a token fetch (three redirect hops
// are followed and the fourth redirect response is refused), a redirect spent
// before the challenge is still spent afterwards, and endless redirects after
// the retry are refused at the exact same request count as an anonymous read.
func TestResolveRedirectCapAfterTokenFetch(t *testing.T) {
	const token = "tok-redirect-cap"
	loopTarget := "https://" + registryOrigin + "/v2/loop/manifests/again"

	t.Run("the fourth redirect is refused with and without a token", func(t *testing.T) {
		cases := []struct {
			name         string
			withToken    bool
			wantManifest int
			wantTotal    int
		}{
			{"without a token", false, 4, 4},
			{"with a token", true, 5, 6},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				manifestCalls := 0
				tokenCalls := 0
				transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
					if request.URL.Host == "auth.example.com" {
						tokenCalls++
						return tokenResponse(request, `{"token":"`+token+`"}`), nil
					}
					manifestCalls++
					if testCase.withToken && manifestCalls == 1 {
						return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
					}
					return redirectResponse(request, loopTarget), nil
				}}
				resolver := newResolver(t, registry.Config{Transport: transport})

				image, err := resolveWith(t, resolver, tagReference)
				requireFault(t, err, fault.InvalidInput)
				requireNoDigest(t, image)
				if manifestCalls != testCase.wantManifest {
					t.Fatalf("manifest requests = %d, want %d (three redirects and the refused fourth)", manifestCalls, testCase.wantManifest)
				}
				if got := len(transport.captured()); got != testCase.wantTotal {
					t.Fatalf("resolution made %d requests, want exactly %d", got, testCase.wantTotal)
				}
				wantTokens := 0
				if testCase.withToken {
					wantTokens = 1
				}
				if tokenCalls != wantTokens {
					t.Fatalf("token requests = %d, want %d", tokenCalls, wantTokens)
				}
			})
		}
	})

	t.Run("endless redirects after a token fetch", func(t *testing.T) {
		manifestCalls := 0
		tokenCalls := 0
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Host == "auth.example.com" {
				tokenCalls++
				return tokenResponse(request, `{"token":"`+token+`"}`), nil
			}
			manifestCalls++
			if manifestCalls == 1 {
				if got := request.Header.Get("Authorization"); got != "" {
					t.Fatalf("challenge request carried Authorization %q", got)
				}
				return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
			}
			if got := request.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("manifest request %d Authorization = %q, want the challenge token", manifestCalls, got)
			}
			return redirectResponse(request, loopTarget), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})

		image, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoDigest(t, image)
		if tokenCalls != 1 {
			t.Fatalf("token requests = %d, want exactly 1", tokenCalls)
		}
		if got := len(transport.captured()); got != 6 {
			t.Fatalf("resolution made %d requests, want exactly 6 (challenge, token, four manifest requests)", got)
		}
		if manifestCalls != 5 {
			t.Fatalf("manifest requests = %d, want 5 (challenge, three redirects and the refused fourth)", manifestCalls)
		}
	})

	t.Run("a redirect before the challenge does not restore the budget", func(t *testing.T) {
		redirectTarget := manifestURL("https", registryOrigin, "mirror/"+repositoryPath, imageTag)
		manifestCalls := 0
		tokenCalls := 0
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			if request.URL.Host == "auth.example.com" {
				tokenCalls++
				return tokenResponse(request, `{"token":"`+token+`"}`), nil
			}
			manifestCalls++
			switch manifestCalls {
			case 1:
				return redirectResponse(request, redirectTarget), nil
			case 2:
				if got := request.URL.String(); got != redirectTarget {
					t.Fatalf("challenge request URL = %q, want the redirected URL %q", got, redirectTarget)
				}
				return unauthorizedResponse(request, bearerRealmChallenge("https://auth.example.com/token")), nil
			}
			if got := request.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("manifest request %d Authorization = %q, want the challenge token", manifestCalls, got)
			}
			return redirectResponse(request, loopTarget), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})

		image, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoDigest(t, image)
		if tokenCalls != 1 {
			t.Fatalf("token requests = %d, want exactly 1", tokenCalls)
		}
		if got := len(transport.captured()); got != 6 {
			t.Fatalf("resolution made %d requests, want exactly 6 (redirect, challenge, token, three more redirects with the fourth refused)", got)
		}
		if manifestCalls != 5 {
			t.Fatalf("manifest requests = %d, want 5 (redirect, challenge, two redirects and the refused fourth)", manifestCalls)
		}
	})
}

// TestResolveSameOriginRedirectThenChallenge pins the origin gate for a
// legitimate flow: a 401 arriving at a same-origin redirected URL still triggers
// the handshake, and the retry goes to that redirected URL with the token. A
// gate that only accepted the first request would refuse this flow.
func TestResolveSameOriginRedirectThenChallenge(t *testing.T) {
	const token = "tok-redirect-origin"
	body := singleManifestBody
	redirectTarget := manifestURL("https", registryOrigin, "mirror/"+repositoryPath, imageTag)
	tokenURL := "https://auth.example.com/token"
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Host == "auth.example.com":
			return tokenResponse(request, `{"token":"`+token+`"}`), nil
		case request.URL.String() == manifestURL("https", registryOrigin, repositoryPath, imageTag):
			return redirectResponse(request, redirectTarget), nil
		case request.URL.String() == redirectTarget && request.Header.Get("Authorization") == "":
			return unauthorizedResponse(request, `Bearer realm="`+tokenURL+`"`), nil
		case request.URL.String() == redirectTarget && request.Header.Get("Authorization") == "Bearer "+token:
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
		}
		t.Fatalf("unexpected request %s %s with Authorization %q", request.Method, request.URL, request.Header.Get("Authorization"))
		return nil, nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a same-origin redirect followed by a challenge: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	captured := transport.captured()
	if len(captured) != 4 {
		t.Fatalf("resolution made %d requests, want 4 (redirect, challenge, token, retry): %v", len(captured), captured)
	}
	wantURLs := []string{
		manifestURL("https", registryOrigin, repositoryPath, imageTag),
		redirectTarget,
		tokenURL,
		redirectTarget,
	}
	wantAuthorization := []string{"", "", "", "Bearer " + token}
	for index := range wantURLs {
		if got := captured[index].URL; got != wantURLs[index] {
			t.Fatalf("request %d URL = %q, want %q", index, got, wantURLs[index])
		}
		if got := capturedHeader(captured, index); got != wantAuthorization[index] {
			t.Fatalf("request %d Authorization = %q, want %q", index, got, wantAuthorization[index])
		}
	}
}

// TestResolveRedirectCap pins the bounded redirect chain for both an explicit
// cap and the default cap.
func TestResolveRedirectCap(t *testing.T) {
	cases := []struct {
		name         string
		maxRedirects int
		wantRequests int
	}{
		{"explicit cap", 1, 2},
		{"default cap", 0, registry.DefaultMaxRedirects + 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{handle: func(call int, request *http.Request) (*http.Response, error) {
				return redirectResponse(request, "https://"+registryOrigin+"/v2/loop/"+strings.Repeat("x", call+1)), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport, MaxRedirects: testCase.maxRedirects})

			image, err := resolveWith(t, resolver, tagReference)
			requireFault(t, err, fault.InvalidInput)
			requireNoDigest(t, image)
			if requests := transport.captured(); len(requests) != testCase.wantRequests {
				t.Fatalf("redirect chain issued %d requests, want %d", len(requests), testCase.wantRequests)
			}
		})
	}
}

// TestResolveRedirectDowngradeRefused pins that an https read never follows a
// redirect down to plaintext http unless the test-only flag allows it.
func TestResolveRedirectDowngradeRefused(t *testing.T) {
	downgrade := manifestURL("http", registryOrigin, repositoryPath, imageTag)
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return redirectResponse(request, downgrade), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.InvalidInput)
	requireNoDigest(t, image)
	if requests := transport.captured(); len(requests) != 1 {
		t.Fatalf("downgrade redirect issued %d requests, want only the origin request", len(requests))
	}
}

// TestResolveInsecureHTTPFlag pins the explicit test-only insecure switch: it
// selects http for the origin request and the credential origin.
func TestResolveInsecureHTTPFlag(t *testing.T) {
	const token = "insecure-token"
	body := singleManifestBody
	source := &staticCredentialSource{
		origin:     "http://" + registryOrigin,
		credential: registry.Credential{Scheme: registry.CredentialBearer, Token: token},
	}
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{
		Transport:         transport,
		Credentials:       source,
		AllowInsecureHTTP: true,
	})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused the insecure test flag: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	requests := transport.captured()
	if want := manifestURL("http", registryOrigin, repositoryPath, imageTag); requests[0].URL != want {
		t.Fatalf("manifest URL = %q, want %q", requests[0].URL, want)
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer "+token)
	}
	requireOrigins(t, source, "http://"+registryOrigin)
}
