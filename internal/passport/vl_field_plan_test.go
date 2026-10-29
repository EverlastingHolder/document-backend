package passport

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func vlPlanBaseline(input Observations) Extraction {
	fields := make(map[string]FieldResult, len(vlPlanFields))
	for _, field := range vlPlanFields {
		fields[field] = emptyField()
	}
	return Extraction{SchemaVersion: ExtractionSchema, ImageID: input.Image.ID, ReviewRequired: true, Fields: fields}
}

func vlPlanCandidate(input *Extraction, field string, ids ...string) {
	input.Fields[field] = FieldResult{
		State:      "uncertain",
		Candidates: []Candidate{{Value: "candidate", LineIDs: append([]string(nil), ids...)}},
		Evidence:   Evidence{LineIDs: append([]string(nil), ids...)},
	}
}

func vlPlanRegionFor(plan VLFieldPlan, field string) (VLFieldRegion, bool) {
	for _, region := range plan.Regions {
		if region.FieldID == field {
			return region, true
		}
	}
	return VLFieldRegion{}, false
}

func vlPlanSkipFor(plan VLFieldPlan, field string) (VLFieldSkip, bool) {
	for _, skipped := range plan.SkippedFields {
		if skipped.FieldID == field {
			return skipped, true
		}
	}
	return VLFieldSkip{}, false
}

func addCoherentNameRows(input *Observations, prefix string, firstY float64) []string {
	rows := []string{prefix + "-top", prefix + "-middle", prefix + "-bottom"}
	texts := []string{"АЛЬФОВ", "ИВАН", "ПЕТРОВИЧ"}
	for i := range rows {
		addLine(input, rows[i], texts[i], 620, firstY+float64(i)*25)
	}
	return rows
}

func TestBuildVLFieldPlanReusesValidatedTemplateCropAndBindsFingerprint(t *testing.T) {
	input, evidence := templateFixture()
	baseline := extractSynthetic(t, input, Options{PassSet: "all"})
	baseline, err := ApplyTemplateEvidence(input, Options{PassSet: "all"}, baseline, &evidence)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	region, ok := vlPlanRegionFor(plan, "given_name")
	if !ok || region.Recipe != "registered_template_crop/v1" || region.Crop != evidence.Regions[0].Crop {
		t.Fatalf("given_name did not reuse validated template crop: %#v", region)
	}
	if plan.SchemaVersion != VLFieldPlanSchema || plan.Image != input.Image || len(plan.Regions)+len(plan.SkippedFields) != 6 {
		t.Fatal("plan envelope or six-field partition is invalid")
	}
	fingerprint, err := VLFieldPlanSHA256(plan)
	if err != nil || len(fingerprint) != 64 {
		t.Fatalf("fingerprint = %q, err=%v", fingerprint, err)
	}
	changed := plan
	changed.Regions = append([]VLFieldRegion(nil), plan.Regions...)
	changed.Regions[0].Recipe += "/tampered"
	changedFingerprint, err := VLFieldPlanSHA256(changed)
	if err != nil || changedFingerprint == fingerprint {
		t.Fatal("plan fingerprint did not change after a material mutation")
	}
	input.Image.ID = "different-image"
	if _, err := BuildVLFieldPlan(input, baseline); err == nil {
		t.Fatal("baseline image mismatch should be rejected")
	}
}

