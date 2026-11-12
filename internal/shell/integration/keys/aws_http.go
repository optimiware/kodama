package keys

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// HTTP plumbing shared by the AWS KMS client's two requests (the STS exchange
// and the KMS decrypt): the owned transport, the endpoint grammar, one bounded
// request and the reading of an AWS refusal.

// awsMaxResponse bounds every response body the client reads: a decrypt reply,
// an STS credential document and an error document are each a few kilobytes.
const awsMaxResponse int64 = 64 << 10

// awsRequestTimeout bounds one STS or KMS request as a whole. The caller's
// context can shorten it further and is always honoured.
const awsRequestTimeout = 30 * time.Second

// Names the public faults use for the two endpoints.
const (
	awsKMSService = "aws kms"
	awsSTSService = "aws sts"
)

// awsStatusClass classifies a refusal that carries no known error name.
func awsStatusClass(status int) fault.Class {
	switch {
	case status == http.StatusUnauthorized:
		return fault.Unauthenticated
	case status == http.StatusForbidden:
		return fault.Forbidden
	case status == http.StatusTooManyRequests, status >= http.StatusInternalServerError:
		return fault.DependencyUnavailable
	case status >= http.StatusBadRequest:
		return fault.InvalidInput
	default:
		// 1xx, other 2xx and 3xx: nothing but 200 is accepted, even when the
		// body is valid.
		return fault.DependencyUnavailable
	}
}

// awsFault is the public fault for a refusal of the given class. It renders no
// response byte: not the error name, the message or a header value.
func awsFault(service string, class fault.Class) error {
	switch class {
	case fault.Unauthenticated:
		return fault.New(class, service+" rejected the credential")
	case fault.Forbidden:
		return fault.New(class, service+" denied the request for this role")
	case fault.NotFound:
		return fault.New(class, service+" key was not found")
	case fault.InvalidInput:
		return fault.New(class, service+" rejected the request")
	default:
		return fault.New(fault.DependencyUnavailable, service+" is unavailable")
	}
}

// awsReply is the part of one response the client looks at. body is nil when
// the response exceeded awsMaxResponse.
type awsReply struct {
	status    int
	errorType string
	body      []byte
	oversized bool
}

// send performs one request and reads its body up to the bound. The status is
// not interpreted here.
func (kms *AWSKMS) send(ctx context.Context, service string, request *http.Request) (awsReply, error) {
	response, err := kms.client.Do(request)
	if err != nil {
		// See the Decrypt entry comment: a cancelled request is cancellation,
		// not an unavailable dependency.
		if contextErr := ctx.Err(); contextErr != nil {
			return awsReply{}, contextErr
		}
		// A transport failure can name the URL and its private dial cause; the
		// public fault carries neither, and never a header value.
		return awsReply{}, fault.Wrap(fault.DependencyUnavailable, service+" is unavailable", err)
	}
	defer func() { _ = response.Body.Close() }()
	// Read at most one byte over the bound so an oversized body is recognised
	// instead of truncated into a parse failure.
	body, err := io.ReadAll(io.LimitReader(response.Body, awsMaxResponse+1))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return awsReply{}, contextErr
		}
		return awsReply{}, fault.Wrap(fault.DependencyUnavailable, service+" response could not be read", err)
	}
	reply := awsReply{status: response.StatusCode, errorType: response.Header.Get("X-Amzn-Errortype")}
	if int64(len(body)) > awsMaxResponse {
		reply.oversized = true
		return reply, nil
	}
	reply.body = body
	return reply, nil
}

// awsErrorType returns the error name of a refusal: the X-Amzn-Errortype header
// when present, otherwise the "__type" or "code" member of a JSON error
// document, otherwise the Code of an XML one. The value is only ever compared
// with known names. A legacy value may carry a "namespace#" prefix or a
// ":uri" suffix; both are dropped.
func awsErrorType(reply awsReply) string {
	name := reply.errorType
	if name == "" {
		var document struct {
			Type string `json:"__type"`
			Code string `json:"code"`
		}
		if json.Unmarshal(reply.body, &document) == nil {
			name = document.Type
			if name == "" {
				name = document.Code
			}
		}
	}
	if name == "" {
		var document struct {
			Code string `xml:"Error>Code"`
		}
		if xml.Unmarshal(reply.body, &document) == nil {
			name = document.Code
		}
	}
	name, _, _ = strings.Cut(name, ":")
	if _, after, found := strings.Cut(name, "#"); found {
		name = after
	}
	return strings.TrimSpace(name)
}

// awsEndpoint validates one service origin under the Scaleway BaseURL rule: the
// client never downgrades to cleartext, never carries an embedded credential,
// query or fragment, and trims only a trailing path separator.
func awsEndpoint(service, value, fallback string) (string, error) {
	endpoint := strings.TrimRight(value, "/")
	if endpoint == "" {
		return fallback, nil
	}
	if strings.ContainsAny(endpoint, " \t\r\n") {
		return "", fault.New(fault.InvalidInput, service+" endpoint contains whitespace")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fault.New(fault.InvalidInput, service+" endpoint is not a valid URL")
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fault.New(fault.InvalidInput, service+" endpoint must be an explicit https URL")
	}
	if parsed.User != nil {
		return "", fault.New(fault.InvalidInput, service+" endpoint must not carry userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", fault.New(fault.InvalidInput, service+" endpoint must not carry a query or fragment")
	}
	return endpoint, nil
}

// awsOwnedTransport returns the private transport New installs, under the
// Scaleway client's rules: a nil transport gets that client's explicit defaults
// with no proxy function; a caller-supplied *http.Transport is cloned so its
// explicit TLS, dial and HTTP/2 settings are preserved and its proxy is
// cleared. An opaque RoundTripper, a typed nil and the shared
// http.DefaultTransport are refused rather than silently trusted.
func awsOwnedTransport(transport http.RoundTripper) (*http.Transport, error) {
	if transport == nil {
		return newScalewayTransport(), nil
	}
	caller, ok := transport.(*http.Transport)
	if !ok {
		return nil, fault.New(fault.InvalidInput, "aws kms client must use an explicit HTTP transport")
	}
	if caller == nil {
		return nil, fault.New(fault.InvalidInput, "aws kms client supplies a nil HTTP transport")
	}
	if http.DefaultTransport != nil && caller == http.DefaultTransport {
		return nil, fault.New(fault.InvalidInput, "aws kms client must not use the shared default HTTP transport")
	}
	owned := caller.Clone()
	owned.Proxy = nil
	return owned, nil
}

// awsPrintableToken reports whether value is a non-empty run of printable
// ASCII without a space: the only shape a token or credential that becomes a
// header or form value may have.
func awsPrintableToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] <= ' ' || value[index] >= 0x7f {
			return false
		}
	}
	return true
}
