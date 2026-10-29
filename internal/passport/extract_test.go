package passport

import (
	"strings"
	"testing"
)

func syntheticObservations() Observations {
	seam := 500.0
	identity := Matrix3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	return Observations{
		SchemaVersion: ObservationsSchema,
		Image:         ImageObservation{ID: "synthetic-neutral", Width: 1000, Height: 1000, SHA256: strings.Repeat("a", 64), CoordinateFrame: "exif_normalized_pixels"},
		Document:      DocumentBounds{Bounds: Rect{0, 0, 1000, 1000}, Method: "contour", Quality: "candidate", SeamY: &seam},
		Passes:        []OCRPass{{ID: "original-1", Kind: "original", Crop: Rect{0, 0, 1000, 1000}, TransformToImage: identity, Status: "ok"}},
	}
}

func addLine(input *Observations, id, text string, x, y float64) {
	input.Lines = append(input.Lines, OCRLine{
		ID: id, PassID: "original-1", TextRaw: text, Score: 0.99,
		Quad: Quad{{x, y}, {x + 60, y}, {x + 60, y + 10}, {x, y + 10}},
	})
}

func addWideLine(input *Observations, id, text string, x0, y0, x1, y1 float64) {
	input.Lines = append(input.Lines, OCRLine{
		ID: id, PassID: "original-1", TextRaw: text, Score: 0.99,
		Quad: Quad{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}},
	})
}

func extractSynthetic(t *testing.T, input Observations, options Options) Extraction {
	t.Helper()
	result, err := Extract(input, options)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if !result.ReviewRequired || len(result.Fields) != 11 {
		t.Fatalf("expected 11 review-required fields, got %d", len(result.Fields))
	}
	return result
}

func hasRule(values []string, rule string) bool {
	for _, value := range values {
		if value == rule {
			return true
		}
	}
	return false
}

func TestJoinedTenDigitSeriesNumberPreservesLeadingZeroes(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "line-code", "0123456789", 820, 360)
	result := extractSynthetic(t, input, Options{})
	if got := result.Fields["series"].Value; got == nil || *got != "0123" {
		t.Fatalf("series = %v, want 0123", got)
	}
	if got := result.Fields["number"].Value; got == nil || *got != "456789" {
		t.Fatalf("number = %v, want 456789", got)
	}
}

func TestLowerPageRightMarginTenDigitRunIsReviewOnly(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "lower-serial", "0012345678", 820, 620)
	result := extractSynthetic(t, input, Options{})
	for _, field := range []string{"series", "number"} {
		got := result.Fields[field]
		if got.State != "uncertain" || got.Value == nil || !hasRule(got.Rules, "split_exact_4_plus_6_digits") || len(got.Evidence.LineIDs) != 1 {
			t.Fatalf("%s should retain lower-page serial evidence as a review proposal", field)
		}
	}
	if got := result.Fields["series"].Value; got == nil || *got != "0012" {
		t.Fatal("leading zeroes in lower-page series must be preserved")
	}
	if got := result.Fields["number"].Value; got == nil || *got != "345678" {
		t.Fatal("lower-page number split must preserve exact six-digit suffix")
	}
}

func TestSingleSixDigitVerticalOriginalNumberIsIndependentOfSeries(t *testing.T) {
	input := syntheticObservations()
	addWideLine(&input, "single-number", "000456", 820, 600, 830, 660)
	result := extractSynthetic(t, input, Options{})
	number := result.Fields["number"]
	if number.State != "uncertain" || number.Value == nil || *number.Value != "000456" || len(number.Evidence.LineIDs) != 1 {
		t.Fatal("unique lower-right six-digit run should be a review-only number proposal")
	}
	series := result.Fields["series"]
	if series.State != "missing" || series.Value != nil {
		t.Fatal("number proposal must not invent or require a series")
	}
}

