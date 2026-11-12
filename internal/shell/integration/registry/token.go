package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

// maxTokenResponse bounds one token endpoint reply (a JWT and a few fields).
const maxTokenResponse = 64 << 10

// bearerChallenge is a parsed `WWW-Authenticate: Bearer ...` registry challenge
// (the Docker/OCI token authentication handshake).
type bearerChallenge struct {
	realm   string
	service string
	scopes  []string
}

// parseBearerChallenge reads the first usable Bearer challenge across every
// WWW-Authenticate value. Each value may carry several comma-separated
// challenges (`Basic realm="...", Bearer realm="..."`); they are scanned in
// order, so a preceding Basic challenge never hides the Bearer one. Only the
// realm, service and scope parameters are used; anything unrecognized is
// ignored, and a Bearer challenge without a realm is not usable.
func parseBearerChallenge(headers []string) (bearerChallenge, bool) {
	for _, header := range headers {
		for _, parts := range splitChallenges(header) {
			if len(parts) == 0 {
				continue
			}
			scheme, rest, _ := strings.Cut(strings.TrimSpace(parts[0]), " ")
			if !strings.EqualFold(scheme, "Bearer") {
				continue
			}
			challenge := bearerChallenge{}
			params := parts[1:]
			if rest = strings.TrimSpace(rest); rest != "" {
				params = append([]string{rest}, params...)
			}
			for _, part := range params {
				key, value, ok := parseAuthParam(part)
				if !ok {
					continue
				}
				switch strings.ToLower(key) {
				case "realm":
					challenge.realm = value
				case "service":
					challenge.service = value
				case "scope":
					if value != "" {
						challenge.scopes = append(challenge.scopes, strings.Fields(value)...)
					}
				}
			}
			if challenge.realm != "" {
				return challenge, true
			}
		}
	}
	return bearerChallenge{}, false
}

// splitChallenges splits one header value into challenges. A comma outside a
// quoted string separates parts; a part whose first field is a bare token
// (no '=') starts a new challenge, and every other part continues the current
// one. Quoted strings honor backslash escapes, so a comma or quote inside a
// quoted parameter never splits a challenge.
func splitChallenges(header string) [][]string {
	var challenges [][]string
	var current []string
	for _, part := range splitOutsideQuotes(header) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !startsChallenge(part) {
			if len(current) == 0 {
				// A parameter before any challenge scheme is malformed.
				continue
			}
			current = append(current, part)
			continue
		}
		if len(current) > 0 {
			challenges = append(challenges, current)
		}
		current = []string{part}
	}
	if len(current) > 0 {
		challenges = append(challenges, current)
	}
	return challenges
}

// splitOutsideQuotes splits on commas that are not inside a quoted string.
func splitOutsideQuotes(value string) []string {
	var parts []string
	var current strings.Builder
	quoted := false
	escaped := false
	for _, character := range value {
		switch {
		case escaped:
			escaped = false
			current.WriteRune(character)
		case character == '\\' && quoted:
			escaped = true
			current.WriteRune(character)
		case character == '"':
			quoted = !quoted
			current.WriteRune(character)
		case character == ',' && !quoted:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(character)
		}
	}
	return append(parts, current.String())
}

// startsChallenge reports whether a comma-separated part begins a new
// challenge rather than continuing the current one's authentication
// parameters. A part continues the current challenge when its first field is
// followed (after optional spaces) by '=', as in `realm="..."` or `realm = "..."`.
func startsChallenge(part string) bool {
	first, rest, found := strings.Cut(part, " ")
	if !found {
		return !strings.Contains(first, "=")
	}
	if first == "" {
		return false
	}
	if strings.Contains(first, "=") {
		return false
	}
	return !strings.HasPrefix(strings.TrimLeft(rest, " \t"), "=")
}

// parseAuthParam parses one `key=value` authentication parameter. The value is
// either a quoted string (with backslash escapes and commas allowed inside) or
// a bare token. A malformed parameter is ignored by the caller.
func parseAuthParam(part string) (string, string, bool) {
	key, rest, ok := strings.Cut(part, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return "", "", false
	}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, `"`) {
		value, ok := unquoteAuthValue(rest)
		return key, value, ok
	}
	if rest == "" {
		return "", "", false
	}
	for _, character := range rest {
		if character <= 0x20 || character == 0x7f || character == ',' || character == '"' {
			return "", "", false
		}
	}
	return key, rest, true
}

