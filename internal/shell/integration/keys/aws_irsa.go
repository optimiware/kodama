package keys

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// EKS IRSA (IAM Roles for Service Accounts) credentials for the AWS KMS client:
// the projected service-account token is exchanged at STS for short-lived role
// credentials, which are cached in memory until shortly before they expire.
// There is no background refresh: an exchange happens inside the Decrypt call
// that needs it.

// awsDefaultRoleSessionName names the STS session when the configuration gives
// none. It appears in CloudTrail and in the assumed-role ARN.
const awsDefaultRoleSessionName = "kodama"

// awsMaxWebIdentityTokenBytes bounds the projected service-account token. It is
// the documented maximum length of the STS WebIdentityToken parameter.
const awsMaxWebIdentityTokenBytes = 20000

// awsMaxRoleARNBytes is the documented maximum length of the STS RoleArn
// parameter.
const awsMaxRoleARNBytes = 2048

// awsCredentialExpiryMargin is how long before their expiration the cached role
// credentials stop being used, so a request signed now is still valid when KMS
// evaluates it.
const awsCredentialExpiryMargin = 5 * time.Minute

// awsRoleCredentials is one STS answer. It lives only in memory.
type awsRoleCredentials struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	expiration      time.Time
}

// credentials returns the cached role credentials, exchanging the web identity
// token for new ones when there are none or they are inside the expiry margin.
// A failed exchange caches nothing, so the next call tries again.
func (kms *AWSKMS) credentials(ctx context.Context) (*awsRoleCredentials, error) {
	kms.mu.Lock()
	defer kms.mu.Unlock()
	// The lock may have been held across another caller's exchange.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kms.cached != nil && kms.now().Before(kms.cached.expiration.Add(-awsCredentialExpiryMargin)) {
		return kms.cached, nil
	}
	kms.cached = nil
	fresh, err := kms.assumeRole(ctx)
	if err != nil {
		return nil, err
	}
	kms.cached = fresh
	return fresh, nil
}

// forget drops stale from the cache. Credentials another caller has already
// replaced are kept.
func (kms *AWSKMS) forget(stale *awsRoleCredentials) {
	kms.mu.Lock()
	defer kms.mu.Unlock()
	if kms.cached == stale {
		kms.cached = nil
	}
}

// assumeRole performs one AssumeRoleWithWebIdentity exchange. The request is
// unsigned: the web identity token is the credential, and it travels only in
// the form body, never in the URL.
func (kms *AWSKMS) assumeRole(ctx context.Context) (*awsRoleCredentials, error) {
	token, err := kms.readWebIdentityToken()
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {kms.roleARN},
		"RoleSessionName":  {kms.sessionName},
		"WebIdentityToken": {token},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, kms.stsEndpoint+"/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "aws sts request could not be built", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	reply, err := kms.send(ctx, awsSTSService, request)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, awsFault(awsSTSService, awsSTSClass(awsErrorType(reply), reply.status))
	}
	if reply.oversized {
		return nil, fault.New(fault.DependencyUnavailable, "aws sts response exceeds the configured bound")
	}
	var decoded struct {
		XMLName     xml.Name `xml:"AssumeRoleWithWebIdentityResponse"`
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if err := xml.Unmarshal(reply.body, &decoded); err != nil {
		return nil, fault.New(fault.DependencyUnavailable, "aws sts returned no credentials")
	}
	credentials := &awsRoleCredentials{
		accessKeyID:     strings.TrimSpace(decoded.Credentials.AccessKeyID),
		secretAccessKey: strings.TrimSpace(decoded.Credentials.SecretAccessKey),
		sessionToken:    strings.TrimSpace(decoded.Credentials.SessionToken),
	}
	// The access key id and the session token become header values and the
	// secret key signs: anything but printable ASCII is refused here.
	if !awsPrintableToken(credentials.accessKeyID) || !awsPrintableToken(credentials.secretAccessKey) || !awsPrintableToken(credentials.sessionToken) {
		return nil, fault.New(fault.DependencyUnavailable, "aws sts returned no credentials")
	}
	credentials.expiration, err = time.Parse(time.RFC3339, strings.TrimSpace(decoded.Credentials.Expiration))
	if err != nil {
		return nil, fault.New(fault.DependencyUnavailable, "aws sts returned credentials without a valid expiration")
	}
	// Credentials already inside the margin would be exchanged again on every
	// call; they are refused instead of cached or used.
	if !kms.now().Before(credentials.expiration.Add(-awsCredentialExpiryMargin)) {
		return nil, fault.New(fault.DependencyUnavailable, "aws sts returned credentials that are already expiring")
	}
	return credentials, nil
}

