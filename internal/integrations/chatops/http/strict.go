package chatopshttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/optimiware/kodama/internal/httpapi"
)

var errStrict = errors.New("chatopshttp: body is not a strict JSON document")

// decodeStrict decodes exactly one JSON object into dst. Beyond the shared
// foundation's rules (unknown fields, trailing data, empty body) it rejects a
// key repeated at any depth, which encoding/json would silently resolve to the
// last value, and an explicit null anywhere: no field of this module's request
// schemas is nullable, and a null for an optional member must never read as
// "omitted". Failures are 400 invalid_request and never echo the body.
func decodeStrict(body []byte, dst any) error {
	if err := checkTree(body); err != nil {
		return httpapi.BadRequestError("the request body is not valid for this operation")
	}
	return httpapi.DecodeJSONBytes(body, dst)
}

func checkTree(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if tok == nil {
			return errStrict
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := k.(string)
				if seen[key] {
					return errStrict
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token()
		return err
	}
	if err := walk(); err != nil {
		if errors.Is(err, io.EOF) {
			return errStrict
		}
		return err
	}
	return nil
}
