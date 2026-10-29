package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"passport-backend/internal/passport"
)

const testRequestID = "94b86c5c-2520-4c0f-b91e-6d4a948fc88a"

func TestMapExtractionReturnsOnlyPublicEnvelopeAndAllFields(t *testing.T) {
	input := validExtraction()
	input.ImageID = "PRIVATE-IMAGE-SENTINEL"
	input.SchemaVersion = "PRIVATE-SCHEMA-SENTINEL"
	input.MRZ = &passport.MRZDiagnostics{}
	field := input.Fields["surname"]
	field.Raw = "PRIVATE-RAW-SENTINEL"
	field.Candidates = []passport.Candidate{{
		Value: "PRIVATE-CANDIDATE-SENTINEL", Raw: "PRIVATE-CANDIDATE-RAW-SENTINEL",
		Rules: []string{"PRIVATE-RULE-SENTINEL"}, LineIDs: []string{"PRIVATE-LINE-SENTINEL"},
		PassIDs: []string{"PRIVATE-PASS-SENTINEL"}, Context: "PRIVATE-CONTEXT-SENTINEL",
	}}
	field.Evidence = passport.Evidence{LineIDs: []string{"PRIVATE-EVIDENCE-SENTINEL"}, PassIDs: []string{"PRIVATE-EVIDENCE-PASS-SENTINEL"}}
	input.Fields["surname"] = field

	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	body := marshalResponse(t, response)
	if body["request_id"] != testRequestID || body["status"] != "completed" || body["review_required"] != true || body["pipeline_version"] != "pipeline-v1" {
		t.Fatalf("unexpected response envelope: %#v", body)
	}
	fields := objectAt(t, body, "fields")
	wantNames := []string{"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place", "series", "number", "issue_date", "issuer", "department_code"}
	if len(fields) != len(wantNames) {
		t.Fatalf("fields has %d keys, want exactly %d: %#v", len(fields), len(wantNames), fields)
	}
	for _, name := range wantNames {
		field := objectAt(t, fields, name)
		if _, ok := field["value"]; !ok {
			t.Errorf("fields.%s.value is omitted", name)
		}
		if _, ok := field["state"]; !ok {
			t.Errorf("fields.%s.state is omitted", name)
		}
		if issues, ok := field["issues"].([]any); !ok || issues == nil {
			t.Errorf("fields.%s.issues = %#v, want a non-nil array", name, field["issues"])
		}
	}
	warnings, ok := body["warnings"].([]any)
	if !ok || warnings == nil || len(warnings) != 0 {
		t.Errorf("warnings = %#v, want []", body["warnings"])
	}
	for _, sentinel := range []string{
		"PRIVATE-IMAGE-SENTINEL", "PRIVATE-SCHEMA-SENTINEL", "PRIVATE-RAW-SENTINEL",
		"PRIVATE-CANDIDATE-SENTINEL", "PRIVATE-CANDIDATE-RAW-SENTINEL", "PRIVATE-RULE-SENTINEL",
		"PRIVATE-LINE-SENTINEL", "PRIVATE-PASS-SENTINEL", "PRIVATE-CONTEXT-SENTINEL",
		"PRIVATE-EVIDENCE-SENTINEL", "PRIVATE-EVIDENCE-PASS-SENTINEL",
	} {
		if strings.Contains(string(mustJSON(t, response)), sentinel) {
			t.Errorf("public JSON leaked private sentinel %q", sentinel)
		}
	}
}

func TestMapExtractionUsesMissingForAbsentAndIgnoresExtraInternalFields(t *testing.T) {
	input := validExtraction()
	delete(input.Fields, "patronymic")
	input.Fields["future_private_field"] = passport.FieldResult{Value: stringPointer("PRIVATE-EXTRA-SENTINEL"), State: "recognized"}

	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	body := marshalResponse(t, response)
	fields := objectAt(t, body, "fields")
	patronymic := objectAt(t, fields, "patronymic")
	if patronymic["value"] != nil || patronymic["state"] != "missing" {
		t.Errorf("missing patronymic = %#v, want null/missing", patronymic)
	}
	if len(fields) != 11 || strings.Contains(string(mustJSON(t, response)), "PRIVATE-EXTRA-SENTINEL") {
		t.Errorf("unexpected extra public field or leaked value: %#v", fields)
	}
}

