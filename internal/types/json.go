package types

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
)

// JSON is a custom type that wraps json.RawMessage.
// Used for storing JSON data in the database.
type JSON json.RawMessage

// Scan implements the sql.Scanner interface.
func (j *JSON) Scan(value interface{}) error {
	if value == nil {
		*j = nil
		return nil
	}

	var bytes []byte
	switch v := value.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return errors.New("type assertion to []byte or string failed")
	}

	if len(bytes) == 0 {
		*j = nil
		return nil
	}

	result := json.RawMessage{}
	err := json.Unmarshal(bytes, &result)
	*j = JSON(result)
	return err
}

// Value implements the driver.Valuer interface.
func (j JSON) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return json.RawMessage(j).MarshalJSON()
}

// MarshalJSON implements the json.Marshaler interface.
func (j JSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (j *JSON) UnmarshalJSON(data []byte) error {
	if j == nil {
		return errors.New("JSON: UnmarshalJSON on nil pointer")
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	*j = JSON(copied)
	return nil
}

// NewJSON 把任意值序列化为 JSON 载荷（审计日志 Details 等场景）；
// 序列化失败返回 nil，调用方按"无明细"处理。
func NewJSON(v any) JSON {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return JSON(b)
}

// ToString converts JSON to a string.
func (j JSON) ToString() string {
	if len(j) == 0 {
		return "{}"
	}
	return string(j)
}

// Map converts JSON to a map.
func (j JSON) Map() (map[string]interface{}, error) {
	if len(j) == 0 {
		return map[string]interface{}{}, nil
	}

	var m map[string]interface{}
	err := json.Unmarshal(j, &m)
	return m, err
}
