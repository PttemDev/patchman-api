package apm

import (
	"encoding/json"
	"fmt"
)

// decodeList accepts a bare JSON array or one wrapped in {data: [...]}.
func decodeList[T any](raw []byte) ([]T, error) {
	var list []T
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Data []T `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("unexpected answer")
	}
	return wrapped.Data, nil
}
