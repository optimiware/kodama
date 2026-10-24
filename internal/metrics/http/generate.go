// Package metricshttp holds the wire types of the flow-summary contribution and
// their mapping from metrics.WorkSummary. It registers no route: the integration
// task composes the contribution into the existing work list.
package metricshttp

// The generator is pinned by version and invoked without being added to go.mod,
// exactly as in internal/work/http/generate.go. Verify reproducibility with:
//
//	go generate ./internal/metrics/... && git diff --exit-code -- internal/metrics/http/work-summaries.gen.go
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../../../api/work-summaries.yaml
