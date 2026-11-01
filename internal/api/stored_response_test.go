package api

import (
	"encoding/json"
	"strings"
	"testing"

	"passport-backend/internal/passport"
)

func TestDecodeStoredRecognitionResponsePreservesCanonicalSnapshot(t *testing.T) {
	response, err := MapExtraction(passport.Extraction{}, testRequestID, "pipeline-v7")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeStoredRecognitionResponse(data, testRequestID, "pipeline-v7")
	if err != nil {
		t.Fatalf("DecodeStoredRecognitionResponse() error = %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("stored bytes changed during validation")
	}
}

func TestDecodeStoredRecognitionResponseRejectsCorruptSnapshots(t *testing.T) {
	response, err := MapExtraction(passport.Extraction{}, testRequestID, "pipeline-v7")
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	withIssue, err := MapExtraction(passport.Extraction{Fields: map[string]passport.FieldResult{
		"surname": {State: "uncertain", Issues: []string{"manual_review_required"}},
	}}, testRequestID, "pipeline-v7")
	if err != nil {
		t.Fatal(err)
	}
	withIssueJSON, err := json.Marshal(withIssue)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		data    string
		id      string
		version string
	}{
		{"wrong job identity", string(valid), "8d2fae9c-efb7-4e98-8a90-f10b00edbdf2", "pipeline-v7"},
		{"wrong stored pipeline", string(valid), testRequestID, "pipeline-v8"},
		{"duplicate root key", strings.Replace(string(valid), `"status":"completed"`, `"status":"completed","status":"completed"`, 1), testRequestID, "pipeline-v7"},
		{"missing required field member", strings.Replace(string(valid), `"value":null,`, ``, 1), testRequestID, "pipeline-v7"},
		{"unknown field member", strings.Replace(string(valid), `"state":"missing"`, `"state":"missing","private":"sentinel"`, 1), testRequestID, "pipeline-v7"},
		{"missing one public field", strings.Replace(string(valid), `"department_code":`, `"private_field":`, 1), testRequestID, "pipeline-v7"},
		{"unknown warning code", strings.Replace(string(valid), `"code":"no_fields_recognized"`, `"code":"private_internal_code"`, 1), testRequestID, "pipeline-v7"},
		{"unknown issue code", strings.Replace(string(withIssueJSON), `"code":"manual_review_required"`, `"code":"private_internal_code"`, 1), testRequestID, "pipeline-v7"},
		{"malformed json", `{`, testRequestID, "pipeline-v7"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeStoredRecognitionResponse([]byte(test.data), test.id, test.version); err == nil {
				t.Fatal("DecodeStoredRecognitionResponse() succeeded for corrupt snapshot")
			}
		})
	}
}
