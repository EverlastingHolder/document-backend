package mrz

import (
	"strings"
	"testing"
)

func TestCheckDigitReferenceVectors(t *testing.T) {
	for _, test := range []struct {
		data string
		want byte
	}{
		{data: "123456789", want: '7'},
		{data: "AB2134<<<", want: '5'},
	} {
		got, err := CheckDigit(test.data)
		if err != nil || got != test.want {
			t.Fatalf("CheckDigit(%q) = %q, %v; want %q", test.data, got, err, test.want)
		}
	}
	for _, unsupported := range []string{"abc", "AB-12", "А"} {
		if _, err := CheckDigit(unsupported); err == nil {
			t.Errorf("CheckDigit accepted unsupported input %q", unsupported)
		}
	}
}

func TestModifiedCleerMapRoundTripsAllRussianLetters(t *testing.T) {
	russian := "АБВГДЕЁЖЗИЙКЛМНОПРСТУФХЦЧШЩЪЫЬЭЮЯ"
	encoded := "ABVGDE2JZIQKLMNOPRSTUFHC34WXY9678"
	if len([]rune(russian)) != len(encoded) {
		t.Fatal("reference mapping lengths differ")
	}
	for i, letter := range []rune(russian) {
		got, err := DecodeNamePart(string(encoded[i]))
		if err != nil || got != string(letter) {
			t.Errorf("mapping at index %d = %q, %v; want %q", i, got, err, string(letter))
		}
	}
	if _, err := DecodeNamePart("0"); err == nil {
		t.Fatal("DecodeNamePart accepted an unsupported digit")
	}
}

func TestParseValidNationalFixture(t *testing.T) {
	lines := nationalFixture("900101")
	result := Parse(lines)
	if result.Status != "valid" {
		t.Fatalf("Parse status = %q, issues=%v checks=%+v", result.Status, result.Issues, result.Checks)
	}
	if result.Fields.Surname != "ИВАНОВ" || result.Fields.GivenName != "ИВАН" || result.Fields.Patronymic != "ИВАНОВИЧ" {
		t.Fatalf("decoded names = %+v", result.Fields)
	}
	if result.Fields.Series != "1234" || result.Fields.Number != "456789" || result.Fields.DepartmentCode != "123456" {
		t.Fatalf("decoded identifiers = %+v", result.Fields)
	}
	if result.Fields.BirthDate.Raw != "900101" || !result.Fields.BirthDate.MonthKnown || !result.Fields.BirthDate.DayKnown {
		t.Fatalf("birth date = %+v", result.Fields.BirthDate)
	}
	if result.Fields.IssueDate.Raw != "230101" || result.NamesChecksumProtected {
		t.Fatalf("issue date or names checksum metadata incorrect: %+v", result)
	}
}

func TestNationalFixtureUsesIndependentChecksumConstants(t *testing.T) {
	lines := [2]string{
		"PNRUSIVANOV<<IVAN<IVANOVI3" + strings.Repeat("<", 18),
		"1234567897RUS9001011M<<<<<<<4230101123456<20",
	}
	if len(lines[0]) != 44 || len(lines[1]) != 44 {
		t.Fatalf("static fixture lengths = %d and %d", len(lines[0]), len(lines[1]))
	}
	result := Parse(lines)
	if result.Status != "valid" {
		t.Fatalf("static checksum fixture rejected: %+v", result)
	}
	for i, want := range []string{"7", "1", "2", "0"} {
		if result.Checks[i].Expected != want || result.Checks[i].Observed != want {
			t.Errorf("check %q = expected %q observed %q; want %q", result.Checks[i].Name, result.Checks[i].Expected, result.Checks[i].Observed, want)
		}
	}
	if got, err := CheckDigit("4230101123456<"); err != nil || got != '2' {
		t.Errorf("auxiliary reference checksum = %q, %v; want 2", got, err)
	}
	compositeWithoutSex := "12345678979001011<<<<<<<4230101123456<2"
	compositeWithSex := "12345678979001011M<<<<<<<4230101123456<2"
	if got, err := CheckDigit(compositeWithoutSex); err != nil || got != '0' {
		t.Errorf("composite checksum without sex = %q, %v; want 0", got, err)
	}
	if got, err := CheckDigit(compositeWithSex); err != nil || got != '2' {
		t.Errorf("composite checksum with sex = %q, %v; want 2", got, err)
	}
}

func TestParseRejectsEachCheckDigitMutation(t *testing.T) {
	for _, position := range []int{9, 19, 42, 43} {
		lines := nationalFixture("900101")
		lines[1] = replaceDigit(lines[1], position)
		result := Parse(lines)
		if result.Status != "invalid" {
			t.Errorf("mutation at byte %d produced status %q", position, result.Status)
		}
		if result.Checks[checkIndex(position)].Status != "invalid" {
			t.Errorf("mutation at byte %d did not fail corresponding check: %+v", position, result.Checks)
		}
	}
}

