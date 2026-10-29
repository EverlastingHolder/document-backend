// Package mrz parses the national machine-readable zone used by Russian
// internal passports, as specified in Appendix 24 to MVD Order No. 773.
package mrz

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Profile identifies the Russian internal-passport MRZ layout.
// Primary layout source: MVD Order 773, Appendix 24, pp. 110-116:
// https://epp.genproc.gov.ru/upload/iblock/701/11rtwhq8z58el7ebz3gm0lqtlnz21y86.pdf
// General MRZ checksum rules: ICAO Doc 9303 Part 3, section 4.9:
// https://www.icao.int/sites/default/files/publications/DocSeries/9303_p3_cons_en.pdf
const Profile = "ru_internal_passport_app24"

var errUnsupportedCharacter = errors.New("unsupported MRZ character")

// Result describes structural parsing and checksum consistency. A valid
// result does not establish that a document is genuine or that OCR is correct.
type Result struct {
	Profile                string    `json:"profile"`
	Status                 string    `json:"status"`
	RawLines               [2]string `json:"raw_lines"`
	Fields                 Fields    `json:"fields"`
	Checks                 []Check   `json:"checks"`
	Issues                 []string  `json:"issues"`
	NameIssues             []string  `json:"name_issues"`
	NamesComplete          bool      `json:"names_complete"`
	NamesChecksumProtected bool      `json:"names_checksum_protected"`
}

type Fields struct {
	Surname        string    `json:"surname"`
	GivenName      string    `json:"given_name"`
	Patronymic     string    `json:"patronymic"`
	Series         string    `json:"series"`
	Number         string    `json:"number"`
	Gender         string    `json:"gender"`
	DepartmentCode string    `json:"department_code"`
	BirthDate      DateParts `json:"birth_date"`
	IssueDate      DateParts `json:"issue_date"`
}

// DateParts preserves the two-digit year and the literal raw MRZ date. An
// unknown month or day is represented by 00 and a false Known flag.
type DateParts struct {
	Raw           string `json:"raw"`
	YearTwoDigits int    `json:"year_two_digits"`
	Month         int    `json:"month"`
	Day           int    `json:"day"`
	MonthKnown    bool   `json:"month_known"`
	DayKnown      bool   `json:"day_known"`
}

type Check struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
}

var mrzToCyrillic = map[byte]rune{
	'A': 'А', 'B': 'Б', 'V': 'В', 'G': 'Г', 'D': 'Д', 'E': 'Е', '2': 'Ё',
	'J': 'Ж', 'Z': 'З', 'I': 'И', 'Q': 'Й', 'K': 'К', 'L': 'Л', 'M': 'М',
	'N': 'Н', 'O': 'О', 'P': 'П', 'R': 'Р', 'S': 'С', 'T': 'Т', 'U': 'У',
	'F': 'Ф', 'H': 'Х', 'C': 'Ц', '3': 'Ч', '4': 'Ш', 'W': 'Щ', 'X': 'Ъ',
	'Y': 'Ы', '9': 'Ь', '6': 'Э', '7': 'Ю', '8': 'Я',
}

// CheckDigit returns the ASCII check digit for one MRZ data element.
func CheckDigit(data string) (byte, error) {
	if data == "" {
		return 0, errors.New("empty check digit data")
	}
	weights := [...]int{7, 3, 1}
	sum := 0
	for i := 0; i < len(data); i++ {
		value, ok := mrzValue(data[i])
		if !ok {
			return 0, errUnsupportedCharacter
		}
		sum += value * weights[i%len(weights)]
	}
	return byte('0' + sum%10), nil
}

func mrzValue(char byte) (int, bool) {
	switch {
	case char >= '0' && char <= '9':
		return int(char - '0'), true
	case char >= 'A' && char <= 'Z':
		return int(char-'A') + 10, true
	case char == '<':
		return 0, true
	default:
		return 0, false
	}
}