func TestSerialConflictsAbstainAndDuplicateEvidenceMerges(t *testing.T) {
	for _, test := range []struct {
		name      string
		second    string
		wantCount int
		wantValue bool
	}{
		{name: "same run merges", second: "0012345678", wantCount: 1, wantValue: true},
		{name: "different runs abstain", second: "0098765432", wantCount: 2, wantValue: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := syntheticObservations()
			addLine(&input, "serial-a", "0012345678", 820, 620)
			addLine(&input, "serial-b", test.second, 830, 650)
			field := extractSynthetic(t, input, Options{}).Fields["number"]
			if len(field.Candidates) != test.wantCount || (field.Value != nil) != test.wantValue {
				t.Fatal("serial candidates were not deduplicated or conflict-abstained")
			}
			if test.wantValue && len(field.Evidence.LineIDs) != 2 {
				t.Fatal("duplicate serial evidence must be retained")
			}
		})
	}
}

func TestSingleSixDigitRightMarginPassCanProposeNumber(t *testing.T) {
	input := syntheticObservations()
	input.Passes = append(input.Passes, OCRPass{
		ID: "right-margin-cw", Kind: "right_margin_cw", Crop: Rect{700, 300, 1000, 700},
		RotationDegrees: 90, TransformToImage: Matrix3{{0, -1, 1000}, {1, 0, 300}, {0, 0, 1}}, Status: "ok",
	})
	input.Lines = append(input.Lines, OCRLine{
		ID: "rotated-number", PassID: "right-margin-cw", TextRaw: "000789", Score: 0.9,
		Quad: Quad{{820, 600}, {830, 600}, {830, 660}, {820, 660}},
	})
	field := extractSynthetic(t, input, Options{PassSet: "all"}).Fields["number"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "000789" ||
		len(field.Evidence.PassIDs) != 1 || field.Evidence.PassIDs[0] != "right-margin-cw" {
		t.Fatal("right-margin pass should preserve a single six-digit number proposal and provenance")
	}
}

func TestUpperIssueDateSixDigitCodeIsDepartmentNotNumber(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 400)
	addLine(&input, "upper-code", "000456", 820, 410)
	result := extractSynthetic(t, input, Options{})
	if result.Fields["number"].State != "missing" || result.Fields["number"].Value != nil {
		t.Fatal("upper issue-date code must not be stolen as a serial number")
	}
	if got := result.Fields["department_code"].Value; got == nil || *got != "000-456" {
		t.Fatal("upper issue-date code should remain a department-code proposal")
	}
}

func TestNineAndElevenDigitRunsAreNotTrimmedOrInvented(t *testing.T) {
	for _, raw := range []string{"123456789", "01234567890"} {
		t.Run(raw, func(t *testing.T) {
			input := syntheticObservations()
			addLine(&input, "line-code", raw, 820, 360)
			result := extractSynthetic(t, input, Options{})
			if result.Fields["series"].State != "missing" || result.Fields["number"].State != "missing" {
				t.Fatal("malformed numeric run must not be coerced")
			}
		})
	}
}

func TestAdjacentFourAndSixDigitRecordsCombineOnlyInRightMargin(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "series-part", "0123", 840, 340)
	addLine(&input, "number-part", "004567", 842, 365)
	result := extractSynthetic(t, input, Options{})
	if got := result.Fields["series"].Value; got == nil || *got != "0123" {
		t.Fatalf("series = %v, want 0123", got)
	}
	if got := result.Fields["number"].Value; got == nil || *got != "004567" {
		t.Fatalf("number = %v, want 004567", got)
	}
	if len(result.Fields["series"].Candidates) != 1 || !hasRule(result.Fields["series"].Candidates[0].Rules, "join_adjacent_4_digit_series_and_6_digit_number") {
		t.Fatal("serial split rule must be retained")
	}
}