func TestBuildVLFieldPlanDeduplicatesTemplateOutputAlternativesAndDoesNotMutateBaseline(t *testing.T) {
	input, evidence := templateFixture()
	baseline := extractSynthetic(t, input, Options{PassSet: "all"})
	baseline, err := ApplyTemplateEvidence(input, Options{PassSet: "all"}, baseline, &evidence)
	if err != nil {
		t.Fatal(err)
	}
	field := baseline.Fields["given_name"]
	if len(field.Candidates) == 0 || len(field.Candidates[0].TemplateEvidence) == 0 {
		t.Fatal("fixture did not retain template provenance")
	}
	alternative := field.Candidates[0]
	alternative.TemplateEvidence = append([]TemplateProvenance(nil), alternative.TemplateEvidence...)
	alternative.TemplateEvidence[0].Region.Records = []TemplateRecord{{TextRaw: "different reader output", Score: .3, Box: Rect{610, 635, 650, 650}, GeometryScope: "row_crop", ReaderMode: "rec_only"}}
	field.Candidates = append(field.Candidates, alternative)
	baseline.Fields["given_name"] = field

	bad := alternative
	bad.LineIDs = nil
	bad.FieldEvidence = nil
	bad.TemplateEvidence = append([]TemplateProvenance(nil), alternative.TemplateEvidence...)
	bad.TemplateEvidence[0].Region.FieldID = "surname"
	bad.TemplateEvidence[0].Region.Crop = Rect{0, 0, 1000, 1000}
	baseline.Fields["surname"] = FieldResult{State: "uncertain", Candidates: []Candidate{bad}, Evidence: Evidence{LineIDs: []string{}, PassIDs: []string{}}}

	before, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("planner mutated baseline extraction/provenance")
	}
	region, ok := vlPlanRegionFor(plan, "given_name")
	if !ok || region.Recipe != "registered_template_crop/v1" {
		t.Fatal("same-geometry row alternatives should retain the validated template crop")
	}
	if region, ok := vlPlanRegionFor(plan, "surname"); ok && region.Recipe == "registered_template_crop/v1" {
		t.Fatal("invalid template crop must not be reused; any fallback must come from source geometry")
	}
}

func TestBuildVLFieldPlanDeduplicatesDuplicateGlyphsAndAbstainsOnTwoNameRows(t *testing.T) {
	input := syntheticObservations()
	identity := Matrix3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	input.Passes = append(input.Passes, OCRPass{ID: "upper-copy", Kind: "upper_page", Crop: Rect{0, 0, 1000, 500}, TransformToImage: identity, Status: "ok"})
	rows := addCoherentNameRows(&input, "name", 600)
	duplicate := input.Lines[len(input.Lines)-1]
	duplicate.ID, duplicate.PassID = "name-bottom-copy", "upper-copy"
	input.Lines = append(input.Lines, duplicate)
	baseline := vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "given_name", rows[0], "name-bottom-copy", rows[2])
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	region, ok := vlPlanRegionFor(plan, "given_name")
	if !ok || len(region.AnchorLineIDs) != 4 {
		t.Fatalf("three coherent rows should retain deduplicated geometry provenance: %#v", region)
	}
	for range 100 {
		repeated, err := BuildVLFieldPlan(input, baseline)
		if err != nil || repeated.PlanSHA256 != plan.PlanSHA256 {
			t.Fatalf("plan was nondeterministic across repeated builds: %q != %q (%v)", repeated.PlanSHA256, plan.PlanSHA256, err)
		}
	}
	input = syntheticObservations()
	addLine(&input, "two-a", "АЛЬФОВ", 620, 600)
	addLine(&input, "two-b", "ИВАН", 620, 625)
	addLine(&input, "gender-one-letter", "Ж", 620, 650)
	baseline = vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "surname", "two-a")
	vlPlanCandidate(&baseline, "given_name", "two-b")
	vlPlanCandidate(&baseline, "patronymic", "gender-one-letter")
	plan, err = BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"surname", "given_name", "patronymic"} {
		if _, ok := vlPlanSkipFor(plan, field); !ok {
			t.Fatalf("two name rows and a single-letter gender token must abstain for %s", field)
		}
	}
}

