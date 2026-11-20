package readers

import (
	"context"
	"encoding/json"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

const (
	gitRefUpdatedReportType = "git.ref.updated"
	gitRefUpdateOperation   = "git.ref.update"
)

// gitRefUpdatedPayload is the durable JSON payload of one committed verified
// git.ref.updated event produced by resource.report.
type gitRefUpdatedPayload struct {
	Operation    string `json:"operation"`
	Resource     string `json:"resource"`
	Ref          string `json:"ref"`
	ExpectedBase string `json:"expected_base"`
	Candidate    string `json:"candidate"`
	Deleted      bool   `json:"deleted"`
}

// RefHeadReader projects the latest verified commit for (scope, resource, ref)
// strictly from committed resource.verified / git.ref.updated rows in the
// durable events table. It never calls Git synchronously and works identically
// over both PostgreSQL and in-memory store scanners.
type RefHeadReader struct {
	scanner store.Scanner
}

// NewRefHeadReader builds the store-backed verified ref-head reader.
func NewRefHeadReader(scanner store.Scanner) (*RefHeadReader, error) {
	if scanner == nil {
		return nil, fault.New(fault.InvalidInput, "ref head reader requires a scanner")
	}
	return &RefHeadReader{scanner: scanner}, nil
}

var _ request.RefHeadReader = (*RefHeadReader)(nil)

// LatestRefHead returns the latest committed verified git.ref.updated commit
// for (scope, resourceID, ref), ordered strictly by publish_sequence DESC.
// Only resource.verified events of report_type "git.ref.updated" from a
// service source whose payload matches (git.ref.update, resourceID, ref) are
// considered; custom observations and other scopes are never visible.
func (reader *RefHeadReader) LatestRefHead(ctx context.Context, scope tenancy.Scope, resourceID, ref string) (request.RefHead, error) {
	if err := ctx.Err(); err != nil {
		return request.RefHead{}, err
	}
	if err := scope.Validate(); err != nil {
		return request.RefHead{}, err
	}
	if !scope.ProjectScoped() {
		return request.RefHead{}, fault.New(fault.InvalidInput, "ref head lookup requires a project scope")
	}
	if resourceID == "" {
		return request.RefHead{}, fault.New(fault.InvalidInput, "ref head lookup requires a resource id")
	}
	if ref == "" {
		return request.RefHead{}, fault.New(fault.InvalidInput, "ref head lookup requires a ref")
	}

	query := store.ScanQuery{
		Table: "events",
		Equals: map[string]any{
			"type":        string(event.TypeVerifiedReport),
			"report_type": gitRefUpdatedReportType,
			"source_kind": string(event.SourceService),
			"subject_id":  resourceID,
		},
		OrderBy: []store.Order{{Column: "publish_sequence", Desc: true}},
		Limit:   store.MaxQueryLimit,
	}
	for {
		page, err := reader.scanner.Query(ctx, scope, query)
		if err != nil {
			return request.RefHead{}, err
		}
		for _, row := range page.Rows {
			data, err := rowBytes(row, "data")
			if err != nil {
				return request.RefHead{}, err
			}
			if len(data) == 0 {
				continue
			}
			var payload gitRefUpdatedPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				continue
			}
			if payload.Operation != gitRefUpdateOperation || payload.Resource != resourceID || payload.Ref != ref {
				continue
			}
			if payload.Deleted || request.IsUnbornGitOID(payload.Candidate) {
				return request.RefHead{Deleted: true, Found: true}, nil
			}
			return request.RefHead{Commit: payload.Candidate, Found: true}, nil
		}
		if !page.HasMore || page.Next == "" {
			return request.RefHead{}, nil
		}
		query.After = page.Next
	}
}