func TestAdjacentTwoTwoAndSixDigitRecordsCombineInSamePass(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "series-part-a", "00", 840, 340)
	addLine(&input, "series-part-b", "12", 842, 365)
	addLine(&input, "number-part", "004567", 844, 390)
	result := extractSynthetic(t, input, Options{})
	if got := result.Fields["series"].Value; got == nil || *got != "0012" {
		t.Fatal("two two-digit rows should retain series leading zeroes")
	}
	if got := result.Fields["number"].Value; got == nil || *got != "004567" {
		t.Fatal("six-digit row should retain number leading zeroes")
	}
	if !hasRule(result.Fields["series"].Rules, "join_adjacent_two_two_six_digit_records") {
		t.Fatal("2+2+6 layout association rule must be recorded")
	}
}

func TestExplicitSerialLabelWinsOverLayoutFallback(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "series-label", "Серия: 0123", 300, 350)
	addLine(&input, "layout-serial", "9999888877", 820, 360)
	field := extractSynthetic(t, input, Options{}).Fields["series"]
	if field.Value == nil || *field.Value != "0123" || field.State != "recognized" || field.Context != "explicit_label:series" {
		t.Fatal("explicit serial label should take precedence over layout candidates")
	}
}

func TestDateRoleUsesLabelsAndRejectsInvalidCalendarDate(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-label", "Дата рождения:", 100, 610)
	addLine(&input, "birth-value", "31.02.2030", 240, 610)
	addLine(&input, "issue-label", "Дата выдачи:", 100, 400)
	addLine(&input, "issue-value", "12.03.2030", 240, 400)
	result := extractSynthetic(t, input, Options{})
	if result.Fields["birth_date"].State != "missing" || !hasIssue(result.Fields["birth_date"].Issues, "date_invalid_calendar_or_format") {
		t.Fatal("invalid birth date should be rejected")
	}
	if got := result.Fields["issue_date"].Value; got == nil || *got != "2030-03-12" {
		t.Fatalf("issue date = %v, want ISO date", got)
	}
	if !hasRule(result.Fields["issue_date"].Rules, "date_dd_mm_yyyy_to_iso8601") {
		t.Fatal("date normalization rule must be retained")
	}
}

func TestHardInvalidDateAbstainsEvenWithAnotherUniqueValidCandidate(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-label", "Дата рождения:", 100, 610)
	addLine(&input, "birth-invalid", "31.02.2030", 240, 610)
	addLine(&input, "birth-valid", "12.03.2030", 320, 610)
	result := extractSynthetic(t, input, Options{})
	field := result.Fields["birth_date"]
	if field.State != "uncertain" || field.Value != nil || !hasIssue(field.Issues, "date_invalid_calendar_or_format") {
		t.Fatal("hard-invalid competing date evidence must abstain")
	}
}

func TestDepartmentCodeRequiresLabelAndPreservesSourceRule(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "dept-label", "Код подразделения:", 100, 400)
	addLine(&input, "dept-value", "123456", 250, 400)
	result := extractSynthetic(t, input, Options{})
	field := result.Fields["department_code"]
	if field.Value == nil || *field.Value != "123-456" || !hasRule(field.Rules, "six_digit_department_code_to_3_3") {
		t.Fatal("six-digit department code should normalize only in labeled context")
	}
}

func TestUnlabeledDepartmentCodeNearIssueDateIsReviewOnly(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 400)
	addLine(&input, "layout-code", "001234", 820, 410)
	result := extractSynthetic(t, input, Options{})
	field := result.Fields["department_code"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "001-234" {
		t.Fatal("nearby exact code should be proposed with review")
	}
	if field.Raw != "001234" || len(field.Evidence.LineIDs) != 1 || field.Evidence.LineIDs[0] != "layout-code" ||
		!hasIssue(field.Issues, "department_code_layout_fallback_requires_manual_review") {
		t.Fatal("layout proposal must preserve raw text, source evidence, and review issue")
	}
}

func TestUnlabeledDashedDepartmentCodePreservesLeadingZeroes(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 400)
	addLine(&input, "layout-code", "001-234", 820, 410)
	field := extractSynthetic(t, input, Options{}).Fields["department_code"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "001-234" || field.Raw != "001-234" {
		t.Fatal("dashed department code should preserve its source form in a review proposal")
	}
}