func TestBuildVLFieldPlanPlansNeighborsWithoutMRZAndKeepsOtherFieldsIndependent(t *testing.T) {
	input := syntheticObservations()
	nameIDs := addCoherentNameRows(&input, "name", 600)
	addLine(&input, "dob-noisy", "ДАТА 01 . 02 . 1990 ГОДА", 700, 690)
	addLine(&input, "issuer-row", "ОТДЕЛ МВД", 250, 200)
	baseline := vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "surname", nameIDs[0], nameIDs[1], nameIDs[2])
	vlPlanCandidate(&baseline, "birth_date", "dob-noisy")
	vlPlanCandidate(&baseline, "issuer", "issuer-row")
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"gender", "birth_place", "issuer"} {
		if _, ok := vlPlanRegionFor(plan, field); !ok {
			t.Fatalf("field %s should be planned independently", field)
		}
	}
	if _, ok := vlPlanRegionFor(plan, "patronymic"); !ok {
		t.Fatal("three-row coherent name block should map patronymic by vertical order")
	}
	if region, _ := vlPlanRegionFor(plan, "birth_place"); region.Recipe != "birth_date_neighbor/v1" {
		t.Fatalf("birth place recipe = %q", region.Recipe)
	}
}

func TestBuildVLFieldPlanRequiresDOBAndNameSupportAndBoundsCrops(t *testing.T) {
	input := syntheticObservations()
	baseline := vlPlanBaseline(input)
	nameIDs := addCoherentNameRows(&input, "name", 600)
	vlPlanCandidate(&baseline, "surname", nameIDs[0], nameIDs[1], nameIDs[2])
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vlPlanSkipFor(plan, "gender"); !ok {
		t.Fatal("unknown DOB should not produce a DOB-neighbor crop")
	}
	addLine(&input, "dob", "01.02.1990", 700, 690)
	vlPlanCandidate(&baseline, "birth_date", "dob")
	plan, err = BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"gender", "birth_place"} {
		region, ok := vlPlanRegionFor(plan, field)
		if !ok || !validRect(region.Crop, float64(input.Image.Width), float64(input.Image.Height)) || region.Crop[0] < input.Document.Bounds[0] || region.Crop[3] > input.Document.Bounds[3] {
			t.Fatalf("invalid crop for %s: %#v", field, region)
		}
	}
	gender, _ := vlPlanRegionFor(plan, "gender")
	if gender.Crop[3]-gender.Crop[1] < 15 {
		t.Fatalf("gender neighborhood is shorter than the DOB row: %#v", gender.Crop)
	}
	if _, planned := vlPlanRegionFor(plan, "patronymic"); !planned {
		t.Fatal("patronymic should be mapped from the third coherent row")
	}
	if _, ok := vlIntegerCrop(Rect{40, 40, 20, 20}, input); ok {
		t.Fatal("an inverted crop must be rejected")
	}
}

func TestBuildVLFieldPlanGrowsUniqueAuthorityBlockAndRejectsAmbiguousBlocks(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "authority-1", "УПРАВЛЕНИЕ МВД", 250, 200)
	addLine(&input, "authority-2", "ОТДЕЛ ПО РАЙОНУ", 250, 220)
	addLine(&input, "issue-date-barrier", "ДАТА 02.03.2015", 250, 235)
	addLine(&input, "after-date-text", "ДОПОЛНИТЕЛЬНЫЙ ТЕКСТ", 250, 245)
	baseline := vlPlanBaseline(input)
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	region, ok := vlPlanRegionFor(plan, "issuer")
	if !ok || region.Recipe != "authority_text_block/v1" || len(region.AnchorLineIDs) != 2 {
		t.Fatalf("unique authority block was not planned: %#v", region)
	}
	if strings.Contains(strings.Join(region.AnchorLineIDs, ","), "after-date-text") {
		t.Fatal("authority block must stop at the issue-date barrier")
	}
	addLine(&input, "authority-other", "ОТДЕЛ МВД", 250, 300)
	plan, err = BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vlPlanSkipFor(plan, "issuer"); !ok {
		t.Fatal("multiple separated authority blocks should abstain")
	}
}

