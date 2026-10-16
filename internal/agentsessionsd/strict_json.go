package agentsessionsd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"unicode/utf8"
)

// strictJSON checks the ambiguities encoding/json otherwise accepts at this operator/peer
// boundary: duplicate keys, case-folded fields, scalar nulls and replacement of invalid UTF-8.
// Dynamic tool output objects still allow arbitrary keys, but never duplicate members.
func strictJSON(data []byte, dst any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := checkJSONValue(d, reflect.TypeOf(dst).Elem(), 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		// UnmarshalTypeError can include a peer-controlled numeric value. These errors may
		// become durable tool receipts, so retain the category but never the payload.
		return errors.New("JSON value has the wrong type or is out of range")
	}
	return nil
}

func checkJSONValue(d *json.Decoder, typ reflect.Type, depth int) error {
	if depth > 100 {
		return errors.New("JSON nesting exceeds 100")
	}
	token, err := d.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	if token == nil {
		if typ == nil || typ.Kind() == reflect.Map || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Interface || (typ.Kind() == reflect.Pointer && typ.Elem().Kind() == reflect.Bool) {
			return nil
		}
		return errors.New("null is not allowed for this field")
	}
	if typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ != nil && typ.Kind() == reflect.Interface {
		typ = nil
	}
	switch token {
	case json.Delim('{'):
		if typ != nil && typ.Kind() != reflect.Struct && typ.Kind() != reflect.Map {
			return errors.New("unexpected JSON object")
		}
		seen := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return errors.New("invalid JSON member")
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errors.New("duplicate or invalid JSON member")
			}
			seen[key] = true
			var fieldType reflect.Type
			if typ != nil {
				if typ.Kind() == reflect.Map {
					fieldType = typ.Elem()
				} else {
					for i := 0; i < typ.NumField(); i++ {
						field := typ.Field(i)
						if field.Tag.Get("json") == key {
							fieldType = field.Type
							break
						}
					}
					if fieldType == nil {
						return errors.New("unknown JSON field")
					}
				}
			}
			if err := checkJSONValue(d, fieldType, depth+1); err != nil {
				return err
			}
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case json.Delim('['):
		if typ != nil && typ.Kind() != reflect.Slice {
			return errors.New("unexpected JSON array")
		}
		var elem reflect.Type
		if typ != nil {
			elem = typ.Elem()
		}
		for d.More() {
			if err := checkJSONValue(d, elem, depth+1); err != nil {
				return err
			}
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		if typ != nil {
			switch typ.Kind() {
			case reflect.Struct, reflect.Map, reflect.Slice:
				return errors.New("expected JSON object or array")
			}
		}
	}
	return nil
}