func TestUnlabeledDepartmentCodeConflictsAbstainAndDuplicatesMerge(t *testing.T) {
	for _, test := range []struct {
		name           string
		second         string
		wantCandidates int
		wantValue      bool
	}{
		{name: "same code deduplicates", second: "123456", wantCandidates: 1, wantValue: true},
		{name: "different codes abstain", second: "654321", wantCandidates: 2, wantValue: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := syntheticObservations()
			addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 400)
			addLine(&input, "layout-code-a", "123456", 820, 405)
			addLine(&input, "layout-code-b", test.second, 830, 425)
			field := extractSynthetic(t, input, Options{}).Fields["department_code"]
			if len(field.Candidates) != test.wantCandidates || (field.Value != nil) != test.wantValue {
				t.Fatal("code candidates were not deduplicated or conflict-abstained as expected")
			}
		})
	}
}

func TestUnlabeledDepartmentCodeOutsideIssueDateRegionIsIgnored(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 400)
	addLine(&input, "far-code", "123456", 820, 650)
	addLine(&input, "left-code", "654321", 100, 405)
	field := extractSynthetic(t, input, Options{}).Fields["department_code"]
	if field.State != "missing" || len(field.Evidence.LineIDs) != 0 {
		t.Fatal("unrelated six-digit text must not fill department code")
	}
}

func TestUnlabeledGenderFallbackRequiresWholeWordNearBirthDate(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 650)
	addLine(&input, "gender-word", "МУЖСКОЙ.", 600, 640)
	result := extractSynthetic(t, input, Options{})
	field := result.Fields["gender"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "M" || field.Raw != "МУЖСКОЙ." ||
		!hasIssue(field.Issues, "gender_layout_fallback_requires_manual_review") {
		t.Fatal("whole-word gender should be a review-only layout proposal")
	}
}

func TestUnlabeledGenderFallbackAcceptsMeaningfulZoneOverlap(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 650)
	addLine(&input, "gender-word", "МУЖСКОЙ", 380, 640)
	field := extractSynthetic(t, input, Options{}).Fields["gender"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "M" ||
		field.Raw != "МУЖСКОЙ" || len(field.Evidence.LineIDs) != 1 || field.Evidence.LineIDs[0] != "gender-word" ||
		!hasIssue(field.Issues, "gender_layout_fallback_requires_manual_review") {
		t.Fatal("a whole-word token overlapping the bounded gender zone should remain a review-only proposal")
	}
}

func TestUnlabeledGenderFallbackStillRequiresNearbyBirthDate(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 650)
	addLine(&input, "gender-word", "МУЖСКОЙ", 380, 520)
	field := extractSynthetic(t, input, Options{}).Fields["gender"]
	if field.State != "missing" || field.Value != nil || len(field.Evidence.LineIDs) != 0 {
		t.Fatal("zone overlap alone must not propose gender far from the birth-date anchor")
	}
}

func TestUnlabeledGenderFallbackAbstainsOutsideZoneOrOnScalar(t *testing.T) {
	for _, raw := range []string{"МУЖСКОЙ", "M"} {
		t.Run(raw, func(t *testing.T) {
			input := syntheticObservations()
			addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 650)
			x := 600.0
			if raw == "МУЖСКОЙ" {
				x = 250 // Left of the bounded text region.
			}
			addLine(&input, "gender-word", raw, x, 640)
			field := extractSynthetic(t, input, Options{}).Fields["gender"]
			if field.State != "missing" || field.Value != nil {
				t.Fatal("out-of-zone word and unlabeled scalar must not be proposed")
			}
		})
	}
}

func TestUnlabeledGenderFallbackConflictsAbstain(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 650)
	addLine(&input, "gender-a", "МУЖ", 600, 640)
	addLine(&input, "gender-b", "ЖЕН", 610, 660)
	field := extractSynthetic(t, input, Options{}).Fields["gender"]
	if field.State != "uncertain" || field.Value != nil || len(field.Candidates) != 2 || !hasIssue(field.Issues, "conflicting_field_candidates") {
		t.Fatal("conflicting gender proposals must be retained without selecting one")
	}
}

