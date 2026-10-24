package secretkeys

import (
	"context"
	"errors"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// NewAWS wraps with one AWS KMS key. keyID is an ARN or key id, not an alias.
func NewAWS(client *kms.Client, keyID string, retained []string) (*Cloud, error) {
	if client == nil {
		return nil, ErrInvalid
	}
	return newCloud("aws", awsFormat, awsCrypter{client}, keyID, retained)
}

type awsCrypter struct{ client *kms.Client }

func (c awsCrypter) Encrypt(ctx context.Context, keyID string, plaintext, aad []byte) ([]byte, error) {
	out, err := c.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId: aws.String(keyID), Plaintext: plaintext,
		EncryptionContext: map[string]string{"binding": bindingContext(aad)},
	})
	if err != nil {
		return nil, classifyAWS(ctx, err)
	}
	return out.CiphertextBlob, nil
}

func (c awsCrypter) Decrypt(ctx context.Context, keyID string, ciphertext, aad []byte) ([]byte, error) {
	out, err := c.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId: aws.String(keyID), CiphertextBlob: ciphertext,
		EncryptionContext: map[string]string{"binding": bindingContext(aad)},
	})
	if err != nil {
		return nil, classifyAWS(ctx, err)
	}
	return out.Plaintext, nil
}

// classifyAWS maps an SDK failure to a package sentinel. The SDK error, which
// can carry request identifiers and provider messages, is never returned.
func classifyAWS(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "InvalidCiphertextException", "IncorrectKeyException":
			return ErrCiphertext
		case "NotFoundException":
			return ErrNotFound
		case "AccessDeniedException", "AccessDenied", "UnrecognizedClientException", "InvalidClientTokenId",
			"ExpiredTokenException", "ExpiredToken", "InvalidSignatureException", "SignatureDoesNotMatch",
			"MissingAuthenticationTokenException", "MissingAuthenticationToken", "IncompleteSignature",
			"DisabledException", "KMSInvalidStateException", "InvalidKeyUsageException", "InvalidArnException":
			return ErrDenied
		case "ThrottlingException", "Throttling", "TooManyRequestsException", "RequestLimitExceeded",
			"LimitExceededException", "KeyUnavailableException", "DependencyTimeoutException",
			"KMSInternalException", "InternalFailure", "ServiceUnavailable", "ServiceUnavailableException":
			return ErrUnavailable
		}
	}
	var resp *smithyhttp.ResponseError
	if errors.As(err, &resp) && resp.HTTPStatusCode() > 0 {
		switch resp.HTTPStatusCode() {
		case http.StatusUnauthorized, http.StatusForbidden:
			return ErrDenied
		case http.StatusNotFound:
			return ErrNotFound
		}
	}
	return ErrUnavailable
}