func TestMapExtractionAddsNoFieldsWarningAfterNullFiltering(t *testing.T) {
	input := allNullExtraction()
	input.Fields["birth_date"] = passport.FieldResult{
		State: "uncertain", Issues: []string{"date_invalid_calendar_or_format"},
		Candidates: []passport.Candidate{{Value: "2023-02-29"}},
	}
	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	body := marshalResponse(t, response)
	warnings, ok := body["warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want one all-null warning", body["warnings"])
	}
	warning := warnings[0].(map[string]any)
	if warning["code"] != "no_fields_recognized" || warning["message"] == "" {
		t.Errorf("warning = %#v, want safe no_fields_recognized code and message", warning)
	}
}

func TestMapExtractionSuppressesNoFieldsWarningForValidUncertainValue(t *testing.T) {
	input := allNullExtraction()
	input.Fields["given_name"] = passport.FieldResult{
		Value: stringPointer("АЛЕКСЕЙ"), State: "uncertain", Issues: []string{"manual_review_required"},
	}
	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	body := marshalResponse(t, response)
	warnings, ok := body["warnings"].([]any)
	if !ok || warnings == nil || len(warnings) != 0 {
		t.Errorf("warnings = %#v, want [] when uncertain field has a public value", body["warnings"])
	}
}

func TestMapExtractionPreservesCanonicalValuesIncludingLeadingZeroes(t *testing.T) {
	input := validExtraction()
	series := input.Fields["series"]
	series.Value = stringPointer("0012")
	input.Fields["series"] = series
	number := input.Fields["number"]
	number.Value = stringPointer("000345")
	input.Fields["number"] = number
	input.Fields["patronymic"] = passport.FieldResult{Value: stringPointer("ПЕТРОВИЧ"), State: "uncertain", Issues: []string{"manual_review_required"}}

	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	fields := objectAt(t, marshalResponse(t, response), "fields")
	if got := objectAt(t, fields, "series")["value"]; got != "0012" {
		t.Errorf("series value = %#v, want leading zeroes preserved", got)
	}
	if got := objectAt(t, fields, "number")["value"]; got != "000345" {
		t.Errorf("number value = %#v, want leading zeroes preserved", got)
	}
}

func TestMapExtractionValidatesValuesForUncertainFieldsToo(t *testing.T) {
	tests := []struct {
		name, field, value, state, safeCode string
	}{
		{"invalid uncertain gender", "gender", "X", "uncertain", "manual_review_required"},
		{"invalid uncertain date", "birth_date", "2023-02-29", "uncertain", "date_invalid_calendar_or_format"},
		{"whitespace name", "surname", "   ", "uncertain", "manual_review_required"},
		{"short series", "series", "123", "uncertain", "manual_review_required"},
		{"bad department code", "department_code", "123456", "uncertain", "manual_review_required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validExtraction()
			input.Fields[test.field] = passport.FieldResult{Value: stringPointer(test.value), State: test.state, Issues: []string{"manual_review_required"}}
			response, err := MapExtraction(input, testRequestID, "pipeline-v1")
			if err != nil {
				t.Fatalf("MapExtraction() error = %v", err)
			}
			field := objectAt(t, objectAt(t, marshalResponse(t, response), "fields"), test.field)
			if field["value"] != nil || field["state"] != "missing" {
				t.Errorf("invalid field = %#v, want withheld missing/null", field)
			}
			if got := issueCodes(t, field); !containsString(got, test.safeCode) {
				t.Errorf("issue codes = %#v, want %q", got, test.safeCode)
			}
		})
	}
}

func TestMapExtractionUsesRealCalendarValidation(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"2024-02-29", true},
		{"2023-02-29", false},
		{"2024-13-01", false},
		{"0000-01-01", false},
	} {
		t.Run(test.value, func(t *testing.T) {
			input := validExtraction()
			birthDate := input.Fields["birth_date"]
			birthDate.Value = stringPointer(test.value)
			input.Fields["birth_date"] = birthDate
			response, err := MapExtraction(input, testRequestID, "pipeline-v1")
			if err != nil {
				t.Fatalf("MapExtraction() error = %v", err)
			}
			field := objectAt(t, objectAt(t, marshalResponse(t, response), "fields"), "birth_date")
			if test.valid {
				if field["value"] != test.value {
					t.Errorf("date value = %#v, want %q", field["value"], test.value)
				}
			} else if field["value"] != nil || field["state"] != "missing" || !containsString(issueCodes(t, field), "date_invalid_calendar_or_format") {
				t.Errorf("invalid date result = %#v", field)
			}
		})
	}
}