func TestMixedScriptLabelLookalikeMatchesWithoutChangingRaw(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "gender-label", "ПOЛ: МУЖ", 450, 650)
	field := extractSynthetic(t, input, Options{}).Fields["gender"]
	if field.State != "recognized" || field.Value == nil || *field.Value != "M" || field.Raw != "ПOЛ: МУЖ" {
		t.Fatal("label matching may canonicalize known lookalikes but must preserve source text")
	}
}

func TestLabelFreeIssuerUsesBoundedAuthorityBlockAsReviewProposal(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "authority-anchor", "MВД UNIT", 300, 380)
	addLine(&input, "authority-row", "OFFICE ALPHA", 320, 400)
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 440)
	field := extractSynthetic(t, input, Options{}).Fields["issuer"]
	if field.State != "uncertain" || field.Value == nil || len(field.Evidence.LineIDs) != 2 ||
		!hasIssue(field.Issues, "issuer_layout_fallback_requires_manual_review") {
		t.Fatal("bounded authority-keyword block should be proposed with its source rows")
	}
	if field.Raw != "MВД UNIT\nOFFICE ALPHA" {
		t.Fatal("authority matching must not rewrite raw OCR rows")
	}
}

func TestLabelFreeIssuerSkipsInterleavedOffLaneText(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "authority-anchor", "МВД UNIT", 650, 380)
	addLine(&input, "off-lane-row", "UNRELATED LEFT ROW", 200, 400)
	addLine(&input, "authority-continuation", "OFFICE ALPHA", 655, 420)
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 460)
	field := extractSynthetic(t, input, Options{}).Fields["issuer"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "МВД UNIT OFFICE ALPHA" ||
		field.Raw != "МВД UNIT\nOFFICE ALPHA" || len(field.Evidence.LineIDs) != 2 ||
		field.Evidence.LineIDs[0] != "authority-anchor" || field.Evidence.LineIDs[1] != "authority-continuation" ||
		!hasIssue(field.Issues, "issuer_layout_fallback_requires_manual_review") {
		t.Fatal("issuer fallback should skip interleaved off-lane text and retain the aligned source rows")
	}
}

func TestLabelFreeIssuerDoesNotPrependAlignedTextAboveAnchor(t *testing.T) {
	input := syntheticObservations()
	addWideLine(&input, "unrelated-wide-header", "UNRELATED WIDE HEADER", 350, 340, 930, 360)
	addLine(&input, "authority-anchor", "МВД UNIT", 650, 380)
	addLine(&input, "authority-continuation", "OFFICE ALPHA", 655, 400)
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 440)
	field := extractSynthetic(t, input, Options{}).Fields["issuer"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "МВД UNIT OFFICE ALPHA" ||
		field.Raw != "МВД UNIT\nOFFICE ALPHA" || len(field.Evidence.LineIDs) != 2 ||
		field.Evidence.LineIDs[0] != "authority-anchor" || field.Evidence.LineIDs[1] != "authority-continuation" ||
		!hasIssue(field.Issues, "issuer_layout_fallback_requires_manual_review") {
		t.Fatal("unlabeled issuer fallback must start at its authority anchor and keep the aligned continuation")
	}
}

func TestLabelFreeIssuerRejectsAuthorityAfterIssueDate(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issue-date", "Дата выдачи: 12.03.2030", 200, 400)
	addLine(&input, "late-authority", "МВД UNIT", 300, 430)
	field := extractSynthetic(t, input, Options{}).Fields["issuer"]
	if field.State != "missing" || field.Value != nil {
		t.Fatal("authority-like text after issue-date row is outside the issuer fallback")
	}
}