func unquoteAuthValue(value string) (string, bool) {
	var parsed strings.Builder
	escaped := false
	for index := 1; index < len(value); index++ {
		character := value[index]
		switch {
		case escaped:
			escaped = false
			parsed.WriteByte(character)
		case character == '\\':
			escaped = true
		case character == '"':
			if strings.TrimSpace(value[index+1:]) != "" {
				return "", false
			}
			return parsed.String(), true
		case character < 0x20 || character == 0x7f:
			return "", false
		default:
			parsed.WriteByte(character)
		}
	}
	return "", false
}

// absoluteRealm reports whether a challenge realm names an absolute URL. A
// registry that sends a relative realm (the distribution reference registry's
// test controller does) cannot be fetched, so the 401 stays an ordinary
// authentication rejection instead of becoming a realm error.
func absoluteRealm(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme != ""
}

// fetchToken performs the token request the registry challenged for. The
// installation credential is offered only if the credential source has one for
// the realm's own origin, so a registry cannot redirect a credential to a host
// the operator never configured; otherwise the request is anonymous, which is
// what public images need. The token is returned for one request only.
func (resolver *Resolver) fetchToken(ctx context.Context, challenge bearerChallenge) (string, error) {
	realm, err := url.Parse(challenge.realm)
	if err != nil || realm.Host == "" || realm.User != nil || strings.Contains(challenge.realm, "#") ||
		(realm.Scheme != "https" && !(resolver.insecure && realm.Scheme == "http")) {
		return "", fault.New(fault.InvalidInput, "registry token realm must be an https URL")
	}
	query := realm.Query()
	if challenge.service != "" {
		query.Set("service", challenge.service)
	}
	for _, scope := range challenge.scopes {
		query.Add("scope", scope)
	}
	realm.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fault.New(fault.InvalidInput, "registry token request is invalid")
	}
	credential, err := resolver.credential(ctx, realm.Scheme+"://"+realm.Host)
	if err != nil {
		return "", err
	}
	if authorization, ok, err := credential.authorization(); err != nil {
		return "", err
	} else if ok {
		request.Header.Set("Authorization", authorization)
	}
	request.Header.Set("Accept", "application/json")

	response, err := resolver.transport.RoundTrip(request)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return "", deadlineFault(contextError)
		}
		return "", fault.Wrap(fault.DependencyUnavailable, "registry token endpoint is unavailable", err)
	}
	if response == nil {
		return "", fault.New(fault.DependencyUnavailable, "registry token endpoint returned no response")
	}
	defer func() { _ = response.Body.Close() }()
	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
		return "", fault.New(fault.Unauthenticated, "registry token request was rejected")
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= http.StatusInternalServerError:
		return "", fault.New(fault.DependencyUnavailable, "registry token endpoint is unavailable")
	default:
		return "", fault.New(fault.InvalidInput, "registry token endpoint returned an unexpected status")
	}
	body, err := readBounded(response.Body, maxTokenResponse)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return "", deadlineFault(contextError)
		}
		return "", fault.New(fault.InvalidInput, "registry token response is unreadable")
	}
	if int64(len(body)) > maxTokenResponse {
		return "", fault.New(fault.InvalidInput, "registry token response is too large")
	}
	var reply struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return "", fault.New(fault.InvalidInput, "registry token response is not JSON")
	}
	token := reply.Token
	if token == "" {
		token = reply.AccessToken
	}
	if !validTokenValue(token) {
		return "", fault.New(fault.InvalidInput, "registry token response carries no usable token")
	}
	return token, nil
}

// validTokenValue refuses an empty token and any value that cannot be carried
// in an Authorization header without changing its meaning: a bearer value is
// 7-bit visible ASCII, so a control byte (including NUL, tab and CR/LF), DEL or
// any non-ASCII rune is refused.
func validTokenValue(token string) bool {
	if token == "" {
		return false
	}
	for _, character := range token {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}
