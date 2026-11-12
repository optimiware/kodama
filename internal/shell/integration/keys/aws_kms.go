package keys

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// The AWS KMS decrypt call is one SigV4-signed JSON POST, and the EKS IRSA
// credential exchange is one unsigned form POST to STS, so this client is plain
// net/http and encoding/xml: no provider SDK.

// AWSKMSConfig configures the AWS KMS client. The credential is an EKS IRSA
// (IAM Roles for Service Accounts) web identity: the client exchanges the
// projected service-account token for short-lived role credentials at STS.
type AWSKMSConfig struct {
	// Region is the AWS region whose regional KMS and STS endpoints the client
	// calls and whose name scopes every signature. A key reference naming
	// another region is refused before any credential is acquired.
	Region string
	// RoleARN is the IAM role the service account assumes. On EKS the pod
	// identity webhook injects it as AWS_ROLE_ARN.
	RoleARN string
	// WebIdentityTokenFile is the absolute path of the projected
	// service-account token (AWS_WEB_IDENTITY_TOKEN_FILE). The kubelet rotates
	// the file, so it is read again on every exchange and never at
	// construction; its content is never cached, logged, returned or rendered
	// in an error.
	WebIdentityTokenFile string
	// RoleSessionName names the STS session. Empty selects "kodama".
	RoleSessionName string
	// KMSEndpoint overrides the KMS origin. Empty selects
	// https://kms.<region>.amazonaws.com (amazonaws.com.cn for a cn- region).
	// Only an explicit https URL without userinfo, query or fragment is
	// accepted.
	//
	// It exists for tests and for a composition root that is itself trusted: a
	// production composition must never take this value from operator
	// configuration (an operator-selected origin would receive the session
	// token and a signed decrypt request). The client intentionally does not
	// restrict the host to AWS; the wiring order must keep KMSEndpoint out of
	// operator configuration, or a later composition change must restrict the
	// host.
	KMSEndpoint string
	// STSEndpoint overrides the STS origin. Empty selects
	// https://sts.<region>.amazonaws.com (amazonaws.com.cn for a cn- region).
	// The same grammar and the same warning as KMSEndpoint apply, more
	// strongly: an operator-selected origin would receive the service-account
	// token itself and would choose the credentials the client signs with.
	STSEndpoint string
	// HTTPClient optionally supplies HTTP client settings such as explicit TLS
	// roots. New copies the client, installs a private transport with Proxy=nil
	// (so neither HTTP_PROXY/HTTPS_PROXY nor a caller proxy determines routing)
	// and always refuses redirects; the caller's client is never mutated. Its
	// transport must be an explicit *http.Transport other than the shared
	// http.DefaultTransport. Nil selects an owned transport with verification on.
	HTTPClient *http.Client
	// Now is the clock used for signing and for credential expiry. Nil selects
	// time.Now.
	Now func() time.Time
}

// AWSKMS implements KMSClient over the AWS KMS JSON API.
type AWSKMS struct {
	region      string
	roleARN     string
	tokenFile   string
	sessionName string
	kmsEndpoint string
	stsEndpoint string
	client      *http.Client
	now         func() time.Time
	// openTokenFile is os.Open. It is a field only so a white-box test can
	// count token-file reads.
	openTokenFile func(string) (io.ReadCloser, error)

	// mu guards cached and is held across the STS exchange, so concurrent
	// Decrypt calls make one exchange and the others reuse its result.
	mu     sync.Mutex
	cached *awsRoleCredentials
}

var _ KMSClient = (*AWSKMS)(nil)

