package ocr

import (
	"errors"
	"regexp"
	"strings"

	"passport-backend/internal/passport"
)

var russianWorkerFields = []string{
	"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place",
	"series", "number", "issue_date", "issuer", "department_code",
}

var russianIdentifierPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// RussianFieldsExtraction validates and adapts the worker's exact eleven-field
// payload into the shared internal extraction model. The public Go mapper
// remains responsible for canonical response validation and serialization.
func RussianFieldsExtraction(imageID string, values map[string]*string) (passport.Extraction, error) {
	if !validRussianFields(values) || !russianIdentifierPattern.MatchString(imageID) {
		return passport.Extraction{}, errors.New("invalid RussianDocsOCR fields")
	}
	copyValues := make(map[string]*string, len(values))
	for name, value := range values {
		copyValues[name] = value
	}
	return passport.StructuredExtraction(imageID, copyValues), nil
}

func validRussianFields(fields map[string]*string) bool {
	if fields == nil || len(fields) != len(russianWorkerFields) {
		return false
	}
	for _, name := range russianWorkerFields {
		if _, ok := fields[name]; !ok {
			return false
		}
		if value := fields[name]; value != nil && (len(*value) > 4096 || strings.ContainsRune(*value, '\x00')) {
			return false
		}
	}
	return true
}