func TestUnlabeledIssueDateAnchorsIssuerAndDepartmentFallbacks(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "authority-anchor", "МВД UNIT", 300, 380)
	addLine(&input, "authority-row", "OFFICE ALPHA", 320, 400)
	addLine(&input, "issue-date", "12.03.2030", 200, 440)
	addLine(&input, "department-code", "001234", 820, 445)
	result := extractSynthetic(t, input, Options{})
	if result.Fields["issuer"].State != "uncertain" || result.Fields["issuer"].Value == nil ||
		result.Fields["department_code"].State != "uncertain" || result.Fields["department_code"].Value == nil {
		t.Fatal("unlabeled issue-date role should anchor bounded issuer and department proposals")
	}
}

func TestLabelFreeBirthPlaceUsesAdjacentLowerPageRows(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 650)
	addLine(&input, "place-row-a", "REGION ALPHA", 500, 680)
	addLine(&input, "place-row-b", "ZONE BETA", 520, 700)
	field := extractSynthetic(t, input, Options{}).Fields["birth_place"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "REGION ALPHA ZONE BETA" ||
		len(field.Evidence.LineIDs) != 2 || !hasIssue(field.Issues, "birth_place_layout_fallback_requires_manual_review") {
		t.Fatal("adjacent lower-page text should be a bounded review proposal")
	}
}

func TestLabelFreeBirthPlaceStopsAtUnrelatedGap(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 650)
	addLine(&input, "gap-row", "REGION ALPHA", 500, 720)
	field := extractSynthetic(t, input, Options{}).Fields["birth_place"]
	if field.State != "missing" || field.Value != nil {
		t.Fatal("text beyond a geometric gap must not be proposed")
	}
}

func TestLabelFreeBirthPlaceStopsBeforeWideMRZ(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "Дата рождения: 12.03.2000", 200, 800)
	addLine(&input, "place-row-a", "REGION ALPHA", 500, 820)
	addLine(&input, "place-row-b", "ZONE BETA", 520, 845)
	addWideLine(&input, "mrz-wide", "MRZTEXT", 200, 870, 950, 880)
	field := extractSynthetic(t, input, Options{}).Fields["birth_place"]
	if field.State != "uncertain" || field.Value == nil || len(field.Evidence.LineIDs) != 2 {
		t.Fatal("birth-place block must stop before a bottom-wide MRZ row")
	}
}

func TestUnlabeledBirthDateAnchorsGenderAndBirthPlaceFallbacks(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "birth-date", "12.03.2000", 200, 650)
	addLine(&input, "gender-word", "ЖЕНСКИЙ", 600, 640)
	addLine(&input, "place-row", "REGION ALPHA", 500, 680)
	result := extractSynthetic(t, input, Options{})
	if result.Fields["gender"].State != "uncertain" || result.Fields["gender"].Value == nil ||
		result.Fields["birth_place"].State != "uncertain" || result.Fields["birth_place"].Value == nil {
		t.Fatal("unlabeled birth-date role should anchor bounded gender and birthplace proposals")
	}
}

func TestOCRBBoxEnvelopeIsAcceptedButAlwaysReviewOnly(t *testing.T) {
	input := syntheticObservations()
	input.Document.Method = "ocr_bbox_envelope"
	input.Document.Quality = "candidate"
	addLine(&input, "surname-label", "Фамилия: ALPHA", 200, 600)
	field := extractSynthetic(t, input, Options{}).Fields["surname"]
	if field.State != "uncertain" || field.Value == nil ||
		!hasIssue(field.Issues, "document_bounds_ocr_bbox_envelope_provisional_requires_review") {
		t.Fatal("OCR-envelope geometry must validate but remain provisional")
	}
}

func TestIssuerLinesAreJoinedByGeometryNotSourceOrder(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issuer-3", "UNIT Z", 220, 440)
	addLine(&input, "issuer-label", "Кем выдан:", 120, 400)
	addLine(&input, "issuer-1", "UNIT X", 220, 410)
	addLine(&input, "issuer-2", "UNIT Y", 220, 425)
	result := extractSynthetic(t, input, Options{})
	field := result.Fields["issuer"]
	if field.Value == nil || *field.Value != "UNIT X UNIT Y UNIT Z" {
		t.Fatalf("issuer = %v, want spatially ordered three-line value", field.Value)
	}
	if len(field.Evidence.LineIDs) != 4 {
		t.Fatalf("issuer evidence line count = %d, want 4", len(field.Evidence.LineIDs))
	}
}

