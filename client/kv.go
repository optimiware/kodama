package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
)

// KVObject is one retrieved project-scoped immutable KV object with decoded
// raw bytes and server-verified metadata.
type KVObject struct {
	Key         string    `json:"key"`
	ProjectID   string    `json:"project_id"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentType string    `json:"content_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	Data        []byte    `json:"-"`
	Raw         api.KVGetResult
}

// KVPut stores bounded immutable bytes (up to 16 MiB) in a project's
// content-addressed KV store and returns the server-derived metadata and key.
func KVPut(ctx context.Context, transport Transport, projectID string, data []byte, contentType string) (api.KVPutResult, error) {
	if transport == nil {
		return api.KVPutResult{}, fault.New(fault.InvalidInput, "kv put requires a client transport")
	}
	if len(data) > api.MaxKVValueBytes {
		return api.KVPutResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("kv value is %d bytes; at most %d are allowed", len(data), api.MaxKVValueBytes))
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	req := api.KVPutRequest{
		ProjectID:   projectID,
		DataBase64:  &encoded,
		ContentType: contentType,
	}
	if err := req.Validate(); err != nil {
		return api.KVPutResult{}, fault.New(fault.InvalidInput, publicMessage(err))
	}
	rawInput, err := json.Marshal(req)
	if err != nil {
		return api.KVPutResult{}, fault.New(fault.InvalidInput, "kv.put request is not encodable")
	}
	res, err := transport.Invoke(ctx, api.Invocation{
		Operation: "kv.put",
		Request:   api.Request{Input: rawInput},
	})
	if err != nil {
		return api.KVPutResult{}, err
	}
	var out api.KVPutResult
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return api.KVPutResult{}, fault.New(fault.InternalFailure, "kv.put response is unreadable")
	}
	if err := out.Validate(); err != nil {
		return api.KVPutResult{}, fault.New(fault.InternalFailure, "kv.put response is invalid")
	}
	return out, nil
}

// KVGet fetches an immutable KV object by its canonical key in projectID and
// decodes its exact bytes.
func KVGet(ctx context.Context, transport Transport, projectID, key string) (KVObject, error) {
	if transport == nil {
		return KVObject{}, fault.New(fault.InvalidInput, "kv get requires a client transport")
	}
	req := api.KVGetRequest{
		ProjectID: projectID,
		Key:       key,
	}
	if err := req.Validate(); err != nil {
		return KVObject{}, fault.New(fault.InvalidInput, publicMessage(err))
	}
	rawInput, err := json.Marshal(req)
	if err != nil {
		return KVObject{}, fault.New(fault.InvalidInput, "kv.get request is not encodable")
	}
	res, err := transport.Invoke(ctx, api.Invocation{
		Operation: "kv.get",
		Request:   api.Request{Input: rawInput},
	})
	if err != nil {
		return KVObject{}, err
	}
	var out api.KVGetResult
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return KVObject{}, fault.New(fault.InternalFailure, "kv.get response is unreadable")
	}
	if err := out.Validate(); err != nil {
		return KVObject{}, fault.New(fault.InternalFailure, "kv.get response is invalid")
	}
	decoded, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		return KVObject{}, fault.New(fault.InternalFailure, "kv.get payload is not valid base64")
	}
	return KVObject{
		Key:         out.Key,
		ProjectID:   out.ProjectID,
		SizeBytes:   out.SizeBytes,
		ContentType: out.ContentType,
		CreatedBy:   out.CreatedBy,
		CreatedAt:   out.CreatedAt,
		Data:        decoded,
		Raw:         out,
	}, nil
}

// KVPut stores bounded immutable bytes in a project's content-addressed KV store.
func (transport *HTTPTransport) KVPut(ctx context.Context, projectID string, data []byte, contentType string) (api.KVPutResult, error) {
	return KVPut(ctx, transport, projectID, data, contentType)
}

// KVGet retrieves an immutable KV object by key from a project's KV store.
func (transport *HTTPTransport) KVGet(ctx context.Context, projectID, key string) (KVObject, error) {
	return KVGet(ctx, transport, projectID, key)
}
