package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
)

// List limits (docs/contracts/http-api.md).
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// PageRequest is a parsed, scope-verified list request.
type PageRequest struct {
	// Limit is the requested page size, 1..MaxLimit.
	Limit int
	// Position is the verified continuation position, nil for the first page.
	Position []byte
}

// FetchLimit is the number of rows the module should read: one more than the
// page, so BuildPage can tell whether another page exists without a count.
func (p PageRequest) FetchLimit() int { return p.Limit + 1 }

// ParseLimit reads the limit query parameter. Absent means DefaultLimit; a
// repeated, non-numeric, zero, negative or too large value is rejected with 400
// rather than silently changed, so clients never receive a page size they did
// not ask for.
func ParseLimit(r *http.Request) (int, error) {
	vs := r.URL.Query()["limit"]
	switch len(vs) {
	case 0:
		return DefaultLimit, nil
	case 1:
	default:
		return 0, BadRequestError("limit must be given once")
	}
	n, err := strconv.Atoi(vs[0])
	if err != nil || n < 1 || n > MaxLimit || vs[0] != strconv.Itoa(n) {
		return 0, BadRequestError(fmt.Sprintf("limit must be an integer from 1 to %d", MaxLimit))
	}
	return n, nil
}

// CheckQuery rejects query parameters other than limit, cursor and allowed, so
// an unrecognized filter can never be ignored while still being baked into a
// cursor's expectations. Repeated cursors are rejected too.
func CheckQuery(r *http.Request, allowed ...string) error {
	ok := map[string]bool{"limit": true, "cursor": true}
	for _, a := range allowed {
		ok[a] = true
	}
	q := r.URL.Query()
	for k := range q {
		if !ok[k] {
			return BadRequestError("unsupported query parameter " + strconv.Quote(k))
		}
	}
	if len(q["cursor"]) > 1 {
		return BadRequestError("cursor must be given once")
	}
	return nil
}

// ParsePage parses limit and cursor for a list endpoint. scope must carry the
// normalized filters and sort the module is about to apply. A cursor issued
// under a different scope is 400 invalid_cursor. Call it only after the router
// authorized the request; the cursor adds no authority.
func (c *CursorCodec) ParsePage(r *http.Request, scope CursorScope) (PageRequest, error) {
	limit, err := ParseLimit(r)
	if err != nil {
		return PageRequest{}, err
	}
	req := PageRequest{Limit: limit}
	if q := r.URL.Query(); len(q["cursor"]) > 0 {
		if len(q["cursor"]) > 1 {
			return PageRequest{}, BadRequestError("cursor must be given once")
		}
		pos, err := c.Decode(scope, q.Get("cursor"))
		if err != nil {
			return PageRequest{}, err
		}
		req.Position = pos
	}
	return req, nil
}

// ListPage is the list envelope: {items, nextCursor}. NextCursor is JSON null
// when the list is exhausted; Items is never null.
type ListPage[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// BuildPage turns up to FetchLimit rows into a page. When more than Limit rows
// came back, the extra row is dropped and position is called with the last
// returned row to build the continuation. position must return a key that
// identifies that row in the module's deterministic order, whose last component
// is a unique tie-breaker.
func BuildPage[T any](c *CursorCodec, scope CursorScope, req PageRequest, rows []T, position func(T) ([]byte, error)) (ListPage[T], error) {
	if req.Limit < 1 || req.Limit > MaxLimit {
		return ListPage[T]{}, InternalServerError(fmt.Errorf("httpapi: invalid page limit %d", req.Limit))
	}
	if len(rows) > req.Limit+1 {
		return ListPage[T]{}, InternalServerError(fmt.Errorf("httpapi: module returned %d rows for a page of %d", len(rows), req.Limit))
	}
	page := ListPage[T]{Items: rows}
	if page.Items == nil {
		page.Items = []T{}
	}
	if len(rows) > req.Limit {
		page.Items = rows[:req.Limit]
		pos, err := position(page.Items[req.Limit-1])
		if err != nil {
			return ListPage[T]{}, InternalServerError(err)
		}
		tok, err := c.Encode(scope, pos)
		if err != nil {
			return ListPage[T]{}, InternalServerError(err)
		}
		page.NextCursor = &tok
	}
	return page, nil
}