func TestMapExtractionMapsIssuesToSafePublicMessagesAndDownshiftsRecognized(t *testing.T) {
	input := validExtraction()
	surname := input.Fields["surname"]
	surname.Issues = []string{
		"manual_review_required", "manual_review_required",
		"PRIVATE-PII-SENTINEL: ИВАНОВ 001234", "", "unknown_internal_issue",
	}
	input.Fields["surname"] = surname
	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	bodyJSON := string(mustJSON(t, response))
	if strings.Contains(bodyJSON, "PRIVATE-PII-SENTINEL") || strings.Contains(bodyJSON, "unknown_internal_issue") || strings.Contains(bodyJSON, "unknown_internal_issue") {
		t.Fatalf("private issue string leaked: %s", bodyJSON)
	}
	field := objectAt(t, objectAt(t, marshalResponse(t, response), "fields"), "surname")
	if field["state"] != "uncertain" || field["value"] != "ИВАНОВ" {
		t.Errorf("recognized field with issues = %#v, want uncertain with canonical value", field)
	}
	codes := issueCodes(t, field)
	if !reflect.DeepEqual(codes, []string{"manual_review_required"}) {
		t.Errorf("mapped issue codes = %#v, want stable deduplicated safe codes", codes)
	}
	for _, rawIssue := range field["issues"].([]any) {
		issue := rawIssue.(map[string]any)
		if issue["message"] == "" || strings.Contains(issue["message"].(string), "ИВАНОВ") {
			t.Errorf("unsafe or empty issue message: %#v", issue)
		}
	}
}

func TestMapExtractionMakesUncertainWithoutIssuesReviewable(t *testing.T) {
	input := validExtraction()
	input.Fields["given_name"] = passport.FieldResult{Value: stringPointer("АЛЕКСЕЙ"), State: "uncertain"}
	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	field := objectAt(t, objectAt(t, marshalResponse(t, response), "fields"), "given_name")
	if field["state"] != "uncertain" || !containsString(issueCodes(t, field), "manual_review_required") {
		t.Errorf("uncertain without source issue = %#v, want generic review issue", field)
	}
}

func TestMapExtractionConflictsAbstainWithoutChoosingCandidate(t *testing.T) {
	input := validExtraction()
	input.Fields["surname"] = passport.FieldResult{
		Value: stringPointer("ИВАНОВ"), State: "recognized", Issues: []string{"conflicting_field_candidates"},
		Candidates: []passport.Candidate{{Value: "ИВАНОВ"}, {Value: "ПЕТРОВ"}, {Value: "ПЕТРОВ"}},
	}
	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	field := objectAt(t, objectAt(t, marshalResponse(t, response), "fields"), "surname")
	if field["value"] != nil || field["state"] != "uncertain" || !containsString(issueCodes(t, field), "conflicting_field_candidates") {
		t.Errorf("conflicting field = %#v, want uncertain/null/conflict", field)
	}
	if len(issueCodes(t, field)) != 1 {
		t.Errorf("duplicate candidate conflict should map once: %#v", field["issues"])
	}
}

func TestMapExtractionRejectsMalformedInternalCombinations(t *testing.T) {
	tests := []struct {
		name  string
		field passport.FieldResult
	}{
		{"unknown state", passport.FieldResult{Value: stringPointer("ИВАНОВ"), State: "guess"}},
		{"missing with value", passport.FieldResult{Value: stringPointer("ИВАНОВ"), State: "missing"}},
		{"recognized without value", passport.FieldResult{State: "recognized"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validExtraction()
			input.Fields["surname"] = test.field
			if _, err := MapExtraction(input, testRequestID, "pipeline-v1"); err == nil {
				t.Fatal("MapExtraction() error = nil, want safe mapper error")
			}
		})
	}
}

func TestMapExtractionRejectsMalformedRequestMetadata(t *testing.T) {
	for _, test := range []struct {
		name, requestID, pipelineVersion string
	}{
		{"bad UUID", "not-a-uuid", "pipeline-v1"},
		{"empty UUID", "", "pipeline-v1"},
		{"blank pipeline version", testRequestID, "  \t "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := MapExtraction(validExtraction(), test.requestID, test.pipelineVersion); err == nil {
				t.Fatal("MapExtraction() error = nil, want metadata validation error")
			}
		})
	}
}

