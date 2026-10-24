package identityhttp

// The generator is pinned by version and invoked without being added to go.mod,
// exactly as in internal/httpapi/generate.go. Verify reproducibility with:
//
//	go generate ./internal/identity/... && git diff --exit-code -- internal/identity/http/identity.gen.go
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../../../api/identity.yaml