// awsSTSClass classifies a refused exchange: the error code decides when it is
// known and the status decides otherwise.
func awsSTSClass(name string, status int) fault.Class {
	switch name {
	case "InvalidIdentityToken", "ExpiredTokenException", "ExpiredToken", "IDPRejectedClaim":
		return fault.Unauthenticated
	case "AccessDenied", "AccessDeniedException":
		return fault.Forbidden
	case "IDPCommunicationError", "Throttling", "ThrottlingException", "InternalFailure", "ServiceUnavailable":
		return fault.DependencyUnavailable
	}
	return awsStatusClass(status)
}

// readWebIdentityToken reads the projected service-account token. It is read on
// every exchange because the kubelet rotates it, bounded, and stripped of one
// trailing newline only. No failure names the token; the private cause of a
// read failure names the path and never the content.
func (kms *AWSKMS) readWebIdentityToken() (string, error) {
	file, err := kms.openTokenFile(kms.tokenFile)
	if err != nil {
		return "", fault.Wrap(fault.Unauthenticated, "aws web identity token file (AWS_WEB_IDENTITY_TOKEN_FILE) could not be read", err)
	}
	defer func() { _ = file.Close() }()
	// One byte for the trailing newline and one more to detect an oversized
	// file without reading it.
	raw, err := io.ReadAll(io.LimitReader(file, awsMaxWebIdentityTokenBytes+2))
	if err != nil {
		return "", fault.Wrap(fault.Unauthenticated, "aws web identity token file (AWS_WEB_IDENTITY_TOKEN_FILE) could not be read", err)
	}
	token := strings.TrimSuffix(string(raw), "\n")
	switch {
	case token == "":
		return "", fault.New(fault.Unauthenticated, "aws web identity token file (AWS_WEB_IDENTITY_TOKEN_FILE) is empty")
	case len(token) > awsMaxWebIdentityTokenBytes:
		return "", fault.New(fault.Unauthenticated, "aws web identity token file (AWS_WEB_IDENTITY_TOKEN_FILE) exceeds the token bound")
	case !awsPrintableToken(token):
		return "", fault.New(fault.Unauthenticated, "aws web identity token file (AWS_WEB_IDENTITY_TOKEN_FILE) does not hold a single token")
	}
	return token, nil
}

// validAWSRoleARN reports whether arn is
// arn:<partition>:iam::<12-digit account>:role/<path/name>. The value is sent
// to STS as a form field, so anything outside the IAM name alphabet, including
// whitespace and control bytes, is refused.
func validAWSRoleARN(arn string) bool {
	parts := strings.SplitN(arn, ":", 6)
	if len(arn) > awsMaxRoleARNBytes || len(parts) != 6 || parts[0] != "arn" || parts[2] != "iam" || parts[3] != "" {
		return false
	}
	if !validAWSPartitionName(parts[1]) || !validAWSAccountID(parts[4]) {
		return false
	}
	name, ok := strings.CutPrefix(parts[5], "role/")
	if !ok {
		return false
	}
	for segment := range strings.SplitSeq(name, "/") {
		if !awsIAMName(segment) {
			return false
		}
	}
	return true
}

// validAWSRoleSessionName applies the STS RoleSessionName grammar
// [\w+=,.@-]{2,64}.
func validAWSRoleSessionName(name string) bool {
	return len(name) >= 2 && len(name) <= 64 && awsIAMName(name)
}

// awsIAMName reports whether value is a non-empty run of the IAM name
// alphabet [\w+=,.@-].
func awsIAMName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z', character >= '0' && character <= '9':
		case strings.IndexByte("_+=,.@-", character) >= 0:
		default:
			return false
		}
	}
	return true
}
