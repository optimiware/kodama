package deliveryhttp

// The generator is pinned by version and invoked without being added to go.mod
// (see internal/httpapi/generate.go): `go run pkg@version` resolves it in its
// own module graph. Verify reproducibility with:
//
//	go generate ./internal/delivery/http && git diff --exit-code -- internal/delivery/http/delivery.gen.go
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../../../api/delivery.yaml
