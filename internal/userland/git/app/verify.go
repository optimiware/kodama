package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/git"
)

// ErrRetentionUnavailable reports that the public route table has no
// authorized referenced-commits query. The adapter refuses rather than
// returning an empty page, because an empty page would authorize pruning
// commits that may still be referenced.
var ErrRetentionUnavailable = errors.New("the public contract defines no authorized referenced-commits query")

// VerificationClient implements git.VerificationClient over the public
// `client.Transport` boundary. It sends the public `actor.verify` and
// `resource.report` operations and never carries a credential itself: the
// transport sources the mounted service credential on every attempt.
type VerificationClient struct {
	transport client.Transport
}

var _ git.VerificationClient = (*VerificationClient)(nil)

// NewVerificationClient validates the boundary and returns the client.
func NewVerificationClient(transport client.Transport) (*VerificationClient, error) {
	if transport == nil {
		return nil, fmt.Errorf("verification client transport is missing")
	}
	return &VerificationClient{transport: transport}, nil
}

// Verify performs one public actor verification.
func (verification *VerificationClient) Verify(ctx context.Context, request api.VerifyRequest) (api.VerifyResponse, error) {
	if verification == nil || verification.transport == nil {
		return api.VerifyResponse{}, fmt.Errorf("verification client is not configured")
	}
	if err := request.Validate(); err != nil {
		return api.VerifyResponse{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return api.VerifyResponse{}, fmt.Errorf("encode verification request: %w", err)
	}
	result, err := verification.transport.Invoke(ctx, api.Invocation{
		Operation: "actor.verify",
		Request:   api.Request{Input: input},
	})
	if err != nil {
		return api.VerifyResponse{}, err
	}
	response, err := decodePublicResult[api.VerifyResponse](result, "actor.verify")
	if err != nil {
		return api.VerifyResponse{}, err
	}
	return response, nil
}

// Report performs one public verified-resource report. The stable source-event
// identity is the idempotency key so a retry repeats the same receipt and never
// repeats a ref update.
func (verification *VerificationClient) Report(ctx context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	if verification == nil || verification.transport == nil {
		return api.ReportResponse{}, fmt.Errorf("verification client is not configured")
	}
	if err := request.Validate(); err != nil {
		return api.ReportResponse{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return api.ReportResponse{}, fmt.Errorf("encode report request: %w", err)
	}
	result, err := verification.transport.Invoke(ctx, api.Invocation{
		Operation: "resource.report",
		Request: api.Request{
			Input:          input,
			IdempotencyKey: request.SourceEventID,
		},
	})
	if err != nil {
		return api.ReportResponse{}, err
	}
	response, err := decodePublicResult[api.ReportResponse](result, "resource.report")
	if err != nil {
		return api.ReportResponse{}, err
	}
	return response, nil
}

// decodePublicResult decodes one result body with the public operation
// codec. The codec refuses an unknown member at every nesting level, a
// non-object body and trailing data, so a malicious or odd verification server
// cannot smuggle fields past the DTO. An invalid response is a dependency-class
// failure and is never applied.
func decodePublicResult[T any](result api.Result, operation string) (T, error) {
	var zero T
	if result.Operation != operation {
		return zero, fault.New(fault.DependencyUnavailable, fmt.Sprintf("the public response names operation %q, not %q", result.Operation, operation))
	}
	codec, ok := operationCodec(operation)
	if !ok {
		return zero, fault.New(fault.DependencyUnavailable, fmt.Sprintf("operation %q has no frozen public codec", operation))
	}
	decoded, err := codec.DecodeResult(result.Body)
	if err != nil {
		return zero, fault.Wrap(fault.DependencyUnavailable, fmt.Sprintf("the %q response is not a valid public result", operation), err)
	}
	typed, ok := decoded.(T)
	if !ok {
		return zero, fault.New(fault.DependencyUnavailable, fmt.Sprintf("the %q response has an unexpected type", operation))
	}
	return typed, nil
}

// operationCodec returns the public codec for one operation.
func operationCodec(operation string) (api.OperationCodec, bool) {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec, true
		}
	}
	return api.OperationCodec{}, false
}

// RetentionClient implements git.RetentionQuery over the public client
// boundary.
//
// The public contract defines no authorized referenced-commits operation.
// Returning an empty page would be indistinguishable from "nothing is
// referenced" and could authorize pruning live commits, so this adapter
// refuses every query. The public transport is retained so the wiring changes
// in one place if the operation is added.
type RetentionClient struct {
	transport client.Transport
}

var _ git.RetentionQuery = (*RetentionClient)(nil)

// NewRetentionClient validates the public transport and returns the query.
func NewRetentionClient(transport client.Transport) (*RetentionClient, error) {
	if transport == nil {
		return nil, fmt.Errorf("retention query transport is missing")
	}
	return &RetentionClient{transport: transport}, nil
}

// ReferencedCommits refuses: the public route table has no authorized
// referenced-commits query.
func (retention *RetentionClient) ReferencedCommits(ctx context.Context, page api.Page) (git.CommitRefs, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return git.CommitRefs{}, err
	}
	if retention == nil || retention.transport == nil {
		return git.CommitRefs{}, fmt.Errorf("retention query is not configured")
	}
	return git.CommitRefs{}, fmt.Errorf("%w: page cursor %q", ErrRetentionUnavailable, page.Cursor)
}

// TransportConfig configures the public HTTP transport used by the Git and
// agent app processes.
type TransportConfig struct {
	BaseURL        string
	CredentialFile string
	RoundTripper   http.RoundTripper
	Retry          client.RetryPolicy
	Now            func() time.Time
	Sleep          func(time.Duration)
}

// NewTransport builds the public HTTP transport. The mounted credential file is
// read by client.FileCredentialSource on every attempt, so a rotated token
// takes effect without a restart.
func NewTransport(config TransportConfig) (*client.HTTPTransport, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, fmt.Errorf("public base URL is empty")
	}
	if strings.TrimSpace(config.CredentialFile) == "" {
		return nil, fmt.Errorf("mounted credential file is empty")
	}
	retry := config.Retry
	if retry.MaxAttempts == 0 {
		retry = client.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	}
	if err := retry.Validate(); err != nil {
		return nil, err
	}
	roundTripper := config.RoundTripper
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	return &client.HTTPTransport{
		BaseURL:      config.BaseURL,
		RoundTripper: roundTripper,
		Source:       client.FileCredentialSource{Path: config.CredentialFile},
		Retry:        retry,
		Now:          config.Now,
		Sleep:        config.Sleep,
	}, nil
}
