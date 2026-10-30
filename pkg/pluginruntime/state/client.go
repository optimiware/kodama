// Package state provides the thin runtime client used by container plugins.
// It deliberately contains no deployment or provider semantics.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
)

// ErrNotFound is returned for a missing entry.
var ErrNotFound = fmt.Errorf("state: not found")

// HTTPError is a non-success response from the state API.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("state API: %d %s: %s", e.Status, e.Code, e.Message)
}

// Client accesses one attempt's state binding.
type Client struct {
	BaseURL   string
	Attempt   string
	Binding   string
	TokenFile string
	HTTP      *http.Client
}

// New constructs a client. Attempt and Binding are required.
func New(baseURL, attempt, binding, tokenFile string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" || attempt == "" || binding == "" {
		return nil, fmt.Errorf("state: base URL, attempt and binding are required")
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), Attempt: attempt, Binding: binding,
		TokenFile: tokenFile, HTTP: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Read reads one key and returns its ETag.
func (c *Client) Read(ctx context.Context, key string) (statev1.Entry, string, error) {
	req, err := c.request(ctx, http.MethodGet, key, "", nil)
	if err != nil {
		return statev1.Entry{}, "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return statev1.Entry{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return statev1.Entry{}, "", ErrNotFound
	}
	if err := responseError(resp); err != nil {
		return statev1.Entry{}, "", err
	}
	var entry statev1.Entry
	if err := json.NewDecoder(io.LimitReader(resp.Body, statev1.MaxValueBytes+16<<10)).Decode(&entry); err != nil {
		return statev1.Entry{}, "", fmt.Errorf("state: decode entry: %w", err)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		etag = entry.ETag()
	}
	return entry, etag, nil
}

// Create creates a key only if it does not exist.
func (c *Client) Create(ctx context.Context, key string, req statev1.WriteRequest) (statev1.Entry, string, error) {
	if err := req.Validate(); err != nil {
		return statev1.Entry{}, "", err
	}
	raw, _ := json.Marshal(req)
	httpReq, err := c.requestURL(ctx, http.MethodPut, url.Values{"key": {key}}, bytes.NewReader(raw))
	if err != nil {
		return statev1.Entry{}, "", err
	}
	httpReq.Header.Set("If-None-Match", "*")
	return c.write(httpReq)
}

// Update replaces a key under its observed ETag.
func (c *Client) Update(ctx context.Context, key, etag string, req statev1.WriteRequest) (statev1.Entry, string, error) {
	if etag == "" {
		return statev1.Entry{}, "", fmt.Errorf("state: an ETag is required for update")
	}
	if err := req.Validate(); err != nil {
		return statev1.Entry{}, "", err
	}
	raw, _ := json.Marshal(req)
	httpReq, err := c.request(ctx, http.MethodPut, key, etag, bytes.NewReader(raw))
	if err != nil {
		return statev1.Entry{}, "", err
	}
	return c.write(httpReq)
}

func (c *Client) write(req *http.Request) (statev1.Entry, string, error) {
	resp, err := c.do(req)
	if err != nil {
		return statev1.Entry{}, "", err
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return statev1.Entry{}, "", err
	}
	var entry statev1.Entry
	if err := json.NewDecoder(io.LimitReader(resp.Body, statev1.MaxValueBytes+16<<10)).Decode(&entry); err != nil {
		return statev1.Entry{}, "", fmt.Errorf("state: decode entry: %w", err)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		etag = entry.ETag()
	}
	return entry, etag, nil
}

// Delete removes a key under its observed ETag.
func (c *Client) Delete(ctx context.Context, key, etag string) error {
	if etag == "" {
		return fmt.Errorf("state: an ETag is required for delete")
	}
	req, err := c.request(ctx, http.MethodDelete, key, etag, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return responseError(resp)
}

// List lists entries under a prefix.
func (c *Client) List(ctx context.Context, prefix, cursor string, limit int) ([]statev1.Entry, string, error) {
	if limit <= 0 {
		limit = statev1.DefaultListLimit
	}
	q := url.Values{"prefix": {prefix}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	req, err := c.requestURL(ctx, http.MethodGet, q, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return nil, "", err
	}
	var out struct {
		Items      []statev1.Entry `json:"items"`
		NextCursor string          `json:"nextCursor"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("state: decode list: %w", err)
	}
	return out.Items, out.NextCursor, nil
}

func (c *Client) request(ctx context.Context, method, key, ifMatch string, body io.Reader) (*http.Request, error) {
	return c.requestURL(ctx, method, url.Values{"key": {key}}, body, ifMatch)
}

func (c *Client) requestURL(ctx context.Context, method string, q url.Values, body io.Reader, ifMatch ...string) (*http.Request, error) {
	target := fmt.Sprintf("%s/runtime/v3/attempts/%s/state/%s/entries?%s",
		c.BaseURL, url.PathEscape(c.Attempt), url.PathEscape(c.Binding), q.Encode())
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if len(ifMatch) > 0 && ifMatch[0] != "" {
		req.Header.Set("If-Match", ifMatch[0])
	}
	if c.TokenFile != "" {
		token, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("state: read token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("state: request: %w", err)
	}
	return resp, nil
}

func responseError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	if body.Error.Code == "" {
		body.Error.Code = "http_error"
	}
	return &HTTPError{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
}
