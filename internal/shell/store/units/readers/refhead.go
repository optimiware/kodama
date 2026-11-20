package readers

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

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

// flexBool decodes a JSON boolean or a string-encoded boolean ("true"/"false")
// so verified event payloads emitted by resource.report (which encodes subject
// fields as JSON strings) decode cleanly alongside native booleans.
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*b = false
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		if s == "" {
			*b = false
			return nil
		}
		parsed, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		*b = flexBool(parsed)
		return nil
	}
	var native bool
	if err := json.Unmarshal(trimmed, &native); err != nil {
		return err
	}
	*b = flexBool(native)
	return nil
}

// gitRefUpdatedPayload is the durable JSON payload of one committed verified
// git.ref.updated event produced by resource.report.
type gitRefUpdatedPayload struct {
	Operation    string   `json:"operation"`
	Resource     string   `json:"resource"`
	Ref          string   `json:"ref"`
	ExpectedBase string   `json:"expected_base"`
	Candidate    string   `json:"candidate"`
	Deleted      flexBool `json:"deleted"`
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

var (
	_ request.RefHeadReader      = (*RefHeadReader)(nil)
	_ request.BatchRefHeadReader = (*RefHeadReader)(nil)
)

// LatestRefHead returns the latest committed verified git.ref.updated commit
// for (scope, resourceID, ref), ordered strictly by publish_sequence DESC.
// Only resource.verified events of report_type "git.ref.updated" from a
// service source whose payload matches (git.ref.update, resourceID, ref) are
// considered; custom observations and other scopes are never visible.
func (reader *RefHeadReader) LatestRefHead(ctx context.Context, scope tenancy.Scope, resourceID, ref string) (request.RefHead, error) {
	if ref == "" {
		return request.RefHead{}, fault.New(fault.InvalidInput, "ref head lookup requires a ref")
	}
	heads, err := reader.LatestRefHeads(ctx, scope, resourceID, []string{ref})
	if err != nil {
		return request.RefHead{}, err
	}
	return heads[ref], nil
}

// LatestRefHeads resolves the latest committed verified git.ref.updated state
// for every requested ref on one (scope, resourceID) in a single descending
// paginated scan, stopping as soon as all requested refs have been found.
func (reader *RefHeadReader) LatestRefHeads(ctx context.Context, scope tenancy.Scope, resourceID string, refs []string) (map[string]request.RefHead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !scope.ProjectScoped() {
		return nil, fault.New(fault.InvalidInput, "ref head lookup requires a project scope")
	}
	if resourceID == "" {
		return nil, fault.New(fault.InvalidInput, "ref head lookup requires a resource id")
	}
	wanted := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref == "" {
			return nil, fault.New(fault.InvalidInput, "ref head lookup requires a ref")
		}
		wanted[ref] = struct{}{}
	}
	resolved := make(map[string]request.RefHead, len(wanted))
	if len(wanted) == 0 {
		return resolved, nil
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
			return nil, err
		}
		for _, row := range page.Rows {
			data, err := rowBytes(row, "data")
			if err != nil {
				return nil, err
			}
			if len(data) == 0 {
				continue
			}
			var payload gitRefUpdatedPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				continue
			}
			if payload.Operation != gitRefUpdateOperation || payload.Resource != resourceID {
				continue
			}
			if _, needed := wanted[payload.Ref]; !needed {
				continue
			}
			if _, already := resolved[payload.Ref]; already {
				continue
			}
			if bool(payload.Deleted) || request.IsUnbornGitOID(payload.Candidate) {
				resolved[payload.Ref] = request.RefHead{Deleted: true, Found: true}
			} else {
				resolved[payload.Ref] = request.RefHead{Commit: payload.Candidate, Found: true}
			}
			if len(resolved) == len(wanted) {
				return resolved, nil
			}
		}
		if !page.HasMore || page.Next == "" {
			return resolved, nil
		}
		query.After = page.Next
	}
}
