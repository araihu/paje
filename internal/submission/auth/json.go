package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

var rawMessageType = reflect.TypeOf(json.RawMessage{})

// ValidateExactJSON rejects duplicate object names and requires every typed
// object field to use its exact declared JSON name. This closes encoding/json's
// case-insensitive struct-field fallback before decoding can overwrite data.
func ValidateExactJSON(raw []byte, destination any) error {
	typeOf := reflect.TypeOf(destination)
	if typeOf == nil {
		return errors.New("JSON destination is required")
	}
	for typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := visitExactJSONValue(decoder, typeOf); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func visitExactJSONValue(decoder *json.Decoder, typeOf reflect.Type) error {
	for typeOf != nil && typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	if typeOf == rawMessageType || typeOf != nil && typeOf.Kind() == reflect.Interface {
		typeOf = nil
	}
	switch delimiter {
	case '{':
		fields, typed := exactJSONFields(typeOf)
		var element reflect.Type
		if typeOf != nil && typeOf.Kind() == reflect.Map {
			element = typeOf.Elem()
		}
		seen := make(map[string]bool)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok || seen[name] {
				return errors.New("invalid or duplicate object name")
			}
			seen[name] = true
			fieldType := element
			if typed {
				var exists bool
				fieldType, exists = fields[name]
				if !exists {
					return errors.New("object name does not exactly match the JSON schema")
				}
			}
			if err := visitExactJSONValue(decoder, fieldType); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("object is not closed")
		}
	case '[':
		var element reflect.Type
		if typeOf != nil && (typeOf.Kind() == reflect.Slice || typeOf.Kind() == reflect.Array) {
			element = typeOf.Elem()
		}
		for decoder.More() {
			if err := visitExactJSONValue(decoder, element); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("array is not closed")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func exactJSONFields(typeOf reflect.Type) (map[string]reflect.Type, bool) {
	for typeOf != nil && typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	if typeOf == nil || typeOf.Kind() != reflect.Struct {
		return nil, false
	}
	fields := make(map[string]reflect.Type)
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		tagName := strings.Split(tag, ",")[0]
		if tagName == "-" {
			continue
		}
		if field.Anonymous && tagName == "" {
			embedded, ok := exactJSONFields(field.Type)
			if ok {
				for embeddedName, embeddedType := range embedded {
					fields[embeddedName] = embeddedType
				}
			}
			continue
		}
		name := field.Name
		if tagName != "" {
			name = tagName
		}
		fields[name] = field.Type
	}
	return fields, true
}