func TestBuildVLFieldPlanUsesVerticalNameRolesAndFiltersAuthorityContext(t *testing.T) {
	input := syntheticObservations()
	nameIDs := addCoherentNameRows(&input, "name", 600)
	addLine(&input, "dob", "ДАТА РОЖДЕНИЯ 01 . 02 . 1990", 700, 690)
	addLine(&input, "header", "РОССИЙСКАЯ ФЕДЕРАЦИЯ", 250, 110)
	addLine(&input, "passport-header-word", "ПАСПОРТ", 250, 125)
	addLine(&input, "authority-label-value", "ПАСПОРТ ВЫДАН ОТДЕЛОМ МВД", 250, 200)
	addLine(&input, "authority-value-row", "ПАСПОРТНО-ВИЗОВЫМ ОТДЕЛОМ", 250, 215)
	addLine(&input, "serial-context", "АБ123456", 250, 230)
	addLine(&input, "signature-context", "ЛИЧНАЯ ПОДПИСЬ", 250, 245)
	baseline := vlPlanBaseline(input)
	// Deliberately assign the wrong baseline role IDs. Only the geometry of
	// the coherent three-row block may determine the name field mapping.
	vlPlanCandidate(&baseline, "surname", nameIDs[1])
	vlPlanCandidate(&baseline, "given_name", nameIDs[0])
	vlPlanCandidate(&baseline, "patronymic", "gender-context")
	vlPlanCandidate(&baseline, "issuer", "header", "passport-header-word", "authority-label-value", "authority-value-row", "serial-context", "signature-context")
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{"surname": nameIDs[0], "given_name": nameIDs[1], "patronymic": nameIDs[2]} {
		region, ok := vlPlanRegionFor(plan, field)
		if !ok || !containsString(region.AnchorLineIDs, want) {
			t.Fatalf("%s should follow vertical name order, region=%#v", field, region)
		}
	}
	issuer, ok := vlPlanRegionFor(plan, "issuer")
	if !ok {
		t.Fatal("authority value block should remain plan-able")
	}
	for _, forbidden := range []string{"header", "passport-header-word", "serial-context", "signature-context", "dob"} {
		if containsString(issuer.AnchorLineIDs, forbidden) {
			t.Fatalf("issuer value anchors include context line %q: %#v", forbidden, issuer.AnchorLineIDs)
		}
	}
	if !containsString(issuer.AnchorLineIDs, "authority-label-value") || !containsString(issuer.AnchorLineIDs, "authority-value-row") {
		t.Fatalf("issuer plan lost authority value rows: %#v", issuer.AnchorLineIDs)
	}
	if vlIsHeaderOrCaption("ПАСПОРТНО-ВИЗОВЫМ ОТДЕЛОМ") {
		t.Fatal("passport-derived authority word was incorrectly classified as a caption")
	}
}

func TestBuildVLFieldPlanFindsSeededNameTripletWithOverlappingDOBAndExtraRow(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "fio-a", "ALPHA", 620, 600)
	addLine(&input, "fio-b", "BRAVO", 620, 655)
	addLine(&input, "fio-c", "CHARLIE", 620, 680)
	addLine(&input, "extra-aligned", "DELTA ECHO", 620, 705)
	addWideLine(&input, "dob-oversized", "01.02.1990", 500, 700, 700, 760)
	addWideLine(&input, "dob-compact", "01.02.1990", 600, 720, 700, 730)
	baseline := vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "surname", "fio-a")
	vlPlanCandidate(&baseline, "given_name", "fio-b")
	vlPlanCandidate(&baseline, "patronymic", "fio-c")
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{"surname": "fio-a", "given_name": "fio-b", "patronymic": "fio-c"} {
		region, ok := vlPlanRegionFor(plan, field)
		if !ok || !containsString(region.AnchorLineIDs, want) {
			t.Fatalf("%s should follow the best-supported three-row block: %#v", field, region)
		}
		if containsString(region.AnchorLineIDs, "extra-aligned") {
			t.Fatalf("extra aligned row must not be mistaken for a name role: %#v", region.AnchorLineIDs)
		}
	}
	surname, _ := vlPlanRegionFor(plan, "surname")
	if len(surname.AnchorLineIDs) == 0 {
		t.Fatal("name regions must retain geometry anchors")
	}
}