// DecodeNamePart decodes one uninterrupted modified-CLEER name segment.
func DecodeNamePart(code string) (string, error) {
	if code == "" {
		return "", errors.New("empty name part")
	}
	var decoded strings.Builder
	decoded.Grow(len(code) * 2)
	for i := 0; i < len(code); i++ {
		letter, ok := mrzToCyrillic[code[i]]
		if !ok {
			return "", errUnsupportedCharacter
		}
		decoded.WriteRune(letter)
	}
	return decoded.String(), nil
}

// Parse validates an already-normalized pair of national MRZ lines. It does
// not trim, pad, correct OCR, or attempt to parse other MRZ profiles.
func Parse(lines [2]string) Result {
	result := Result{
		Profile:  Profile,
		Status:   "indeterminate",
		RawLines: lines,
		Checks: []Check{
			{Name: "document_number", Status: "indeterminate"},
			{Name: "birth_date", Status: "indeterminate"},
			{Name: "auxiliary", Status: "indeterminate"},
			{Name: "composite", Status: "indeterminate"},
		},
		Issues:                 []string{},
		NameIssues:             []string{},
		NamesChecksumProtected: false,
	}

	if lines[0] == "" && lines[1] == "" {
		result.Issues = append(result.Issues, "lines_missing")
		return result
	}
	for _, line := range lines {
		if len(line) == 0 {
			result.Issues = appendUnique(result.Issues, "line_missing")
			continue
		}
		if len(line) < 44 {
			result.Issues = appendUnique(result.Issues, "line_incomplete")
			continue
		}
		if len(line) > 44 {
			result.Issues = appendUnique(result.Issues, "line_length_invalid")
		}
	}
	if hasIssue(result.Issues, "line_incomplete") || hasIssue(result.Issues, "line_missing") {
		return result
	}
	if hasIssue(result.Issues, "line_length_invalid") {
		return invalid(result)
	}

	upper, lower := lines[0], lines[1]
	if !validUpperCharset(upper) || !validLowerCharset(lower) {
		result.Issues = append(result.Issues, "character_invalid")
		return invalid(result)
	}
	if upper[:2] != "PN" || upper[2:5] != "RUS" || lower[10:13] != "RUS" {
		result.Issues = append(result.Issues, "profile_not_supported")
		return result
	}

	result.parseNames(upper[5:])
	result.parseLower(lower)
	if hasIssue(result.NameIssues, "name_separator_missing") || hasIssue(result.NameIssues, "name_part_missing") || hasIssue(result.NameIssues, "name_character_invalid") || hasIssue(result.NameIssues, "given_name_missing") {
		result.Issues = appendUnique(result.Issues, "name_format_invalid")
	}
	if len(result.Issues) > 0 || hasInvalidCheck(result.Checks) {
		return invalid(result)
	}
	if hasIndeterminateCheck(result.Checks) {
		result.Status = "indeterminate"
		return result
	}
	result.Status = "valid"
	return result
}

