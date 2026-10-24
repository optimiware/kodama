package http

// The generator is pinned by version and invoked without being added to go.mod
// (`go run pkg@version` resolves it in its own module graph), exactly as for
// internal/httpapi. Verify reproducibility with:
//
//	go generate ./internal/workspace/http/... && git diff --exit-code -- internal/workspace/http/types.gen.go
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../../../api/workspace.yaml
