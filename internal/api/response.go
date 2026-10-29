package api

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"passport-backend/internal/passport"
)

var (
	requestIDPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	seriesPattern     = regexp.MustCompile(`^[0-9]{4}$`)
	numberPattern     = regexp.MustCompile(`^[0-9]{6}$`)
	departmentPattern = regexp.MustCompile(`^[0-9]{3}-[0-9]{3}$`)
)

var publicFieldNames = []string{
	"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
	"series", "number", "issue_date", "issuer", "department_code",
}

type RecognitionResponse struct {
	RequestID       string                       `json:"request_id"`
	Status          string                       `json:"status"`
	ReviewRequired  bool                         `json:"review_required"`
	PipelineVersion string                       `json:"pipeline_version"`
	Fields          map[string]PublicFieldResult `json:"fields"`
	Warnings        []RecognitionWarning         `json:"warnings"`
}

type PublicFieldResult struct {
	Value  *string       `json:"value"`
	State  string        `json:"state"`
	Issues []PublicIssue `json:"issues"`
}

type PublicIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RecognitionWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MapExtraction exposes only the selected public response fields and safe issue messages.
func MapExtraction(extraction passport.Extraction, requestID, pipelineVersion string) (RecognitionResponse, error) {
	if !requestIDPattern.MatchString(requestID) || strings.TrimSpace(pipelineVersion) == "" {
		return RecognitionResponse{}, errors.New("invalid recognition response metadata")
	}

	response := RecognitionResponse{
		RequestID:       requestID,
		Status:          "completed",
		ReviewRequired:  true,
		PipelineVersion: pipelineVersion,
		Fields:          make(map[string]PublicFieldResult, len(publicFieldNames)),
		Warnings:        []RecognitionWarning{},
	}
	allValuesNull := true
	for _, name := range publicFieldNames {
		internal, exists := extraction.Fields[name]
		if !exists {
			internal = passport.FieldResult{State: "missing"}
		}
		field, err := mapField(name, internal)
		if err != nil {
			return RecognitionResponse{}, err
		}
		if field.Value != nil {
			allValuesNull = false
		}
		response.Fields[name] = field
	}
	if allValuesNull {
		response.Warnings = append(response.Warnings, RecognitionWarning{
			Code:    "no_fields_recognized",
			Message: "Не удалось распознать поля. Попробуйте сделать новый снимок.",
		})
	}
	return response, nil
}

func mapField(name string, internal passport.FieldResult) (PublicFieldResult, error) {
	field := PublicFieldResult{State: internal.State, Issues: mapIssues(internal.Issues)}
	switch internal.State {
	case "missing":
		if internal.Value != nil {
			return PublicFieldResult{}, errors.New("invalid extraction field state")
		}
	case "recognized":
		if internal.Value == nil {
			return PublicFieldResult{}, errors.New("invalid extraction field state")
		}
	case "uncertain":
	default:
		return PublicFieldResult{}, errors.New("invalid extraction field state")
	}

	conflict := hasIssue(internal.Issues, "conflicting_field_candidates") || hasDistinctValidCandidates(name, internal.Candidates)
	if conflict {
		field.Value = nil
		field.State = "uncertain"
		field.Issues = appendIssue(field.Issues, "conflicting_field_candidates")
		return ensureUncertainIssue(field), nil
	}

	if internal.Value != nil {
		value := *internal.Value
		if !validPublicValue(name, value) {
			field.Value = nil
			field.State = "missing"
			code := "manual_review_required"
			if name == "birth_date" || name == "issue_date" {
				code = "date_invalid_calendar_or_format"
			}
			field.Issues = appendIssue(field.Issues, code)
			return field, nil
		}
		field.Value = &value
	}

	if field.State == "recognized" && len(field.Issues) > 0 {
		field.State = "uncertain"
	}
	return ensureUncertainIssue(field), nil
}

func mapIssues(internal []string) []PublicIssue {
	issues := make([]PublicIssue, 0, len(internal))
	seen := make(map[string]struct{}, len(internal))
	for _, issue := range internal {
		if issue == "" {
			continue
		}
		code := issue
		message := "Проверьте распознанное значение."
		switch issue {
		case "conflicting_field_candidates":
			message = "Есть несколько вариантов значения. Проверьте поле."
		case "manual_review_required":
		case "date_invalid_calendar_or_format":
			message = "Не удалось получить допустимую дату."
		default:
			code = "manual_review_required"
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		issues = append(issues, PublicIssue{Code: code, Message: message})
	}
	return issues
}

func appendIssue(issues []PublicIssue, code string) []PublicIssue {
	for _, issue := range issues {
		if issue.Code == code {
			return issues
		}
	}
	message := "Проверьте распознанное значение."
	switch code {
	case "conflicting_field_candidates":
		message = "Есть несколько вариантов значения. Проверьте поле."
	case "date_invalid_calendar_or_format":
		message = "Не удалось получить допустимую дату."
	}
	return append(issues, PublicIssue{Code: code, Message: message})
}

func ensureUncertainIssue(field PublicFieldResult) PublicFieldResult {
	if field.State == "uncertain" && len(field.Issues) == 0 {
		field.Issues = appendIssue(field.Issues, "manual_review_required")
	}
	return field
}

func hasDistinctValidCandidates(name string, candidates []passport.Candidate) bool {
	values := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if validPublicValue(name, candidate.Value) {
			values[candidate.Value] = struct{}{}
			if len(values) > 1 {
				return true
			}
		}
	}
	return false
}

func validPublicValue(name, value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	switch name {
	case "gender":
		return value == "M" || value == "F"
	case "birth_date", "issue_date":
		return validISODate(value)
	case "series":
		return seriesPattern.MatchString(value)
	case "number":
		return numberPattern.MatchString(value)
	case "department_code":
		return departmentPattern.MatchString(value)
	default:
		return true
	}
}

func validISODate(value string) bool {
	if len(value) != len("2006-01-02") || value[4] != '-' || value[7] != '-' {
		return false
	}
	for index, char := range value {
		if index == 4 || index == 7 {
			continue
		}
		if char < '0' || char > '9' {
			return false
		}
	}
	parsed, err := time.Parse("2006-01-02", value)
	return err == nil && parsed.Year() >= 1 && parsed.Year() <= 9999 && parsed.Format("2006-01-02") == value
}

func hasIssue(issues []string, wanted string) bool {
	for _, issue := range issues {
		if issue == wanted {
			return true
		}
	}
	return false
}