func (result *Result) parseNames(nameField string) {
	separator := strings.Index(nameField, "<<")
	if separator < 0 {
		result.NameIssues = append(result.NameIssues, "name_separator_missing")
		return
	}

	familyCode := nameField[:separator]
	restCode := strings.TrimRight(nameField[separator+2:], "<")
	if familyCode == "" || restCode == "" {
		result.NameIssues = append(result.NameIssues, "name_part_missing")
		return
	}

	familyParts := strings.Split(familyCode, "<")
	decodedFamily := make([]string, 0, len(familyParts))
	for _, part := range familyParts {
		if part == "" {
			result.NameIssues = appendUnique(result.NameIssues, "name_component_empty")
			continue
		}
		value, err := DecodeNamePart(part)
		if err != nil {
			result.NameIssues = appendUnique(result.NameIssues, "name_character_invalid")
			continue
		}
		decodedFamily = append(decodedFamily, value)
	}
	if len(decodedFamily) > 0 {
		result.Fields.Surname = strings.Join(decodedFamily, " ")
	}
	if len(familyParts) > 1 {
		result.NameIssues = appendUnique(result.NameIssues, "compound_surname_ambiguous")
	}

	nameParts := strings.Split(restCode, "<")
	decodedParts := make([]string, 0, len(nameParts))
	for _, part := range nameParts {
		if part == "" {
			continue
		}
		value, err := DecodeNamePart(part)
		if err != nil {
			result.NameIssues = appendUnique(result.NameIssues, "name_character_invalid")
			continue
		}
		decodedParts = append(decodedParts, value)
	}
	if len(decodedParts) == 0 {
		result.NameIssues = appendUnique(result.NameIssues, "given_name_missing")
	} else {
		result.Fields.GivenName = decodedParts[0]
	}
	if len(decodedParts) == 2 {
		result.Fields.Patronymic = decodedParts[1]
	} else if len(decodedParts) > 2 {
		result.Fields.Patronymic = strings.Join(decodedParts[1:], " ")
		result.NameIssues = appendUnique(result.NameIssues, "compound_name_ambiguous")
	}

	if !strings.HasSuffix(nameField, "<") {
		result.NameIssues = appendUnique(result.NameIssues, "name_may_be_truncated")
	}
	// Punctuation is omitted and hyphens are encoded as fillers in this MRZ;
	// the original spelling cannot be recovered from the transliteration.
	result.NameIssues = appendUnique(result.NameIssues, "punctuation_not_recoverable")
	result.NamesComplete = !hasIssue(result.NameIssues, "name_component_empty") &&
		!hasIssue(result.NameIssues, "name_character_invalid") &&
		!hasIssue(result.NameIssues, "compound_surname_ambiguous") &&
		!hasIssue(result.NameIssues, "compound_name_ambiguous") &&
		!hasIssue(result.NameIssues, "name_may_be_truncated") &&
		!hasIssue(result.NameIssues, "name_part_missing")
}

func (result *Result) parseLower(line string) {
	for i, field := range []struct {
		name  string
		data  string
		check byte
	}{
		{name: "document_number", data: line[:9], check: line[9]},
		{name: "birth_date", data: line[13:19], check: line[19]},
		{name: "auxiliary", data: line[28:42], check: line[42]},
	} {
		result.Checks[i] = evaluateCheck(field.name, field.data, field.check)
	}
	composite := line[:10] + line[13:20] + line[21:28] + line[28:43]
	result.Checks[3] = evaluateCheck("composite", composite, line[43])

	if !digits(line[:10]) || line[9] < '0' || line[9] > '9' {
		result.Issues = appendUnique(result.Issues, "document_number_format_invalid")
	} else {
		result.Fields.Series = line[:3] + string(line[28])
		result.Fields.Number = line[3:9]
	}
	if line[21:28] != "<<<<<<<" {
		result.Issues = appendUnique(result.Issues, "expiry_filler_invalid")
	}
	if line[27] != '<' {
		result.Issues = appendUnique(result.Issues, "position_28_filler_invalid")
	}
	if line[20] != 'M' && line[20] != 'F' {
		result.Issues = appendUnique(result.Issues, "gender_invalid")
	} else {
		result.Fields.Gender = string(line[20])
	}
	if !digits(line[13:19]) || !isDigit(line[19]) {
		result.Issues = appendUnique(result.Issues, "birth_date_format_invalid")
	} else if parts, valid := parseDateParts(line[13:19], true); valid {
		result.Fields.BirthDate = parts
	} else {
		result.Issues = appendUnique(result.Issues, "birth_date_invalid")
	}
	if !digits(line[28:41]) || line[41] != '<' || !isDigit(line[42]) || !digits(line[29:35]) || !digits(line[35:41]) {
		result.Issues = appendUnique(result.Issues, "auxiliary_format_invalid")
	} else if parts, valid := parseDateParts(line[29:35], false); valid {
		result.Fields.IssueDate = parts
		result.Fields.DepartmentCode = line[35:41]
	} else {
		result.Issues = appendUnique(result.Issues, "issue_date_invalid")
	}
}

