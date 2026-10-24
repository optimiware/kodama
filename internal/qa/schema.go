package qa

import _ "embed"

// Schema is the JSON Schema (draft 2020-12) of the report contract. It is the
// document QA step authors and the T-069 profile hand to the agent; the Go
// decoder is the authority and the tests require both to agree on the fixtures.
//
//go:embed schema.json
var Schema []byte