// NewAWSKMS validates the configuration and returns the client. It makes no
// request, reads no file and never contacts STS or KMS.
func NewAWSKMS(config AWSKMSConfig) (*AWSKMS, error) {
	if !validAWSRegionName(config.Region) {
		return nil, fault.New(fault.InvalidInput, "aws kms region is invalid")
	}
	if !validAWSRoleARN(config.RoleARN) {
		return nil, fault.New(fault.InvalidInput, "aws kms role ARN (AWS_ROLE_ARN) must be arn:<partition>:iam::<account>:role/<role-name>")
	}
	if !filepath.IsAbs(config.WebIdentityTokenFile) || strings.ContainsRune(config.WebIdentityTokenFile, 0) {
		return nil, fault.New(fault.InvalidInput, "aws web identity token file (AWS_WEB_IDENTITY_TOKEN_FILE) must be an absolute path")
	}
	sessionName := config.RoleSessionName
	if sessionName == "" {
		sessionName = awsDefaultRoleSessionName
	}
	if !validAWSRoleSessionName(sessionName) {
		return nil, fault.New(fault.InvalidInput, "aws kms role session name must be 2 to 64 characters of letters, digits and _+=,.@-")
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(config.Region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	kmsEndpoint, err := awsEndpoint(awsKMSService, config.KMSEndpoint, "https://kms."+config.Region+"."+suffix)
	if err != nil {
		return nil, err
	}
	stsEndpoint, err := awsEndpoint(awsSTSService, config.STSEndpoint, "https://sts."+config.Region+"."+suffix)
	if err != nil {
		return nil, err
	}

	client := &http.Client{}
	if config.HTTPClient != nil {
		copied := *config.HTTPClient
		client = &copied
	}
	owned, err := awsOwnedTransport(client.Transport)
	if err != nil {
		return nil, err
	}
	client.Transport = owned
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Timeout = awsRequestTimeout

	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &AWSKMS{
		region:        config.Region,
		roleARN:       config.RoleARN,
		tokenFile:     config.WebIdentityTokenFile,
		sessionName:   sessionName,
		kmsEndpoint:   kmsEndpoint,
		stsEndpoint:   stsEndpoint,
		client:        client,
		now:           now,
		openTokenFile: func(name string) (io.ReadCloser, error) { return os.Open(name) },
	}, nil
}

// Decrypt unwraps blob with the key named by keyRef ("aws-kms:<key ARN>",
// "aws-kms:<alias ARN>" or "aws-kms:alias/<name>"). The returned bytes are the
// decoded plaintext unmodified; the shared cloud provider enforces the 32-byte
// key-encryption-key length. No response body, header value, token, credential,
// signature, URL or plaintext enters an error.
func (kms *AWSKMS) Decrypt(ctx context.Context, keyRef secret.KeyRef, blob []byte) ([]byte, error) {
	// Cancellation is reported as the context error itself, never as a fault.
	// net/http returns the cause inside a *url.Error, and a body-read failure
	// carries transport detail, so both failure paths in send re-read the
	// context: fault.ClassOf on the resulting dependency_unavailable fault would
	// misreport a cancellation as an STS or KMS outage. This entry check also
	// keeps a cancelled call from parsing the key reference, reading the token
	// file or building a request.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The reference and its region are settled before any credential is
	// acquired: a reference for another region never causes a token-file read,
	// an STS exchange or a signed request.
	reference, err := ParseAWSReference(keyRef)
	if err != nil {
		return nil, err
	}
	if err := reference.ValidateRegion(kms.region); err != nil {
		return nil, err
	}
	// KeyId is the validated reference verbatim, so KMS itself refuses a
	// ciphertext that was wrapped under any other key. EncryptionAlgorithm is
	// left to its SYMMETRIC_DEFAULT default: the key-encryption key is wrapped
	// by a symmetric KMS key.
	keyID := strings.TrimPrefix(string(keyRef), AWSKeyRefPrefix)
	body, err := json.Marshal(struct {
		CiphertextBlob string `json:"CiphertextBlob"`
		KeyID          string `json:"KeyId"`
	}{base64.StdEncoding.EncodeToString(blob), keyID})
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "aws kms request could not be encoded", err)
	}
	// Only a key ARN can be compared with the key ARN KMS reports; an alias
	// names whatever key it currently points at.
	expectedKey := ""
	if reference.ARN && reference.KeyID != "" {
		expectedKey = keyID
	}

	credentials, err := kms.credentials(ctx)
	if err != nil {
		return nil, err
	}
	plaintext, stale, err := kms.decryptOnce(ctx, credentials, body, expectedKey)
	if !stale {
		return plaintext, err
	}
	// KMS refused the cached credentials themselves. Drop them and retry
	// exactly once with a fresh exchange; the second answer is final, and
	// credentials it refuses are dropped too so the next call starts afresh.
	kms.forget(credentials)
	credentials, err = kms.credentials(ctx)
	if err != nil {
		return nil, err
	}
	plaintext, stale, err = kms.decryptOnce(ctx, credentials, body, expectedKey)
	if stale {
		kms.forget(credentials)
	}
	return plaintext, err
}

