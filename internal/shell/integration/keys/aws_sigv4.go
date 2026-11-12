package keys

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, Authorization-header form. The AWS KMS client sends
// one request shape (POST /, no query, five single-valued headers), so this is
// a small standard-library signer rather than a provider SDK. It still encodes
// the canonical URI and query the documented way, because it is pinned by the
// published AWS test suite rather than by vectors derived from itself.
//
// Not implemented, because the client never sends it: dot-segment and
// duplicate-slash path normalisation, multi-valued headers, query-string
// (presigned) signing and SigV4a.
const (
	awsSigV4Algorithm  = "AWS4-HMAC-SHA256"
	awsSigV4Terminator = "aws4_request"
	awsSigV4TimeFormat = "20060102T150405Z"
	awsSigV4DateFormat = "20060102"
)

// awsSigV4Credential is the signing identity and its credential scope.
type awsSigV4Credential struct {
	accessKeyID     string
	secretAccessKey string
	region          string
	service         string
}

// awsSigV4Request is the signed part of one request. path is the absolute path
// exactly as sent on the request line. headers holds every header that is
// signed, in any case, including host and x-amz-date; a header left out is
// sent unsigned.
type awsSigV4Request struct {
	method  string
	path    string
	query   url.Values
	headers map[string]string
	payload []byte
}

// awsSigV4Timestamp renders the X-Amz-Date value for at.
func awsSigV4Timestamp(at time.Time) string {
	return at.UTC().Format(awsSigV4TimeFormat)
}

// awsSigV4Authorization returns the Authorization header value for request
// signed at the given time. The caller sends awsSigV4Timestamp(at) as
// X-Amz-Date and lists that header in request.headers.
func awsSigV4Authorization(credential awsSigV4Credential, at time.Time, request awsSigV4Request) string {
	canonical, signedHeaders := awsSigV4CanonicalRequest(request)
	date := at.UTC().Format(awsSigV4DateFormat)
	scope := date + "/" + credential.region + "/" + credential.service + "/" + awsSigV4Terminator
	key := awsSigV4SigningKey(credential.secretAccessKey, date, credential.region, credential.service)
	signature := hex.EncodeToString(awsSigV4HMAC(key, awsSigV4StringToSign(at, scope, canonical)))
	return awsSigV4Algorithm + " Credential=" + credential.accessKeyID + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

// awsSigV4CanonicalRequest returns the canonical request and its signed-header
// list: method, URI-encoded path, sorted URI-encoded query, lowercase sorted
// headers with trimmed single-spaced values, and the hex SHA-256 of the payload.
func awsSigV4CanonicalRequest(request awsSigV4Request) (canonical, signedHeaders string) {
	path := request.path
	if path == "" {
		path = "/"
	}
	values := make(map[string]string, len(request.headers))
	names := make([]string, 0, len(request.headers))
	for name, value := range request.headers {
		name = strings.ToLower(name)
		values[name] = strings.Join(strings.Fields(value), " ")
		names = append(names, name)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name + ":" + values[name] + "\n")
	}
	signedHeaders = strings.Join(names, ";")
	payloadHash := sha256.Sum256(request.payload)
	canonical = strings.Join([]string{
		request.method,
		awsSigV4URIEncode(path, true),
		awsSigV4CanonicalQuery(request.query),
		headers.String(),
		signedHeaders,
		hex.EncodeToString(payloadHash[:]),
	}, "\n")
	return canonical, signedHeaders
}

// awsSigV4CanonicalQuery URI-encodes every name and value and sorts the pairs
// by encoded name, then by encoded value. Sorting the joined "name=value"
// strings instead would misorder a name that is a prefix of another.
func awsSigV4CanonicalQuery(query url.Values) string {
	type pair struct{ name, value string }
	pairs := make([]pair, 0, len(query))
	for name, values := range query {
		for _, value := range values {
			pairs = append(pairs, pair{awsSigV4URIEncode(name, false), awsSigV4URIEncode(value, false)})
		}
	}
	sort.Slice(pairs, func(left, right int) bool {
		if pairs[left].name != pairs[right].name {
			return pairs[left].name < pairs[right].name
		}
		return pairs[left].value < pairs[right].value
	})
	encoded := make([]string, len(pairs))
	for index, entry := range pairs {
		encoded[index] = entry.name + "=" + entry.value
	}
	return strings.Join(encoded, "&")
}

// awsSigV4URIEncode percent-encodes every byte except the RFC 3986 unreserved
// set, with uppercase hex and a space as %20. keepSlash leaves the path
// separator alone. net/url's escapers differ ('+' for a space, extra unescaped
// bytes), so they cannot be used here.
func awsSigV4URIEncode(value string, keepSlash bool) string {
	const upperHex = "0123456789ABCDEF"
	var encoded strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'A' && character <= 'Z',
			character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '-', character == '.', character == '_', character == '~',
			character == '/' && keepSlash:
			encoded.WriteByte(character)
		default:
			encoded.WriteByte('%')
			encoded.WriteByte(upperHex[character>>4])
			encoded.WriteByte(upperHex[character&0x0f])
		}
	}
	return encoded.String()
}

// awsSigV4StringToSign binds the algorithm, the request time, the credential
// scope and the hash of the canonical request.
func awsSigV4StringToSign(at time.Time, scope, canonical string) string {
	hash := sha256.Sum256([]byte(canonical))
	return strings.Join([]string{
		awsSigV4Algorithm,
		awsSigV4Timestamp(at),
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")
}

// awsSigV4SigningKey derives the per-day, per-region, per-service signing key
// from the secret access key.
func awsSigV4SigningKey(secretAccessKey, date, region, service string) []byte {
	key := awsSigV4HMAC([]byte("AWS4"+secretAccessKey), date)
	key = awsSigV4HMAC(key, region)
	key = awsSigV4HMAC(key, service)
	return awsSigV4HMAC(key, awsSigV4Terminator)
}

func awsSigV4HMAC(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
