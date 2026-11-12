package readers

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
)

// rowString reads one required text column.
func rowString(row store.Row, name string) (string, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not text", row.Table, name))
	}
	return text, nil
}

// rowNullableString reads one nullable text column.
func rowNullableString(row store.Row, name string) (string, error) {
	value, ok := row.Values[name]
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not text", row.Table, name))
	}
	return text, nil
}

// rowBytes reads one bytea column. A NULL column reads as nil.
func rowBytes(row store.Row, name string) ([]byte, error) {
	value, ok := row.Values[name]
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return nil, nil
	}
	data, ok := value.([]byte)
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not bytes", row.Table, name))
	}
	return append([]byte(nil), data...), nil
}

// rowUint reads one integer column as an unsigned value.
func rowUint(row store.Row, name string) (uint64, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	switch typed := value.(type) {
	case int64:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is negative", row.Table, name))
		}
		return uint64(typed), nil
	case int:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is negative", row.Table, name))
		}
		return uint64(typed), nil
	default:
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not an integer", row.Table, name))
	}
}

// rowTime reads one required timestamptz column.
func rowTime(row store.Row, name string) (time.Time, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return time.Time{}, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	moment, ok := value.(time.Time)
	if !ok {
		return time.Time{}, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not a timestamp", row.Table, name))
	}
	return moment, nil
}

// rowNullableTime reads one nullable timestamptz column.
func rowNullableTime(row store.Row, name string) (*time.Time, error) {
	value, ok := row.Values[name]
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return nil, nil
	}
	moment, ok := value.(time.Time)
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not a timestamp", row.Table, name))
	}
	return &moment, nil
}

// decodeJSON decodes one required structured column value into target. A nil or
// empty value is stored corruption, never a silently empty value.
func decodeJSON(data []byte, target any) error {
	if len(data) == 0 {
		return fault.New(fault.InternalFailure, "a required structured column value is empty")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fault.Wrap(fault.InternalFailure, "a stored structured column value is not valid", err)
	}
	return nil
}

// decodeStringList decodes one required JSON string-array column.
func decodeStringList(data []byte) ([]string, error) {
	var values []string
	if err := decodeJSON(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// contentHash renders one durable receipt fingerprint from its bytea column.
func contentHash(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fault.New(fault.InternalFailure, "durable receipt has no content hash")
	}
	return "sha256:" + hex.EncodeToString(data), nil
}

// rowBool reads one required boolean column.
func rowBool(row store.Row, name string) (bool, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return false, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not a boolean", row.Table, name))
	}
	return boolean, nil
}
