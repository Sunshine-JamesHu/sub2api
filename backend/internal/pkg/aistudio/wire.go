package aistudio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

var ErrAuthenticationRequired = errors.New("AI Studio session requires authentication")

type UnverifiedProtocolError struct{ Feature string }

func (e *UnverifiedProtocolError) Error() string {
	return "unsupported AI Studio protocol: " + e.Feature
}

// RPCError retains the canonical Google RPC code, including errors in HTTP 200 streams.
type RPCError struct {
	Method     string
	StatusCode int
	Code       int
	Message    string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("AI Studio %s HTTP %d RPC %d: %s", e.Method, e.StatusCode, e.Code, e.Message)
}
func (e *RPCError) Is(target error) bool {
	return target == ErrAuthenticationRequired && !e.DriveAuthorizationMissing() &&
		(e.Code == 16 || e.StatusCode == http.StatusUnauthorized)
}

// DriveAuthorizationMissing is specific to the page's optional Drive RPCs.
// It does not invalidate the Google web session used for text generation.
func (e *RPCError) DriveAuthorizationMissing() bool {
	switch e.Method {
	case "GenerateAccessToken", "ListRecentApplets", "ListPrompts":
		return e.StatusCode == http.StatusUnauthorized && e.Code == 16 &&
			strings.Contains(strings.ToLower(e.Message), "unauthorized_client")
	default:
		return false
	}
}

func (e *RPCError) RegionNotSupported() bool {
	return e.StatusCode == http.StatusForbidden && e.Code == 7 &&
		strings.Contains(strings.ToLower(e.Message), "region not supported")
}

// CheckRPCResponse preserves Google's direct or wrapped status array. Only
// ListModels can identify a status array in HTTP 200 unambiguously; other unary
// methods can legitimately begin with an integer and string (e.g. logging).
func CheckRPCResponse(method string, status int, raw []byte) error {
	if status == http.StatusOK && method != "ListModels" {
		return nil
	}
	var root []json.RawMessage
	_ = json.NewDecoder(newSparseJSONReader(bytes.NewReader(raw))).Decode(&root)
	provider := root
	if len(root) > 1 && bytes.Equal(bytes.TrimSpace(root[0]), []byte("null")) {
		var wrapped []json.RawMessage
		if json.Unmarshal(root[1], &wrapped) == nil {
			provider = wrapped
		}
	}
	code, message := 0, ""
	valid := len(provider) > 1 && json.Unmarshal(provider[0], &code) == nil &&
		json.Unmarshal(provider[1], &message) == nil && code > 0
	if status == http.StatusOK && !valid {
		return nil
	}
	if !valid {
		message = http.StatusText(status)
	}
	return &RPCError{Method: method, StatusCode: status, Code: code, Message: message}
}

func encodeWireStruct(object map[string]any) []any {
	if len(object) == 0 {
		return []any{}
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]any, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, []any{key, encodeWireValue(object[key])})
	}
	return []any{entries}
}
func encodeWireValue(value any) []any {
	switch v := value.(type) {
	case nil:
		return []any{int64(0)}
	case float64:
		return []any{nil, v}
	case json.Number:
		return []any{nil, v}
	case string:
		return []any{nil, nil, v}
	case bool:
		return []any{nil, nil, nil, v}
	case map[string]any:
		return []any{nil, nil, nil, nil, encodeWireStruct(v)}
	case []any:
		values := make([]any, 0, len(v))
		for _, item := range v {
			values = append(values, encodeWireValue(item))
		}
		return []any{nil, nil, nil, nil, nil, []any{values}}
	}
	panic(fmt.Sprintf("non-JSON protobuf Value %T", value))
}
func encodeWireObject(value any) ([]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err = decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("protobuf Struct requires an object: %w", err)
	}
	return encodeWireStruct(object), nil
}
func decodeWireStruct(raw json.RawMessage) (map[string]any, error) {
	var fields []json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	object := map[string]any{}
	if len(fields) == 0 {
		return object, nil
	}
	var entries [][]json.RawMessage
	if err := json.Unmarshal(fields[0], &entries); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if len(entry) != 2 {
			return nil, errors.New("invalid protobuf Struct entry")
		}
		var key string
		if err := json.Unmarshal(entry[0], &key); err != nil {
			return nil, err
		}
		value, err := decodeWireValue(entry[1])
		if err != nil {
			return nil, err
		}
		object[key] = value
	}
	return object, nil
}
func decodeWireValue(raw json.RawMessage) (any, error) {
	var fields []json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for index, field := range fields {
		if string(field) == "null" {
			continue
		}
		switch index {
		case 0:
			return nil, nil
		case 1:
			var v json.Number
			err := json.Unmarshal(field, &v)
			return v, err
		case 2:
			var v string
			err := json.Unmarshal(field, &v)
			return v, err
		case 3:
			var v bool
			err := json.Unmarshal(field, &v)
			return v, err
		case 4:
			return decodeWireStruct(field)
		case 5:
			var list []json.RawMessage
			if err := json.Unmarshal(field, &list); err != nil {
				return nil, err
			}
			values := []any{}
			if len(list) > 0 {
				var items []json.RawMessage
				if err := json.Unmarshal(list[0], &items); err != nil {
					return nil, err
				}
				for _, item := range items {
					v, err := decodeWireValue(item)
					if err != nil {
						return nil, err
					}
					values = append(values, v)
				}
			}
			return values, nil
		}
	}
	return nil, errors.New("protobuf Value has no variant")
}

// ParseModels reads ListModels field 1 (a repeated list of model records).
func ParseModels(raw []byte) ([]string, error) {
	if err := CheckRPCResponse("ListModels", http.StatusOK, raw); err != nil {
		return nil, err
	}
	var root []json.RawMessage
	if err := json.NewDecoder(newSparseJSONReader(bytes.NewReader(raw))).Decode(&root); err != nil {
		return nil, err
	}
	if len(root) == 0 {
		return nil, errors.New("ListModels has no model list")
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(root[0], &rows); err != nil {
		return nil, err
	}
	models := []string{}
	for _, row := range rows {
		var model string
		if len(row) == 0 {
			return nil, errors.New("empty ListModels record")
		}
		if err := json.Unmarshal(row[0], &model); err != nil {
			return nil, err
		}
		if model != "" {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("ListModels returned no models")
	}
	return models, nil
}
