package passport

import "testing"

func TestStructuredExtractionCanonicalizesEnumsDatesAndCodes(t *testing.T) {
	input := map[string]*string{
		"surname":         structuredPointer("  Ёжов  "),
		"gender":          structuredPointer("Муж."),
		"birth_date":      structuredPointer("01/02/2003"),
		"issue_date":      structuredPointer("2020-01-02"),
		"series":          structuredPointer("0007"),
		"number":          structuredPointer("000042"),
		"department_code": structuredPointer("001002"),
	}
	got := StructuredExtraction("image-id", input)
	if got.SchemaVersion != ExtractionSchema || got.ImageID != "image-id" || !got.ReviewRequired || len(got.Fields) != 11 {
		t.Fatalf("extraction metadata/field count = %#v", got)
	}
	for name, want := range map[string]string{
		"surname": "Ёжов", "gender": "M", "birth_date": "2003-02-01", "issue_date": "2020-01-02",
		"series": "0007", "number": "000042", "department_code": "001-002",
	} {
		field := got.Fields[name]
		if field.Value == nil || *field.Value != want || field.State != "recognized" {
			t.Errorf("%s = %#v, want recognized %q", name, field, want)
		}
	}
	if got.Fields["given_name"].State != "missing" || got.Fields["given_name"].Value != nil {
		t.Errorf("absent values must remain missing: %#v", got.Fields["given_name"])
	}
}

func structuredPointer(value string) *string { return &value }
