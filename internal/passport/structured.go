package passport

import (
	"strings"
)

// StructuredExtraction adapts already-structured engine output without sending
// it through the generic OCR observation assembler. Public validation remains
// in internal/api.MapExtraction.
func StructuredExtraction(imageID string, values map[string]*string) Extraction {
	fields := make(map[string]FieldResult, 11)
	for _, name := range []string{
		"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
		"series", "number", "issue_date", "issuer", "department_code",
	} {
		field := emptyField()
		raw, exists := values[name]
		if exists && raw != nil {
			value := strings.TrimSpace(*raw)
			if value != "" {
				switch name {
				case "gender":
					label := strings.TrimSuffix(value, ".")
					if canonical, ok := canonicalGender(label); ok {
						value = canonical
					}
				case "birth_date", "issue_date":
					dateValue := strings.ReplaceAll(value, "/", ".")
					if canonical, ok := parseDate(dateValue); ok {
						value = canonical
					}
				case "department_code":
					value = canonicalDepartment(value)
				}
				field.Value = &value
				field.State = "recognized"
				field.Candidates = []Candidate{{Value: value}}
			}
		}
		fields[name] = field
	}
	return Extraction{
		SchemaVersion:  ExtractionSchema,
		ImageID:        imageID,
		ReviewRequired: true,
		Fields:         fields,
	}
}