func TestBirthPlaceBlockStaysSeparateFromIssuer(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issuer-label", "Кем выдан:", 120, 400)
	addLine(&input, "issuer-a", "OFFICE ALPHA", 220, 410)
	addLine(&input, "place-label", "Место рождения:", 120, 610)
	addLine(&input, "place-a", "REGION BETA", 220, 620)
	result := extractSynthetic(t, input, Options{})
	if got := result.Fields["issuer"].Value; got == nil || *got != "OFFICE ALPHA" {
		t.Fatalf("issuer = %v", got)
	}
	if got := result.Fields["birth_place"].Value; got == nil || *got != "REGION BETA" {
		t.Fatalf("birth place = %v", got)
	}
}

func TestKnownNameLookalikeIsProposalAndKeepsRawEvidence(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "surname-label", "Фамилия: ТEСТОВ", 200, 600)
	result := extractSynthetic(t, input, Options{NormalizeNames: true})
	field := result.Fields["surname"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "ТЕСТОВ" || len(field.Candidates) != 1 {
		t.Fatal("unique normalized proposal should be available for review")
	}
	if field.Raw != "Фамилия: ТEСТОВ" || field.Candidates[0].Value != "ТЕСТОВ" || !hasIssue(field.Issues, "normalization_proposal_requires_manual_review") {
		t.Fatal("expected raw text, separate proposal, and review issue")
	}
	if !hasIssue(field.Candidates[0].Rules, "visual_latin_lookalike_to_cyrillic") {
		t.Fatal("normalization rule must be recorded")
	}
}

func TestDeduplicatedNameProposalRetainsEveryRawVariant(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "surname-a", "Фамилия: ТEСТОВ", 200, 600)
	addLine(&input, "surname-b", "Фамилия: ТЕСТОВ", 200, 650)
	result := extractSynthetic(t, input, Options{NormalizeNames: true})
	field := result.Fields["surname"]
	if len(field.Candidates) != 1 || field.Candidates[0].Value != "ТЕСТОВ" || len(field.Candidates[0].RawVariants) != 2 {
		t.Fatal("same canonical proposal must retain both original OCR variants")
	}
}

func TestNameNormalizationDoesNotTouchIssuerText(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issuer", "Кем выдан: OFFICE ALPHA", 120, 400)
	result := extractSynthetic(t, input, Options{NormalizeNames: true})
	field := result.Fields["issuer"]
	if field.Value == nil || *field.Value != "OFFICE ALPHA" || len(field.Rules) != 1 || field.Rules[0] != "explicit_label_inline_value" {
		t.Fatal("name-only normalization must not alter issuer candidates")
	}
}

func TestUnknownNameLetterIsNotGuessed(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "surname-label", "Фамилия: ТQСТ", 200, 600)
	result := extractSynthetic(t, input, Options{NormalizeNames: true})
	field := result.Fields["surname"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "ТQСТ" || len(field.Candidates) != 1 || field.Candidates[0].Value != "ТQСТ" {
		t.Fatal("unknown Latin letter must be preserved as uncertain")
	}
}

func TestMissingOptionalPatronymicIsNull(t *testing.T) {
	result := extractSynthetic(t, syntheticObservations(), Options{})
	if result.Fields["patronymic"].Value != nil || result.Fields["patronymic"].State != "missing" {
		t.Fatal("missing optional patronymic should remain null/missing")
	}
}

func TestConflictingFieldCandidatesAbstain(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "surname-a", "Фамилия: ТЕСТОВ", 200, 600)
	addLine(&input, "surname-b", "Фамилия: ДРУГОЙ", 200, 650)
	result := extractSynthetic(t, input, Options{})
	field := result.Fields["surname"]
	if field.State != "uncertain" || field.Value != nil || len(field.Candidates) != 2 || !hasIssue(field.Issues, "conflicting_field_candidates") {
		t.Fatal("conflicting candidates must be retained without arbitrary selection")
	}
}