func TestParseLeavesNamesAndSexOutsideChecksumProtection(t *testing.T) {
	lines := nationalFixture("900101")
	lines[0] = replaceByte(lines[0], 5, 'J')
	lines[1] = replaceByte(lines[1], 20, 'F')
	result := Parse(lines)
	if result.Status != "valid" {
		t.Fatalf("name and sex changes unexpectedly failed the MRZ checks: %+v", result)
	}
	if result.Fields.Gender != "F" || result.Fields.Surname != "ЖВАНОВ" || result.NamesChecksumProtected {
		t.Fatalf("changed fields or protection flag incorrect: %+v", result)
	}
}

func TestAuxiliaryChecksumCoversIssueDateDepartmentAndFinalSeriesDigit(t *testing.T) {
	for _, position := range []int{28, 29, 35} {
		lines := nationalFixture("900101")
		lines[1] = replaceDigit(lines[1], position)
		result := Parse(lines)
		if result.Status != "invalid" || result.Checks[2].Status != "invalid" {
			t.Errorf("auxiliary mutation at byte %d was not detected: %+v", position, result)
		}
	}
}

func TestPosition28MustBeFiller(t *testing.T) {
	lines := nationalFixture("900101")
	lines[1] = replaceByte(lines[1], 27, '0')
	// Recompute the composite check so this test exercises the format rule,
	// independently of the additional composite-check failure.
	lines[1] = replaceByte(lines[1], 43, referenceCheckDigit(compositeData(lines[1])))
	result := Parse(lines)
	if result.Status != "invalid" || !hasIssue(result.Issues, "position_28_filler_invalid") {
		t.Fatalf("position 28 value 0 was not rejected: %+v", result)
	}
}

func TestPartialBirthDateAndVIZResolution(t *testing.T) {
	lines := nationalFixture("900000")
	result := Parse(lines)
	if result.Status != "valid" || result.Fields.BirthDate.MonthKnown || result.Fields.BirthDate.DayKnown || result.Fields.BirthDate.Raw != "900000" {
		t.Fatalf("partial DOB was not preserved: %+v", result)
	}
	if got, ok := ResolveDate(result.Fields.BirthDate, "1990-05-03"); !ok || got != "1990-05-03" {
		t.Fatalf("full VIZ date matching known YY was not accepted: %q, %v", got, ok)
	}
	if _, ok := ResolveDate(result.Fields.BirthDate, "1991-05-03"); ok {
		t.Fatal("ResolveDate accepted a VIZ date with a different YY")
	}

	monthKnown := nationalFixture("900500")
	monthResult := Parse(monthKnown)
	if got, ok := ResolveDate(monthResult.Fields.BirthDate, "1990-05-03"); !ok || got != "1990-05-03" {
		t.Fatalf("known month with unknown day was not accepted: %q, %v", got, ok)
	}
	if _, ok := ResolveDate(monthResult.Fields.BirthDate, "1990-06-03"); ok {
		t.Fatal("ResolveDate accepted a VIZ date with a different known month")
	}

	fullResult := Parse(nationalFixture("900101"))
	if got, ok := ResolveDate(fullResult.Fields.BirthDate, "1990-01-01"); !ok || got != "1990-01-01" {
		t.Fatalf("matching full VIZ date not accepted: %q, %v", got, ok)
	}
	if _, ok := ResolveDate(fullResult.Fields.BirthDate, "1990-01-02"); ok {
		t.Fatal("ResolveDate accepted a VIZ date with a different known day")
	}
	if _, ok := ResolveDate(fullResult.Fields.BirthDate, "1990-02-30"); ok {
		t.Fatal("ResolveDate accepted an invalid full VIZ date")
	}
	inconsistent := fullResult.Fields.BirthDate
	inconsistent.DayKnown = false
	if _, ok := ResolveDate(inconsistent, "1990-01-01"); ok {
		t.Fatal("ResolveDate accepted DateParts whose known flag contradicts raw data")
	}
	partial := Parse(nationalFixture("900000")).Fields.BirthDate
	partial.YearTwoDigits = 19
	if _, ok := ResolveDate(partial, "2019-05-03"); ok {
		t.Fatal("ResolveDate accepted YearTwoDigits that contradicts raw YY")
	}
}

func TestParseStatusForIncompleteMalformedAndUnsupportedProfile(t *testing.T) {
	if result := Parse([2]string{}); result.Status != "indeterminate" || !hasIssue(result.Issues, "lines_missing") {
		t.Fatalf("absent lines were not left indeterminate: %+v", result)
	}
	lines := nationalFixture("900101")
	short := lines
	short[1] = short[1][:43]
	if result := Parse(short); result.Status != "indeterminate" || len(result.Checks) != 4 || !hasIndeterminateCheck(result.Checks) {
		t.Fatalf("short row status = %q; want indeterminate", result.Status)
	}
	long := lines
	long[1] += "0"
	if result := Parse(long); result.Status != "invalid" {
		t.Fatalf("long row status = %q; want invalid", result.Status)
	}
	badChar := lines
	badChar[0] = replaceByte(badChar[0], 0, 'p')
	if result := Parse(badChar); result.Status != "invalid" {
		t.Fatalf("lowercase row status = %q; want invalid", result.Status)
	}
	otherProfile := lines
	otherProfile[0] = replaceByte(otherProfile[0], 0, 'V')
	if result := Parse(otherProfile); result.Status != "indeterminate" || !hasIssue(result.Issues, "profile_not_supported") {
		t.Fatalf("other profile was not left indeterminate: %+v", result)
	}
}

