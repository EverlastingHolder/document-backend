// Package jsonstrict contains small validation helpers for JSON boundaries.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// RejectDuplicateKeys checks one complete JSON value, including nested objects
// and objects inside arrays. Root shape and schema validation remain with the
// caller.
func RejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON data")
		}
		return err
	}
	return nil
}

func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isContainer := token.(json.Delim)
	if !isContainer {
		return nil
	}
	switch delimiter {
	case '{':
		return scanObject(decoder)
	case '[':
		return scanArray(decoder)
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func scanObject(decoder *json.Decoder) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("invalid JSON object key")
		}
		if _, exists := seen[key]; exists {
			return errors.New("duplicate JSON object key")
		}
		seen[key] = struct{}{}
		if err := scanValue(decoder); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return errors.New("invalid JSON object")
	}
	return nil
}

func scanArray(decoder *json.Decoder) error {
	for decoder.More() {
		if err := scanValue(decoder); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim(']') {
		return errors.New("invalid JSON array")
	}
	return nil
}
