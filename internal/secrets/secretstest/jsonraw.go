package secretstest

import "encoding/json"

func jsonRaw(v any) (json.RawMessage, error) { return json.Marshal(v) }