func evaluateCheck(name, data string, observed byte) Check {
	check := Check{Name: name, Observed: string(observed)}
	if observed < '0' || observed > '9' {
		check.Status = "invalid"
		return check
	}
	expected, err := CheckDigit(data)
	if err != nil {
		check.Status = "invalid"
		return check
	}
	check.Expected = string(expected)
	if expected == observed {
		check.Status = "valid"
	} else {
		check.Status = "invalid"
	}
	return check
}

func parseDateParts(raw string, allowUnknownDayOrMonth bool) (DateParts, bool) {
	if len(raw) != 6 || !digits(raw) {
		return DateParts{}, false
	}
	year, _ := strconv.Atoi(raw[:2])
	month, _ := strconv.Atoi(raw[2:4])
	day, _ := strconv.Atoi(raw[4:6])
	parts := DateParts{
		Raw: raw, YearTwoDigits: year, Month: month, Day: day,
		MonthKnown: month != 0, DayKnown: day != 0,
	}
	if !allowUnknownDayOrMonth && (month == 0 || day == 0) {
		return DateParts{}, false
	}
	if month == 0 && allowUnknownDayOrMonth {
		if month != 0 || day > 31 {
			return DateParts{}, false
		}
		return parts, true
	}
	if month < 1 || month > 12 || day > 31 {
		return DateParts{}, false
	}
	if day == 0 && allowUnknownDayOrMonth {
		return parts, true
	}
	if day < 1 {
		return DateParts{}, false
	}
	maxDay := [...]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[month-1]
	if month == 2 && year%4 == 0 {
		maxDay = 29
	}
	if day > maxDay {
		return DateParts{}, false
	}
	return parts, true
}

// ResolveDate resolves a YYMMDD MRZ date only when a full ISO VIZ date is
// valid and its known components exactly agree with the MRZ components.
func ResolveDate(parts DateParts, vizISO string) (string, bool) {
	if len(vizISO) != 10 {
		return "", false
	}
	vizDate, err := time.Parse("2006-01-02", vizISO)
	if err != nil || vizDate.Year() < 1 || vizDate.Format("2006-01-02") != vizISO || len(parts.Raw) != 6 || !digits(parts.Raw) {
		return "", false
	}
	if parts.YearTwoDigits != mustInt(parts.Raw[:2]) || parts.YearTwoDigits != int(vizDate.Year()%100) || parts.Month != mustInt(parts.Raw[2:4]) || parts.Day != mustInt(parts.Raw[4:6]) {
		return "", false
	}
	if parts.MonthKnown != (parts.Month != 0) || parts.DayKnown != (parts.Day != 0) {
		return "", false
	}
	if parts.MonthKnown && parts.Month != int(vizDate.Month()) {
		return "", false
	}
	if parts.DayKnown && parts.Day != vizDate.Day() {
		return "", false
	}
	return vizISO, true
}

func mustInt(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func validUpperCharset(line string) bool {
	for i := 0; i < len(line); i++ {
		char := line[i]
		if !((char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '<') {
			return false
		}
	}
	return true
}

func validLowerCharset(line string) bool {
	for i := 0; i < len(line); i++ {
		char := line[i]
		if !((char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '<') {
			return false
		}
	}
	return true
}

func digits(value string) bool {
	for i := 0; i < len(value); i++ {
		if !isDigit(value[i]) {
			return false
		}
	}
	return true
}

func isDigit(char byte) bool { return char >= '0' && char <= '9' }

func invalid(result Result) Result {
	result.Status = "invalid"
	return result
}

func hasIssue(issues []string, wanted string) bool {
	for _, issue := range issues {
		if issue == wanted {
			return true
		}
	}
	return false
}

func hasInvalidCheck(checks []Check) bool {
	for _, check := range checks {
		if check.Status == "invalid" {
			return true
		}
	}
	return false
}

func hasIndeterminateCheck(checks []Check) bool {
	for _, check := range checks {
		if check.Status == "indeterminate" {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	if !hasIssue(values, value) {
		return append(values, value)
	}
	return values
}