func TestAllPassesUseOriginalImageCoordinatesAndPreservePassEvidence(t *testing.T) {
	input := syntheticObservations()
	input.Passes = append(input.Passes, OCRPass{ID: "crop-1", Kind: "right_margin_cw", Crop: Rect{700, 200, 1000, 500}, RotationDegrees: 90, TransformToImage: Matrix3{{0, -1, 1000}, {1, 0, 200}, {0, 0, 1}}, Status: "ok"})
	input.Lines = append(input.Lines, OCRLine{ID: "line-code", PassID: "crop-1", TextRaw: "0123456789", Score: 0.8, Quad: Quad{{820, 350}, {900, 350}, {900, 360}, {820, 360}}})
	result := extractSynthetic(t, input, Options{PassSet: "all"})
	if len(result.Fields["series"].Evidence.PassIDs) != 1 || result.Fields["series"].Evidence.PassIDs[0] != "crop-1" {
		t.Fatal("crop evidence should retain pass ID with image-frame geometry")
	}
}

func TestRightMarginPassCannotConflictWithSurnameAndStillProvidesSerial(t *testing.T) {
	input := syntheticObservations()
	input.Passes = append(input.Passes, OCRPass{
		ID: "right-margin-cw", Kind: "right_margin_cw", Crop: Rect{700, 300, 1000, 700},
		RotationDegrees: 90, TransformToImage: Matrix3{{0, 1, 700}, {-1, 0, 699}, {0, 0, 1}}, Status: "ok",
	})
	addLine(&input, "original-surname", "Фамилия: ОБРАЗЕЦ АЛЬФА", 200, 600)
	input.Lines = append(input.Lines,
		OCRLine{
			ID: "margin-surname", PassID: "right-margin-cw", TextRaw: "Фамилия: ОБРАЗЕЦ БЕТА", Score: 0.9,
			Quad: Quad{{820, 600}, {980, 600}, {980, 610}, {820, 610}},
		},
		OCRLine{
			ID: "margin-serial", PassID: "right-margin-cw", TextRaw: "0012345678", Score: 0.9,
			Quad: Quad{{820, 620}, {830, 620}, {830, 680}, {820, 680}},
		},
	)
	result := extractSynthetic(t, input, Options{PassSet: "all"})
	surname := result.Fields["surname"]
	if surname.State != "recognized" || surname.Value == nil || *surname.Value != "ОБРАЗЕЦ АЛЬФА" ||
		len(surname.Candidates) != 1 || len(surname.Evidence.LineIDs) != 1 || surname.Evidence.LineIDs[0] != "original-surname" ||
		len(surname.Evidence.PassIDs) != 1 || surname.Evidence.PassIDs[0] != "original-1" || hasIssue(surname.Issues, "conflicting_field_candidates") {
		t.Fatal("right-margin text must not join semantic name candidates from the original pass")
	}
	series, number := result.Fields["series"], result.Fields["number"]
	if series.Value == nil || *series.Value != "0012" || number.Value == nil || *number.Value != "345678" ||
		len(series.Evidence.PassIDs) != 1 || series.Evidence.PassIDs[0] != "right-margin-cw" ||
		len(number.Evidence.PassIDs) != 1 || number.Evidence.PassIDs[0] != "right-margin-cw" {
		t.Fatal("right-margin numeric evidence must remain available to series and number extraction")
	}
}

func TestMRZLikeLowerPageDigitsAreIgnoredForSeriesAndNumber(t *testing.T) {
	input := syntheticObservations()
	addWideLine(&input, "lower-digits", "0123456789", 200, 900, 950, 910)
	result := extractSynthetic(t, input, Options{})
	if result.Fields["series"].State != "missing" || result.Fields["number"].State != "missing" {
		t.Fatal("lower-page/MRZ-like digits must not become serial fields")
	}
}
