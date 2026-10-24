package gatewayhttp

// The generator is pinned by version and invoked without being added to go.mod
// (see internal/httpapi/generate.go): `go run pkg@version` resolves it in its
// own module graph. Verify reproducibility with:
//
//	go generate ./internal/gateway/... && git diff --exit-code -- internal/gateway/http/gateway.gen.go
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../../../api/gateway.yaml
