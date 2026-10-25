package devenv

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func stringReader(value string) *strings.Reader { return strings.NewReader(value) }

func readStringMap(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return values, nil
}