func TestNumericFieldsRejectLettersEvenWhenCheckDigitCanEncodeThem(t *testing.T) {
	lines := nationalFixture("900101")
	lines[1] = replaceByte(lines[1], 0, 'A')
	lines[1] = replaceByte(lines[1], 9, referenceCheckDigit(lines[1][:9]))
	lines[1] = replaceByte(lines[1], 43, referenceCheckDigit(compositeData(lines[1])))
	result := Parse(lines)
	if result.Status != "invalid" || !hasIssue(result.Issues, "document_number_format_invalid") {
		t.Fatalf("alphabetic document number was accepted: %+v", result)
	}
}

func TestParseRejectsImpossibleDates(t *testing.T) {
	for _, dob := range []string{"901301", "900231", "910229"} {
		result := Parse(nationalFixture(dob))
		if result.Status != "invalid" || !hasIssue(result.Issues, "birth_date_invalid") {
			t.Errorf("DOB %q was not rejected: %+v", dob, result)
		}
	}
	// YY=00 is century-ambiguous but a February 29 date is possible in a
	// Gregorian year ending in 00 (for example, 2000), so do not reject it.
	if result := Parse(nationalFixture("000229")); result.Status != "valid" {
		t.Fatalf("century-ambiguous possible leap date rejected: %+v", result)
	}
}

func TestNameTruncationAndCompoundSeparatorsAreReported(t *testing.T) {
	lines := nationalFixture("900101")
	lines[0] = "PNRUS" + "IVANOV<<IVAN" + strings.Repeat("A", 39-len("IVANOV<<IVAN"))
	result := Parse(lines)
	if !hasIssue(result.NameIssues, "name_may_be_truncated") || result.NamesComplete {
		t.Fatalf("name capacity boundary not reported: %+v", result)
	}

	lines = nationalFixture("900101")
	lines[0] = "PNRUS" + paddedUpper("IVAN<OV<<IVAN<IVAN<OVICH")
	result = Parse(lines)
	if !hasIssue(result.NameIssues, "compound_surname_ambiguous") || !hasIssue(result.NameIssues, "compound_name_ambiguous") || result.NamesComplete {
		t.Fatalf("compound-name ambiguity not reported: %+v", result)
	}
}

func TestInvalidNameCodeCannotBeComplete(t *testing.T) {
	lines := nationalFixture("900101")
	lines[0] = "PNRUS" + paddedUpper("A0<<IVAN<IVANOVI3")
	result := Parse(lines)
	if result.Status != "invalid" || !hasIssue(result.NameIssues, "name_character_invalid") || result.NamesComplete {
		t.Fatalf("invalid name code was not marked incomplete: %+v", result)
	}
	if result.Fields.Surname != "" {
		t.Fatalf("failed surname decode unexpectedly produced a value: %+v", result.Fields)
	}
}

func nationalFixture(birthDate string) [2]string {
	name := paddedUpper("IVANOV<<IVAN<IVANOVI3")
	document := "123456789"
	line := document + string(referenceCheckDigit(document)) + "RUS" + birthDate + string(referenceCheckDigit(birthDate)) + "M" + "<<<<<<<" + "4" + "230101" + "123456" + "<"
	line += string(referenceCheckDigit(line[28:42]))
	composite := compositeData(line)
	line += string(referenceCheckDigit(composite))
	return [2]string{"PNRUS" + name, line}
}

func paddedUpper(name string) string { return name + strings.Repeat("<", 39-len(name)) }

// This deliberately separate test-only implementation provides complete
// literal fixtures while CheckDigit is checked against published examples.
func referenceCheckDigit(data string) byte {
	weights := [...]int{7, 3, 1}
	sum := 0
	for i := 0; i < len(data); i++ {
		value, _ := mrzValue(data[i])
		sum += value * weights[i%3]
	}
	return byte('0' + sum%10)
}

func compositeData(line string) string { return line[:10] + line[13:20] + line[21:28] + line[28:43] }

func replaceDigit(value string, position int) string {
	if value[position] == '9' {
		return replaceByte(value, position, '0')
	}
	return replaceByte(value, position, value[position]+1)
}

func replaceByte(value string, position int, replacement byte) string {
	return value[:position] + string(replacement) + value[position+1:]
}

func checkIndex(position int) int {
	switch position {
	case 9:
		return 0
	case 19:
		return 1
	case 42:
		return 2
	default:
		return 3
	}
}
