package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"passport-backend/internal/jsonstrict"
)

var storedPublicFieldNames = [...]string{
	"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
	"series", "number", "issue_date", "issuer", "department_code",
}

var storedIssueMessages = map[string]string{
	"conflicting_field_candidates":    "Есть несколько вариантов значения. Проверьте поле.",
	"manual_review_required":          "Проверьте распознанное значение.",
	"date_invalid_calendar_or_format": "Не удалось получить допустимую дату.",
}

const storedNoFieldsWarning = "Не удалось распознать поля. Попробуйте сделать новый снимок."

// DecodeStoredRecognitionResponse validates a persisted public response and
// returns its original JSON bytes. It does not remap extraction data or
// normalize the stored snapshot.
func DecodeStoredRecognitionResponse(data []byte, jobID, pipelineVersion string) (json.RawMessage, error) {
	if len(data) == 0 || !requestIDPattern.MatchString(jobID) || strings.TrimSpace(pipelineVersion) == "" {
		return nil, errors.New("invalid stored recognition response")
	}
	if err := jsonstrict.RejectDuplicateKeys(data); err != nil {
		return nil, errors.New("invalid stored recognition response")
	}
	if err := requireExactKeys(data, []string{"request_id", "status", "review_required", "pipeline_version", "fields", "warnings"}); err != nil {
		return nil, errors.New("invalid stored recognition response")
	}
	if err := validateStoredObjectMembers(data); err != nil {
		return nil, errors.New("invalid stored recognition response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var response RecognitionResponse
	if err := decoder.Decode(&response); err != nil {
		return nil, errors.New("invalid stored recognition response")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid stored recognition response")
	}
	if !strings.EqualFold(response.RequestID, jobID) || response.Status != "completed" || !response.ReviewRequired || response.PipelineVersion != pipelineVersion || len(response.Fields) != len(storedPublicFieldNames) || response.Warnings == nil {
		return nil, errors.New("invalid stored recognition response")
	}
	for _, name := range storedPublicFieldNames {
		field, ok := response.Fields[name]
		if !ok || !validStoredField(name, field) {
			return nil, errors.New("invalid stored recognition response")
		}
	}
	for name := range response.Fields {
		if !containsStoredField(name) {
			return nil, errors.New("invalid stored recognition response")
		}
	}
	allValuesNull := true
	for _, field := range response.Fields {
		if field.Value != nil {
			allValuesNull = false
		}
	}
	if allValuesNull {
		if len(response.Warnings) != 1 || response.Warnings[0] != (RecognitionWarning{Code: "no_fields_recognized", Message: storedNoFieldsWarning}) {
			return nil, errors.New("invalid stored recognition response")
		}
	} else if len(response.Warnings) != 0 {
		return nil, errors.New("invalid stored recognition response")
	}
	return append(json.RawMessage(nil), data...), nil
}

func requireExactKeys(data []byte, want []string) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil || values == nil || len(values) != len(want) {
		return errors.New("invalid object keys")
	}
	for _, key := range want {
		if _, ok := values[key]; !ok {
			return fmt.Errorf("missing object key %q", key)
		}
	}
	return nil
}

func validateStoredObjectMembers(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(root["fields"], &fields); err != nil || fields == nil || len(fields) != len(storedPublicFieldNames) {
		return errors.New("invalid stored fields")
	}
	for _, name := range storedPublicFieldNames {
		field, exists := fields[name]
		if !exists || requireExactKeys(field, []string{"value", "state", "issues"}) != nil {
			return errors.New("invalid stored field")
		}
		var typedField map[string]json.RawMessage
		if err := json.Unmarshal(field, &typedField); err != nil {
			return err
		}
		var issues []json.RawMessage
		if err := json.Unmarshal(typedField["issues"], &issues); err != nil || issues == nil {
			return errors.New("invalid stored issues")
		}
		for _, issue := range issues {
			if err := requireExactKeys(issue, []string{"code", "message"}); err != nil {
				return err
			}
		}
	}
	var warnings []json.RawMessage
	if err := json.Unmarshal(root["warnings"], &warnings); err != nil || warnings == nil {
		return errors.New("invalid stored warnings")
	}
	for _, warning := range warnings {
		if err := requireExactKeys(warning, []string{"code", "message"}); err != nil {
			return err
		}
	}
	return nil
}

func validStoredField(name string, field PublicFieldResult) bool {
	if field.Issues == nil {
		return false
	}
	seen := make(map[string]struct{}, len(field.Issues))
	for _, issue := range field.Issues {
		message, ok := storedIssueMessages[issue.Code]
		if !ok || issue.Message != message {
			return false
		}
		if _, exists := seen[issue.Code]; exists {
			return false
		}
		seen[issue.Code] = struct{}{}
	}
	switch field.State {
	case "missing":
		if field.Value != nil {
			return false
		}
	case "recognized":
		if field.Value == nil || len(field.Issues) != 0 {
			return false
		}
	case "uncertain":
		if len(field.Issues) == 0 {
			return false
		}
	default:
		return false
	}
	return field.Value == nil || validPublicValue(name, *field.Value)
}

func containsStoredField(name string) bool {
	for _, candidate := range storedPublicFieldNames {
		if name == candidate {
			return true
		}
	}
	return false
}
