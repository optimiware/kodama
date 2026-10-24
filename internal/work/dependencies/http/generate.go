package dependencieshttp

// The generator is pinned by version and invoked without being added to go.mod,
// exactly as in internal/work/http/generate.go. Verify reproducibility with:
//
//	go generate ./internal/work/dependencies/... && git diff --exit-code -- internal/work/dependencies/http/dependencies.gen.go
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../../../../api/work-dependencies.yaml
