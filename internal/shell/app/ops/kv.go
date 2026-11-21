package opsapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"unicode/utf8"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/shell/kv"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
)

// kvHandlers binds the project-scoped, content-addressed immutable KV
// operations (kv.put and kv.get).
func kvHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"kv.put": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.KVPutRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			data, isText, err := request.DecodeBytes()
			if err != nil {
				return api.Result{}, wireRefusal(err)
			}
			contentType := request.ContentType
			if contentType == "" && isText {
				contentType = kv.DefaultTextContentType
			}
			result, err := deps.KV.Put(ctx, kv.PutCommand{
				Scope:       call.Scope,
				Actor:       call.Principal,
				Data:        data,
				ContentType: contentType,
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.KVPutResult{
				Key:         string(result.Metadata.Key),
				ProjectID:   string(result.Metadata.Scope.ProjectID),
				SizeBytes:   result.Metadata.SizeBytes,
				ContentType: result.Metadata.ContentType,
				CreatedAt:   result.Metadata.CreatedAt,
				Existed:     result.Existed,
			})
		}),
		"kv.get": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.KVGetRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			obj, err := deps.KV.Get(ctx, kv.GetCommand{
				Scope: call.Scope,
				Actor: call.Principal,
				Key:   kv.Key(request.Key),
			})
			if err != nil {
				return api.Result{}, err
			}
			res := api.KVGetResult{
				Key:         string(obj.Metadata.Key),
				ProjectID:   string(obj.Metadata.Scope.ProjectID),
				SizeBytes:   obj.Metadata.SizeBytes,
				ContentType: obj.Metadata.ContentType,
				CreatedBy:   string(obj.Metadata.CreatedBy),
				CreatedAt:   obj.Metadata.CreatedAt,
				DataBase64:  base64.StdEncoding.EncodeToString(obj.Data),
			}
			if len(obj.Data) <= api.MaxKVInlineTextBytes && utf8.Valid(obj.Data) && bytes.IndexByte(obj.Data, 0) < 0 {
				text := string(obj.Data)
				res.Text = &text
			}
			return respond(call, res)
		}),
	}
}
