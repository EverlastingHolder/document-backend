package passport

import (
	"reflect"
	"strings"
	"testing"

	nationalmrz "passport-backend/internal/mrz"
)

func TestV1AndOffRemainCompatible(t *testing.T) {
	input, _ := mrzTestObservation(t, false)
	input.SchemaVersion = ObservationsSchema
	result, err := Extract(input, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.MRZ != nil {
		t.Fatal("default off policy emitted MRZ diagnostics")
	}
	if result.ReviewRequired != true {
		t.Fatal("review gate must remain enabled")
	}
}

func TestMRZPassDoesNotPolluteVIZExtraction(t *testing.T) {
	input, pair := mrzTestObservation(t, true)
	input.Lines = append(input.Lines,
		OCRLine{ID: "line-surname", PassID: "original", TextRaw: "ФАМИЛИЯ: ПЕТРОВ", Score: 0.99, Quad: testQuad(500, 200)},
	)
	// The MRZ-only digit strings must not be consumed by the existing visual serial fallback.
	result, err := Extract(input, Options{PassSet: "all", MRZPolicy: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fields["number"].Value != nil {
		t.Fatal("MRZ pass polluted VIZ serial extraction")
	}
	if parsed := nationalmrz.Parse(pair); parsed.Status != "valid" {
		t.Fatalf("fixture status %q", parsed.Status)
	}
}

func TestValidMRZOffersChecksumProtectedFieldsAsUncertain(t *testing.T) {
	input, pair := mrzTestObservation(t, true)
	result, err := Extract(input, Options{PassSet: "all", MRZPolicy: "augment"})
	if err != nil {
		t.Fatal(err)
	}
	if result.MRZ == nil || result.MRZ.Status != "valid" {
		t.Fatalf("MRZ status = %#v", result.MRZ)
	}
	for field, want := range map[string]string{"series": "1233", "number": "456789", "department_code": "770-001", "gender": "M"} {
		got := result.Fields[field]
		if got.Value == nil || *got.Value != want || got.State != "uncertain" {
			t.Errorf("%s = %#v, want uncertain %q", field, got, want)
		}
	}
	if result.Fields["surname"].Value == nil || *result.Fields["surname"].Value != "ИВАНОВ" {
		t.Errorf("surname offer missing: %#v", result.Fields["surname"])
	}
	if result.ReviewRequired != true || len(pair[0]) != 44 {
		t.Fatal("unexpected review or MRZ fixture shape")
	}
}

func TestInvalidChecksDoNotOfferFields(t *testing.T) {
	input, pair := mrzTestObservation(t, true)
	pair[1] = pair[1][:9] + "0" + pair[1][10:]
	input.Lines = mrzTestLines(pair)
	result, err := Extract(input, Options{PassSet: "all", MRZPolicy: "augment"})
	if err != nil {
		t.Fatal(err)
	}
	if result.MRZ == nil || result.MRZ.Status != "invalid" {
		t.Fatalf("invalid checksum was not diagnosed: %#v", result.MRZ)
	}
	if result.Fields["number"].Value != nil {
		t.Fatal("invalid MRZ offered a number")
	}
}

func TestMRZAndVIZConflictAbstains(t *testing.T) {
	input, _ := mrzTestObservation(t, true)
	input.Lines = append(input.Lines, OCRLine{ID: "viz-number", PassID: "original", TextRaw: "123456", Score: 0.99, Quad: testQuad(900, 400)})
	fields := map[string]FieldResult{"number": {Value: stringPointer("111111"), State: "uncertain", Candidates: []Candidate{{Value: "111111", Raw: "111111"}}}}
	candidate := MRZPairCandidate{LineIDs: []string{"mrz-a", "mrz-b"}, PassIDs: []string{"mrz"}, Result: nationalmrz.Parse(validMRZPair())}
	mergeMRZFields(fields, []MRZPairCandidate{candidate})
	if fields["number"].Value != nil || fields["number"].State != "uncertain" || !hasIssue(fields["number"].Issues, "mrz_field_conflict") {
		t.Fatalf("conflict did not abstain: %#v", fields["number"])
	}
}

func TestMRZDoesNotChooseAmongUnresolvedVIZCandidates(t *testing.T) {
	fields := map[string]FieldResult{
		"number": {
			Value: nil, State: "uncertain",
			Candidates: []Candidate{{Value: "111111"}, {Value: "222222"}},
			Issues:     []string{"conflicting_field_candidates"},
		},
	}
	candidate := MRZPairCandidate{LineIDs: []string{"mrz-a", "mrz-b"}, PassIDs: []string{"mrz"}, Result: nationalmrz.Parse(validMRZPair())}
	mergeMRZFields(fields, []MRZPairCandidate{candidate})
	if fields["number"].Value != nil || fields["number"].State != "uncertain" {
		t.Fatalf("MRZ chose among unresolved VIZ candidates: %#v", fields["number"])
	}
	if len(fields["number"].Candidates) != 3 {
		t.Fatalf("VIZ and MRZ alternatives were not preserved: %#v", fields["number"].Candidates)
	}
}

func TestMRZDatesNeedFullVIZCentury(t *testing.T) {
	result := nationalmrz.Parse(validMRZPair())
	fields := map[string]FieldResult{"birth_date": emptyField(), "issue_date": emptyField()}
	candidate := MRZPairCandidate{Result: result}
	mergeMRZFields(fields, []MRZPairCandidate{candidate})
	if fields["birth_date"].Value != nil || !hasIssue(fields["birth_date"].Issues, "mrz_century_unresolved") {
		t.Fatalf("date century was guessed: %#v", fields["birth_date"])
	}
	if len(fields["birth_date"].Candidates) != 1 || fields["birth_date"].Candidates[0].Value != "900101" {
		t.Fatalf("partial MRZ date metadata not preserved: %#v", fields["birth_date"].Candidates)
	}
	fields = map[string]FieldResult{"birth_date": {Value: stringPointer("1990-01-01"), State: "uncertain"}, "issue_date": emptyField()}
	mergeMRZFields(fields, []MRZPairCandidate{candidate})
	if fields["birth_date"].Value == nil || *fields["birth_date"].Value != "1990-01-01" {
		t.Fatalf("agreeing VIZ date not preserved: %#v", fields["birth_date"])
	}
	partialResult := nationalmrz.Parse(validMRZPairWithBirth("900000"))
	if partialResult.Status != "valid" {
		t.Fatalf("partial date fixture status %q", partialResult.Status)
	}
	fields = map[string]FieldResult{"birth_date": {Value: stringPointer("1990-05-03"), State: "recognized"}, "issue_date": emptyField()}
	mergeMRZFields(fields, []MRZPairCandidate{{Result: partialResult}})
	if fields["birth_date"].Value == nil || *fields["birth_date"].Value != "1990-05-03" || hasIssue(fields["birth_date"].Issues, "mrz_viz_date_conflict") {
		t.Fatalf("compatible partial date incorrectly conflicted with VIZ: %#v", fields["birth_date"])
	}
	fields = map[string]FieldResult{"birth_date": {Value: stringPointer("1991-05-03"), State: "recognized"}, "issue_date": emptyField()}
	mergeMRZFields(fields, []MRZPairCandidate{{Result: partialResult}})
	if fields["birth_date"].Value != nil || !hasIssue(fields["birth_date"].Issues, "mrz_viz_date_conflict") {
		t.Fatalf("contradictory partial MRZ date did not abstain: %#v", fields["birth_date"])
	}
}

func TestEquivalentReaderPairsRetainRawVariantsAndEvidence(t *testing.T) {
	input, pair := mrzTestObservation(t, true)
	input.Passes = append(input.Passes, OCRPass{ID: "mrz-tesseract", Kind: "mrz", Crop: Rect{100, 700, 900, 950}, Status: "ok", Reader: "tesseract-eng"})
	spaced := [2]string{pair[0][:22] + " " + pair[0][22:], pair[1][:22] + " " + pair[1][22:]}
	input.Lines = append(input.Lines,
		OCRLine{ID: "tess-line-1", PassID: "mrz-tesseract", TextRaw: spaced[0], Score: 0.8, Quad: testQuad(500, 800)},
		OCRLine{ID: "tess-line-2", PassID: "mrz-tesseract", TextRaw: spaced[1], Score: 0.8, Quad: testQuad(500, 830)},
	)
	fields := map[string]FieldResult{}
	diagnostics := collectAndMergeMRZ(input, fields)
	if len(diagnostics.Candidates) != 1 || diagnostics.Status != "valid" {
		t.Fatalf("visually equivalent readers should produce one candidate: %#v", diagnostics)
	}
	candidate := diagnostics.Candidates[0]
	if len(candidate.RawVariants) != 2 || len(candidate.LineIDs) != 4 || len(candidate.PassIDs) != 2 {
		t.Fatalf("duplicate pair lost raw variants or source evidence: %#v", candidate)
	}
	if candidate.RawLines != pair || candidate.RawVariants[1] != spaced {
		t.Fatalf("raw OCR variants were normalized or reordered: %#v", candidate.RawVariants)
	}
	if got := fields["number"].Candidates; len(got) != 1 || len(got[0].RawVariants) != 2 {
		t.Fatalf("field evidence lost duplicate-reader raw variants: %#v", got)
	}
	repeated := collectAndMergeMRZ(input, map[string]FieldResult{})
	if !reflect.DeepEqual(candidate.RawVariants, repeated.Candidates[0].RawVariants) {
		t.Fatalf("candidate order varied across identical observations: %#v vs %#v", candidate.RawVariants, repeated.Candidates[0].RawVariants)
	}
}

func TestCanonicalMRZAgreementPreservesLeadingZeroes(t *testing.T) {
	if !sameCanonicalFieldValue("gender", "M", "МУЖ.") {
		t.Fatal("canonical gender spellings should agree")
	}
	if !sameCanonicalFieldValue("department_code", "770001", "770-001") {
		t.Fatal("six-digit and dashed department codes should agree")
	}
	if sameCanonicalFieldValue("series", "0012", "12") || sameCanonicalFieldValue("number", "000123", "123") {
		t.Fatal("numeric identifiers must preserve leading zeroes")
	}
}

func TestMRZTruncationAndVisualCharacterMapping(t *testing.T) {
	if got := normalizeMRZVisual(" вё/ 123 "); got != "" {
		t.Fatalf("unsupported punctuation should not be normalized: %q", got)
	}
	if got := normalizeMRZVisual("В"); got != "B" {
		t.Fatalf("visual Cyrillic mapping = %q, want B", got)
	}
	if got := normalizeMRZVisual("O0"); got != "O0" {
		t.Fatalf("O/0 must remain distinct, got %q", got)
	}
	pair := validMRZPair()
	pair[0] = pair[0][:43]
	result := nationalmrz.Parse(pair)
	if result.Status != "indeterminate" {
		t.Fatalf("truncated pair status=%q", result.Status)
	}
	if _, ok := nationalmrz.ResolveDate(result.Fields.BirthDate, "1990-01-01"); ok {
		t.Fatal("truncated MRZ date resolved")
	}
}

func TestV1RejectsMRZReaderMetadata(t *testing.T) {
	input, _ := mrzTestObservation(t, false)
	input.Passes[0].Reader = "rapidocr"
	if err := input.Validate(); err == nil {
		t.Fatal("v1 accepted v2 MRZ reader metadata")
	}
}

func TestV1RejectsMRZPassKind(t *testing.T) {
	input, _ := mrzTestObservation(t, false)
	input.Passes[0].Kind = "mrz"
	if err := input.Validate(); err == nil {
		t.Fatal("v1 accepted the v2-only MRZ pass kind")
	}
}

func mrzTestObservation(t *testing.T, withMRZ bool) (Observations, [2]string) {
	t.Helper()
	pair := validMRZPair()
	version := ObservationsSchema
	passes := []OCRPass{{ID: "original", Kind: "original", Crop: Rect{0, 0, 1000, 1000}, Status: "ok"}}
	lines := []OCRLine{}
	if withMRZ {
		version = ObservationsSchemaV2
		passes = append(passes, OCRPass{ID: "mrz", Kind: "mrz", Crop: Rect{100, 700, 900, 950}, Status: "ok", Reader: "rapidocr"})
		lines = mrzTestLines(pair)
	}
	input := Observations{SchemaVersion: version, Image: ImageObservation{ID: "neutral-test", Width: 1000, Height: 1000, SHA256: strings.Repeat("a", 64), CoordinateFrame: "exif_normalized_pixels"}, Document: DocumentBounds{Bounds: Rect{0, 0, 1000, 1000}, Method: "contour", Quality: "candidate"}, Passes: passes, Lines: lines}
	return input, pair
}

func mrzTestLines(pair [2]string) []OCRLine {
	return []OCRLine{
		{ID: "mrz-line-1", PassID: "mrz", TextRaw: pair[0], Score: 0.8, Quad: testQuad(500, 800)},
		{ID: "mrz-line-2", PassID: "mrz", TextRaw: pair[1], Score: 0.8, Quad: testQuad(500, 830)},
	}
}

func testQuad(x, y float64) Quad {
	return Quad{{x - 100, y - 5}, {x + 100, y - 5}, {x + 100, y + 5}, {x - 100, y + 5}}
}
func stringPointer(value string) *string { return &value }

func validMRZPair() [2]string {
	return validMRZPairWithBirth("900101")
}

func validMRZPairWithBirth(birth string) [2]string {
	name := "IVANOV<<IVAN<PETR"
	upper := "PNRUS" + name + strings.Repeat("<", 39-len(name))
	document := "123456789"
	documentCheck, _ := nationalmrz.CheckDigit(document)
	birthCheck, _ := nationalmrz.CheckDigit(birth)
	auxiliary := "3" + "210515" + "770001"
	auxiliaryCheck, _ := nationalmrz.CheckDigit(auxiliary + "<")
	lower := document + string(documentCheck) + "RUS" + birth + string(birthCheck) + "M" + "<<<<<<<" + auxiliary + "<" + string(auxiliaryCheck)
	composite := lower[:10] + lower[13:20] + lower[21:28] + lower[28:43]
	compositeCheck, _ := nationalmrz.CheckDigit(composite)
	lower += string(compositeCheck)
	return [2]string{upper, lower}
}
