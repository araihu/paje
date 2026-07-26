package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode"

	"github.com/araihu/paje/internal/submission/auth"
)

const (
	maxBodyBytes          = 1 << 20
	maxResponseBytes      = 1 << 20
	maxAuthorizationBytes = 512
	maxContentTypeBytes   = 128
	minIdempotencyBytes   = 16
	maxIdempotencyBytes   = 128
	maxPathIDBytes        = 128
)

func requireJSON(request *http.Request) error {
	values := request.Header.Values("Content-Type")
	if len(values) != 1 || len(values[0]) > maxContentTypeBytes {
		return errUnsupportedContentType
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return errUnsupportedContentType
	}
	if charset, ok := parameters["charset"]; ok && !strings.EqualFold(charset, "utf-8") {
		return errUnsupportedContentType
	}
	return nil
}

func requireIdempotencyKey(request *http.Request) (string, error) {
	values := request.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		return "", errInvalidRequest
	}
	value := values[0]
	if len(value) > maxIdempotencyBytes {
		return "", errHeaderTooLarge
	}
	if len(value) < minIdempotencyBytes || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\r\n\x00") {
		return "", errInvalidRequest
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", errInvalidRequest
		}
	}
	return value, nil
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return errInvalidRequest
	}
	if err := auth.ValidateExactJSON(raw, destination); err != nil {
		return errInvalidRequest
	}
	decoder := jsonDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return errInvalidRequest
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return errInvalidRequest
	}
	return nil
}

func validateUniqueJSONNames(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := visitJSONValue(decoder); err != nil {
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

func visitJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		names := make(map[string]bool)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok || names[name] {
				return errors.New("invalid or duplicate object name")
			}
			names[name] = true
			if err := visitJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := visitJSONValue(decoder); err != nil {
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