func TestBuildVLFieldPlanDoesNotUseGenderOrBirthplaceRowsAsPatronymic(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "fio-a", "ALPHA", 620, 600)
	addLine(&input, "fio-b", "BRAVO", 620, 655)
	addLine(&input, "dob", "01.02.1990", 620, 700)
	addLine(&input, "gender", "ЖЕНСКИЙ", 620, 725)
	addLine(&input, "place", "ГОРОД ПРИМЕР", 620, 750)
	baseline := vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "surname", "fio-a")
	vlPlanCandidate(&baseline, "given_name", "fio-b")
	vlPlanCandidate(&baseline, "birth_place", "place")
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"surname", "given_name", "patronymic"} {
		if _, ok := vlPlanRegionFor(plan, field); ok {
			t.Fatalf("gender or birthplace context must not complete a name triplet: %s", field)
		}
	}
}

func TestBuildVLFieldPlanStillAbstainsOnTwoEquallySupportedNameTriplets(t *testing.T) {
	input := syntheticObservations()
	first := addCoherentNameRows(&input, "first", 580)
	second := addCoherentNameRows(&input, "second", 760)
	baseline := vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "surname", first[0], second[0])
	vlPlanCandidate(&baseline, "given_name", first[1], second[1])
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"surname", "given_name", "patronymic"} {
		if _, ok := vlPlanSkipFor(plan, field); !ok {
			t.Fatalf("equally supported competing triplets must remain ambiguous: %s", field)
		}
	}
}

func TestBuildVLFieldPlanKeepsNameRowImmediatelyBelowPageSeam(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "upper-noise", "NOISE", 620, 480)
	addLine(&input, "fio-a", "ALPHA", 620, 520)
	addLine(&input, "fio-b", "BRAVO", 620, 560)
	addLine(&input, "fio-c", "CHARLIE", 620, 600)
	addLine(&input, "dob", "01.02.1990", 620, 650)
	baseline := vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "surname", "fio-a", "upper-noise")
	vlPlanCandidate(&baseline, "given_name", "fio-b")
	vlPlanCandidate(&baseline, "patronymic", "fio-c")
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{"surname": "fio-a", "given_name": "fio-b", "patronymic": "fio-c"} {
		region, ok := vlPlanRegionFor(plan, field)
		if !ok || !containsString(region.AnchorLineIDs, want) || containsString(region.AnchorLineIDs, "upper-noise") {
			t.Fatalf("%s should keep the below-seam name row and exclude upper-page noise: %#v", field, region)
		}
	}
}

func TestBuildVLFieldPlanExcludesIssuerHeaderAndKeepsInstitutionRows(t *testing.T) {
	input := syntheticObservations()
	addWideLine(&input, "passport-heading", "ПАСПОРТ", 100, 110, 400, 120)
	addWideLine(&input, "issuer-heading", "РОССИЙСКАЯ ФЕДЕРАЦИЯ", 100, 130, 800, 140)
	addWideLine(&input, "authority-row", "УПРАВЛЕНИЕ МВД", 300, 180, 700, 190)
	addWideLine(&input, "institution-row", "ОТДЕЛ ПО РАЙОНУ", 320, 188, 680, 198)
	addWideLine(&input, "location-row", "ГОРОД ПРИМЕР", 350, 198, 650, 208)
	addWideLine(&input, "issue-date-barrier", "ДАТА 02.03.2015", 300, 250, 700, 260)
	baseline := vlPlanBaseline(input)
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	issuer, ok := vlPlanRegionFor(plan, "issuer")
	if !ok || issuer.Recipe != "authority_text_block/v1" {
		t.Fatalf("issuer institution rows should remain plannable: %#v", issuer)
	}
	for _, forbidden := range []string{"passport-heading", "issuer-heading", "issue-date-barrier"} {
		if containsString(issuer.AnchorLineIDs, forbidden) {
			t.Fatalf("issuer anchors include header/date row %q: %#v", forbidden, issuer.AnchorLineIDs)
		}
	}
	for _, required := range []string{"authority-row", "institution-row", "location-row"} {
		if !containsString(issuer.AnchorLineIDs, required) {
			t.Fatalf("issuer block clipped a legitimate institution row %q: %#v", required, issuer.AnchorLineIDs)
		}
	}
	if issuer.Crop[1] <= 140 || issuer.Crop[3] >= 250 {
		t.Fatalf("issuer crop should exclude heading above and issue-date boundary below: %#v", issuer.Crop)
	}
}

