package ocr

import "testing"

func TestRussianFieldsExtractionRequiresExactFieldSetAndUUID(t *testing.T) {
	fields := map[string]*string{
		"surname": nil, "given_name": nil, "patronymic": nil, "gender": nil,
		"birth_date": nil, "birth_place": nil, "series": nil, "number": nil,
		"issue_date": nil, "issuer": nil, "department_code": nil,
	}
	const imageID = "f2d5a4c8-4735-4242-8a70-ef42fb6db936"
	if _, err := RussianFieldsExtraction(imageID, fields); err != nil {
		t.Fatalf("RussianFieldsExtraction(valid) error = %v", err)
	}
	delete(fields, "issuer")
	if _, err := RussianFieldsExtraction(imageID, fields); err == nil {
		t.Fatal("RussianFieldsExtraction(missing field) error = nil")
	}
	fields["issuer"] = nil
	fields["unexpected"] = nil
	if _, err := RussianFieldsExtraction(imageID, fields); err == nil {
		t.Fatal("RussianFieldsExtraction(extra field) error = nil")
	}
	delete(fields, "unexpected")
	if _, err := RussianFieldsExtraction("not-a-uuid", fields); err == nil {
		t.Fatal("RussianFieldsExtraction(invalid image ID) error = nil")
	}
}

func TestRussianFieldsExtractionRejectsNULAndOversizedValues(t *testing.T) {
	fields := map[string]*string{
		"surname": nil, "given_name": nil, "patronymic": nil, "gender": nil,
		"birth_date": nil, "birth_place": nil, "series": nil, "number": nil,
		"issue_date": nil, "issuer": nil, "department_code": nil,
	}
	value := "bad\x00value"
	fields["surname"] = &value
	if _, err := RussianFieldsExtraction("f2d5a4c8-4735-4242-8a70-ef42fb6db936", fields); err == nil {
		t.Fatal("RussianFieldsExtraction(NUL value) error = nil")
	}
	value = string(make([]byte, 4097))
	fields["surname"] = &value
	if _, err := RussianFieldsExtraction("f2d5a4c8-4735-4242-8a70-ef42fb6db936", fields); err == nil {
		t.Fatal("RussianFieldsExtraction(oversized value) error = nil")
	}
}
