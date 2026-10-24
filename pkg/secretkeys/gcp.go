package secretkeys

import (
	"context"
	"errors"
	"net/http"

	kms "cloud.google.com/go/kms/apiv1"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
)

// NewGCP wraps with one Cloud KMS crypto key. name is the full resource name.
func NewGCP(client *kms.KeyManagementClient, name string, retained []string) (*Cloud, error) {
	if client == nil {
		return nil, ErrInvalid
	}
	return newCloud("gcp", gcpFormat, gcpCrypter{client}, name, retained)
}

type gcpCrypter struct{ client *kms.KeyManagementClient }

func (c gcpCrypter) Encrypt(ctx context.Context, name string, plaintext, aad []byte) ([]byte, error) {
	resp, err := c.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name: name, Plaintext: plaintext, AdditionalAuthenticatedData: aad,
	})
	if err != nil {
		return nil, classifyGCP(ctx, err)
	}
	return resp.Ciphertext, nil
}

func (c gcpCrypter) Decrypt(ctx context.Context, name string, ciphertext, aad []byte) ([]byte, error) {
	resp, err := c.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name: name, Ciphertext: ciphertext, AdditionalAuthenticatedData: aad,
	})
	if err != nil {
		return nil, classifyGCP(ctx, err)
	}
	return resp.Plaintext, nil
}

// classifyGCP maps a client failure to a package sentinel. The provider error,
// which can carry resource names and messages, is never returned.
func classifyGCP(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	var api *apierror.APIError
	if !errors.As(err, &api) {
		return ErrUnavailable
	}
	code := codes.Unknown
	if st := api.GRPCStatus(); st != nil {
		code = st.Code()
	}
	if code == codes.Unknown || code == codes.OK {
		switch api.HTTPCode() {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = codes.PermissionDenied
		case http.StatusNotFound:
			code = codes.NotFound
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			code = codes.InvalidArgument
		case http.StatusPreconditionFailed:
			code = codes.FailedPrecondition
		}
	}
	switch code {
	case codes.Unauthenticated, codes.PermissionDenied, codes.FailedPrecondition:
		return ErrDenied
	case codes.NotFound:
		return ErrNotFound
	case codes.InvalidArgument, codes.DataLoss:
		return ErrCiphertext
	default:
		return ErrUnavailable
	}
}