// decryptOnce sends one signed Decrypt request. stale reports that KMS refused
// the credentials themselves (expired, unknown or mis-signed).
func (kms *AWSKMS) decryptOnce(ctx context.Context, credentials *awsRoleCredentials, body []byte, expectedKey string) (plaintext []byte, stale bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, kms.kmsEndpoint+"/", bytes.NewReader(body))
	if err != nil {
		return nil, false, fault.Wrap(fault.InternalFailure, "aws kms request could not be built", err)
	}
	at := kms.now()
	headers := map[string]string{
		"Content-Type":         "application/x-amz-json-1.1",
		"X-Amz-Date":           awsSigV4Timestamp(at),
		"X-Amz-Security-Token": credentials.sessionToken,
		"X-Amz-Target":         "TrentService.Decrypt",
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	// Every header the request carries is signed, together with the host.
	headers["Host"] = request.URL.Host
	request.Header.Set("Authorization", awsSigV4Authorization(awsSigV4Credential{
		accessKeyID:     credentials.accessKeyID,
		secretAccessKey: credentials.secretAccessKey,
		region:          kms.region,
		service:         "kms",
	}, at, awsSigV4Request{
		method:  http.MethodPost,
		path:    request.URL.EscapedPath(),
		headers: headers,
		payload: body,
	}))

	reply, err := kms.send(ctx, awsKMSService, request)
	if err != nil {
		return nil, false, err
	}
	if reply.status != http.StatusOK {
		name := awsErrorType(reply)
		// Only these three say the credentials themselves were refused.
		stale = name == "ExpiredTokenException" || name == "UnrecognizedClientException" || name == "InvalidSignatureException"
		return nil, stale, awsFault(awsKMSService, awsKMSClass(name, reply.status))
	}
	if reply.oversized {
		return nil, false, fault.New(fault.DependencyUnavailable, "aws kms response exceeds the configured bound")
	}
	var decoded struct {
		KeyID     string `json:"KeyId"`
		Plaintext string `json:"Plaintext"`
	}
	if err := json.Unmarshal(reply.body, &decoded); err != nil || decoded.Plaintext == "" {
		return nil, false, fault.New(fault.DependencyUnavailable, "aws kms returned no plaintext")
	}
	if expectedKey != "" && decoded.KeyID != "" && decoded.KeyID != expectedKey {
		return nil, false, fault.New(fault.DependencyUnavailable, "aws kms decrypted with a key other than the referenced key")
	}
	plaintext, err = base64.StdEncoding.DecodeString(decoded.Plaintext)
	if err != nil {
		return nil, false, fault.New(fault.DependencyUnavailable, "aws kms returned malformed plaintext")
	}
	return plaintext, false, nil
}

// awsKMSClass classifies a refused Decrypt. AWS JSON services answer most
// refusals with HTTP 400 and an error name, so the name decides when it is
// known and the status decides otherwise.
func awsKMSClass(name string, status int) fault.Class {
	switch name {
	case "ExpiredTokenException", "UnrecognizedClientException", "InvalidSignatureException",
		"MissingAuthenticationTokenException", "MissingAuthenticationToken",
		"IncompleteSignatureException", "IncompleteSignature":
		return fault.Unauthenticated
	case "AccessDeniedException":
		return fault.Forbidden
	case "NotFoundException":
		return fault.NotFound
	case "ThrottlingException", "KMSInternalException", "DependencyTimeoutException", "KeyUnavailableException",
		"InternalFailure", "ServiceUnavailable", "RequestTimeoutException":
		return fault.DependencyUnavailable
	case "InvalidCiphertextException", "IncorrectKeyException", "InvalidKeyUsageException", "InvalidGrantTokenException",
		// A disabled key or one in another unusable state is not an outage: no
		// retry succeeds until an operator changes the key, and neither the
		// credential nor the reference is at fault.
		"DisabledException", "KMSInvalidStateException":
		return fault.InvalidInput
	}
	if status == http.StatusNotFound {
		return fault.NotFound
	}
	return awsStatusClass(status)
}
