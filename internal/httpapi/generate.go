package httpapi

// The generator is pinned by version and invoked without being added to go.mod:
// `go run pkg@version` resolves it in its own module graph, so neither it nor
// its transitive modules enter the repository's dependency files. Changing the
// version is a reviewed change to this line and a regeneration of
// common.gen.go. Verify reproducibility with:
//
//	go generate ./internal/httpapi/... && git diff --exit-code -- internal/httpapi/common.gen.go
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../../api/common.yaml