func TestVLDateAcceptsNoisySeparatorsButRequiresCalendarDate(t *testing.T) {
	for _, value := range []string{"ДАТА 01 . 02 . 1990 ГОДА", "01 02 1990", "1990 - 02 - 01"} {
		if !vlDate(value) {
			t.Errorf("expected one valid date in %q", value)
		}
	}
	for _, value := range []string{"31.02.1990", "0101.02.1990", "01.02.1990 И 02.03.1990"} {
		if vlDate(value) {
			t.Errorf("expected no unique valid date in %q", value)
		}
	}
}

func TestBuildVLFieldPlanKeepsDOBNeighborsIndependentOfThreeNameRoleBlock(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "name-surname", "ПОЛЯКОВ", 620, 610)
	addLine(&input, "name-given", "ИМЯРЕК", 620, 635)
	addLine(&input, "dob-valid", "ДАТА 01 . 02 . 1990", 700, 690)
	baseline := vlPlanBaseline(input)
	vlPlanCandidate(&baseline, "surname", "name-surname")
	vlPlanCandidate(&baseline, "given_name", "name-given")
	vlPlanCandidate(&baseline, "birth_date", "dob-valid")
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"gender", "birth_place"} {
		region, ok := vlPlanRegionFor(plan, field)
		if !ok || region.Recipe != "birth_date_neighbor/v1" || !containsString(region.AnchorLineIDs, "dob-valid") {
			t.Fatalf("%s should use its own valid DOB anchor even without three name rows: %#v", field, region)
		}
	}
	for _, field := range []string{"surname", "given_name", "patronymic"} {
		if _, ok := vlPlanRegionFor(plan, field); ok {
			t.Fatalf("two name rows must not be guessed into three semantic roles: %s", field)
		}
	}
	for _, raw := range []string{"ПОЛЯКОВ", "ИМЯРЕК", "ФАМИЛИЯ: ПОЛЯКОВ"} {
		if !vlNameLineAllowed(vlPlanLine{line: OCRLine{TextRaw: raw}}) {
			t.Errorf("whole-word label matching rejected a name containing a field-label substring: %q", raw)
		}
		if raw != "ФАМИЛИЯ: ПОЛЯКОВ" && vlNameBoundaryLabel(raw) {
			t.Errorf("whole-word field matching treated a name as a caption: %q", raw)
		}
	}
	if vlNameBoundaryLabel("НОМЕРОВ") {
		t.Fatal("a name token sharing a field-label prefix must not become a boundary")
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestVLPlanIntegerCropUsesIntegerInteriorForFractionalDocumentBounds(t *testing.T) {
	input := syntheticObservations()
	input.Document.Bounds = Rect{10.3, 10.7, 990.4, 990.6}
	crop, ok := vlIntegerCrop(Rect{20.2, 20.1, 90.2, 80.9}, input)
	if !ok || crop != (Rect{20, 20, 91, 81}) {
		t.Fatalf("integer crop = %#v, ok=%v", crop, ok)
	}
	for _, coordinate := range crop {
		if math.Trunc(coordinate) != coordinate {
			t.Fatalf("crop coordinate is fractional: %#v", crop)
		}
	}
	if _, ok := vlIntegerCrop(Rect{10.4, 10.8, 10.6, 10.9}, input); ok {
		t.Fatal("document region without a feasible integer interior should be rejected")
	}
}

func TestVLFieldPlanSHA256StableAcrossRepeatedCalls(t *testing.T) {
	input := syntheticObservations()
	addLine(&input, "issuer", "УФМС", 250, 200)
	baseline := vlPlanBaseline(input)
	first, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		t.Fatal(err)
	}
	a, err := VLFieldPlanSHA256(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := VLFieldPlanSHA256(second)
	if err != nil || a != b {
		t.Fatalf("plan hash is not deterministic: %q != %q (%v)", a, b, err)
	}
}