func TestMapExtractionDoesNotMutateInput(t *testing.T) {
	input := validExtraction()
	surname := input.Fields["surname"]
	surname.Issues = []string{"manual_review_required", "manual_review_required"}
	input.Fields["surname"] = surname
	input.Fields["given_name"] = passport.FieldResult{Value: stringPointer("АЛЕКСЕЙ"), State: "uncertain"}
	before := cloneExtraction(input)
	if _, err := MapExtraction(input, testRequestID, "pipeline-v1"); err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("MapExtraction() mutated its input extraction")
	}
}

func TestMapExtractionCopiesPublicValueAwayFromInput(t *testing.T) {
	input := validExtraction()
	sourceValue := input.Fields["surname"].Value
	response, err := MapExtraction(input, testRequestID, "pipeline-v1")
	if err != nil {
		t.Fatalf("MapExtraction() error = %v", err)
	}
	publicValue := response.Fields["surname"].Value
	if sourceValue == nil || publicValue == nil {
		t.Fatal("surname values are nil, want a canonical value in both models")
	}

	*sourceValue = "ПЕТРОВ"
	if got := *publicValue; got != "ИВАНОВ" {
		t.Errorf("public value changed with source mutation: got %q, want %q", got, "ИВАНОВ")
	}

	*publicValue = "СИДОРОВ"
	if got := *sourceValue; got != "ПЕТРОВ" {
		t.Errorf("source value changed with public mutation: got %q, want %q", got, "ПЕТРОВ")
	}
}

func validExtraction() passport.Extraction {
	values := map[string]string{
		"surname": "ИВАНОВ", "given_name": "АЛЕКСЕЙ", "gender": "M",
		"birth_date": "1990-04-12", "birth_place": "ГОРОД ПРИМЕР", "series": "0012",
		"number": "000345", "issue_date": "2010-06-18", "issuer": "ОТДЕЛ ПРИМЕРА",
		"department_code": "123-456",
	}
	fields := make(map[string]passport.FieldResult, 11)
	for name, value := range values {
		fields[name] = passport.FieldResult{Value: stringPointer(value), State: "recognized", Candidates: []passport.Candidate{}, Rules: []string{}, Issues: []string{}}
	}
	fields["patronymic"] = passport.FieldResult{State: "missing", Candidates: []passport.Candidate{}, Rules: []string{}, Issues: []string{}}
	return passport.Extraction{SchemaVersion: passport.ExtractionSchema, ImageID: "private-image", ReviewRequired: false, Fields: fields}
}

func allNullExtraction() passport.Extraction {
	input := validExtraction()
	for name, field := range input.Fields {
		field.Value = nil
		field.State = "missing"
		field.Issues = []string{}
		input.Fields[name] = field
	}
	return input
}

func cloneExtraction(input passport.Extraction) passport.Extraction {
	clone := input
	clone.Fields = make(map[string]passport.FieldResult, len(input.Fields))
	for name, field := range input.Fields {
		copyField := field
		if field.Value != nil {
			copyField.Value = stringPointer(*field.Value)
		}
		if field.Issues != nil {
			copyField.Issues = append([]string{}, field.Issues...)
		}
		if field.Rules != nil {
			copyField.Rules = append([]string{}, field.Rules...)
		}
		if field.Candidates != nil {
			copyField.Candidates = append([]passport.Candidate{}, field.Candidates...)
		}
		clone.Fields[name] = copyField
	}
	return clone
}

func marshalResponse(t *testing.T, response any) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(mustJSON(t, response), &body); err != nil {
		t.Fatalf("response JSON is not an object: %v", err)
	}
	return body
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return data
}

func objectAt(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	got, ok := value[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, value[key])
	}
	return got
}

func issueCodes(t *testing.T, field map[string]any) []string {
	t.Helper()
	raw, ok := field["issues"].([]any)
	if !ok {
		t.Fatalf("issues = %#v, want array", field["issues"])
	}
	codes := make([]string, 0, len(raw))
	for _, item := range raw {
		issue, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("issue = %#v, want object", item)
		}
		code, ok := issue["code"].(string)
		if !ok {
			t.Fatalf("issue code = %#v, want string", issue["code"])
		}
		codes = append(codes, code)
	}
	return codes
}

func stringPointer(value string) *string { return &value }

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
