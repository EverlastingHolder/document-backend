package passport

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func supportedEvidenceFixture() (Observations, FieldEvidence) {
	input := syntheticObservations()
	addLine(&input, "name-upper", "АЛЬФА", 700, 600)
	addLine(&input, "name-middle", "ИВАН", 702, 620)
	addLine(&input, "name-lower", "БЕТА", 699, 640)
	addLine(&input, "birth-date-anchor", "01.02.2000", 700, 690)
	record := FieldEvidenceRecord{
		FieldID: "given_name", RoleQuality: "supported_row", ProposalMethod: "viz_layout_v1",
		SourcePassIDs: []string{"original-1"}, SourceLineIDs: []string{"name-middle"},
		NameRowLineIDs:  [][]string{{"name-upper"}, {"name-middle"}, {"name-lower"}},
		AdjacentAnchors: []FieldEvidenceAnchor{{Role: "birth_date", LineID: "birth-date-anchor", PassID: "original-1", Relation: "upper_row_boundary"}},
		ROIBounds:       Rect{690, 618, 820, 638}, GeometryScope: "entire_crop", Reader: "rapidocr", ReaderMode: "rec_only",
		ReaderModelSHA256: strings.Repeat("b", 64), RawText: "ИВАН", Score: 0.9,
		Quad: Quad{{690, 618}, {820, 618}, {820, 638}, {690, 638}},
	}
	evidence := FieldEvidence{SchemaVersion: FieldEvidenceSchema, Image: FieldEvidenceImage{
		SHA256: input.Image.SHA256, Width: input.Image.Width, Height: input.Image.Height, CoordinateFrame: input.Image.CoordinateFrame,
	}, Evidence: []FieldEvidenceRecord{record}}
	return input, evidence
}

func TestNilFieldEvidencePreservesBaselineJSON(t *testing.T) {
	input, _ := supportedEvidenceFixture()
	baseline, err := Extract(input, Options{})
	if err != nil {
		t.Fatal("baseline extraction failed")
	}
	withNil, err := ExtractWithFieldEvidence(input, Options{}, nil)
	if err != nil {
		t.Fatal("nil-evidence extraction failed")
	}
	a, _ := json.Marshal(baseline)
	b, _ := json.Marshal(withNil)
	if string(a) != string(b) {
		t.Fatal("nil sidecar changed baseline extraction JSON")
	}
}

func TestSupportedNameEvidenceProducesUncertainCandidateWithProvenance(t *testing.T) {
	input, evidence := supportedEvidenceFixture()
	result, err := ExtractWithFieldEvidence(input, Options{}, &evidence)
	if err != nil {
		t.Fatalf("valid sidecar rejected: %v", err)
	}
	field := result.Fields["given_name"]
	if field.State != "uncertain" || field.Value == nil || *field.Value != "ИВАН" || len(field.Candidates) != 1 || len(field.Candidates[0].FieldEvidence) != 1 {
		t.Fatal("supported ROI candidate was not retained as uncertain provenance")
	}
	if len(field.Candidates[0].LineIDs) != 1 || field.Candidates[0].LineIDs[0] != "name-middle" {
		t.Fatal("equal candidate should preserve only the baseline OCR line reference")
	}
	if field.Candidates[0].FieldEvidence[0].ReaderModelSHA256 != strings.Repeat("b", 64) {
		t.Fatal("reader hash provenance missing")
	}
}

func TestDifferentSupportedNameCandidateAbstainsAndPreservesBoth(t *testing.T) {
	input, evidence := supportedEvidenceFixture()
	evidence.Evidence[0].RawText = "ПЕТР"
	result, err := ExtractWithFieldEvidence(input, Options{}, &evidence)
	if err != nil {
		t.Fatalf("valid conflicting sidecar rejected: %v", err)
	}
	field := result.Fields["given_name"]
	if field.State != "uncertain" || field.Value != nil || len(field.Candidates) != 2 || !hasIssue(field.Issues, "conflicting_field_candidates") {
		t.Fatal("different ROI candidate must abstain while preserving both candidates")
	}
	if field.Evidence.LineIDs == nil || field.Evidence.PassIDs == nil || !hasIssue(field.Issues, "field_roi_candidate_requires_manual_review") {
		t.Fatal("conflict resolution lost baseline evidence or review issue")
	}
	for _, candidate := range field.Candidates {
		if candidate.Raw == "" || len(candidate.Rules) == 0 || candidate.Context == "" {
			t.Fatal("conflicting candidate provenance was lost")
		}
	}
}

func TestExistingConflictStaysConflictedWhenROIMatchesOneCandidate(t *testing.T) {
	input, evidence := supportedEvidenceFixture()
	roiCandidate, err := evidence.candidate("ИВАН", false)
	if err != nil {
		t.Fatal(err)
	}
	baselineCandidates := []Candidate{
		{Value: "ИВАН", Raw: "raw-a", RawVariants: []string{"raw-a"}, Rules: []string{"rule-a"}, LineIDs: []string{"a"}, PassIDs: []string{"p"}, Context: "ctx-a"},
		{Value: "ПЕТР", Raw: "raw-b", RawVariants: []string{"raw-b"}, Rules: []string{"rule-b"}, LineIDs: []string{"b"}, PassIDs: []string{"p"}, Context: "ctx-b"},
	}
	field := resolveCandidateSet(input, "given_name", append(baselineCandidates, roiCandidate), []string{"preexisting_issue"}, true)
	if field.Value != nil || field.State != "uncertain" || len(field.Candidates) != 2 || !hasIssue(field.Issues, "preexisting_issue") || !hasIssue(field.Issues, "conflicting_field_candidates") {
		t.Fatal("ROI match must not resolve an existing conflict or drop existing issues")
	}
	matched := field.Candidates[0]
	if matched.Value != "ИВАН" || len(matched.FieldEvidence) != 1 || !hasRule(matched.Rules, "rule-a") || !hasRule(matched.Rules, "supported_name_roi_candidate") || len(matched.RawVariants) < 2 {
		t.Fatal("equal-value evidence must merge without losing candidate provenance")
	}
	if len(field.Evidence.LineIDs) != 2 || field.Candidates[0].Context == "" || field.Candidates[1].Context == "" {
		t.Fatal("existing conflict line evidence or candidate context was lost")
	}
}

func TestSidecarRejectsUnsupportedWeakOrReaderModes(t *testing.T) {
	for _, mutate := range []func(*FieldEvidence){
		func(e *FieldEvidence) { e.Evidence[0].RoleQuality = "weak" },
		func(e *FieldEvidence) { e.Evidence[0].FieldID = "issuer" },
		func(e *FieldEvidence) { e.Evidence[0].ReaderMode = "det_rec" },
		func(e *FieldEvidence) { e.Evidence[0].GeometryScope = "glyph_box" },
	} {
		input, evidence := supportedEvidenceFixture()
		mutate(&evidence)
		if err := evidence.ValidateAgainst(input); err == nil {
			t.Fatal("unsupported evidence mode was accepted")
		}
	}
}

func TestSidecarRejectsImageMismatchAndDanglingReferences(t *testing.T) {
	for _, mutate := range []func(*FieldEvidence){
		func(e *FieldEvidence) { e.Image.SHA256 = strings.Repeat("c", 64) },
		func(e *FieldEvidence) { e.Image.Width++ },
		func(e *FieldEvidence) { e.Evidence[0].SourceLineIDs = []string{"missing"} },
		func(e *FieldEvidence) { e.Evidence[0].AdjacentAnchors[0].PassID = "not-original" },
		func(e *FieldEvidence) { e.Evidence[0].SourceLineIDs = []string{"name-middle", "name-middle"} },
	} {
		input, evidence := supportedEvidenceFixture()
		mutate(&evidence)
		if err := evidence.ValidateAgainst(input); err == nil {
			t.Fatal("mismatched or duplicated references were accepted")
		}
	}
}

func TestSidecarRejectsInvalidROIReaderGeometryAndNumericValues(t *testing.T) {
	for _, mutate := range []func(*FieldEvidence){
		func(e *FieldEvidence) { e.Evidence[0].ROIBounds[0] = -1 },
		func(e *FieldEvidence) { e.Evidence[0].Quad[0][0] = math.NaN() },
		func(e *FieldEvidence) { e.Evidence[0].Score = math.Inf(1) },
		func(e *FieldEvidence) { e.Evidence[0].Score = 1.1 },
		func(e *FieldEvidence) { e.Evidence[0].Quad = Quad{{690, 618}, {820, 618}, {820, 638}, {820, 638}} },
	} {
		input, evidence := supportedEvidenceFixture()
		mutate(&evidence)
		if err := evidence.ValidateAgainst(input); err == nil {
			t.Fatal("invalid ROI or geometry was accepted")
		}
	}
}

func TestSidecarRejectsIssuerRowsAndDOBOrMRZOverlap(t *testing.T) {
	input, evidence := supportedEvidenceFixture()
	input.Document.SeamY = nil
	if err := evidence.ValidateAgainst(input); err == nil {
		t.Fatal("lower-page context without seam was accepted")
	}
	input, evidence = supportedEvidenceFixture()
	evidence.Evidence[0].NameRowLineIDs = [][]string{{"name-upper"}, {"birth-date-anchor"}, {"name-lower"}}
	evidence.Evidence[0].SourceLineIDs = []string{"birth-date-anchor"}
	if err := evidence.ValidateAgainst(input); err == nil {
		t.Fatal("DOB cannot masquerade as the middle name row")
	}
	input, evidence = supportedEvidenceFixture()
	addWideLine(&input, "mrz-test", "ABCD<<<<<<<<<<<<<<", 680, 620, 840, 636)
	if err := evidence.ValidateAgainst(input); err == nil {
		t.Fatal("ROI overlapping MRZ-like line was accepted")
	}
}

func TestNameNormalizerAppliesSameWayToROIAndBaselineCandidates(t *testing.T) {
	input, evidence := supportedEvidenceFixture()
	evidence.Evidence[0].RawText = "IVAN"
	for _, enabled := range []bool{false, true} {
		candidate, err := evidence.candidate("IVAN", enabled)
		if err != nil {
			t.Fatal(err)
		}
		want, _, _ := normalizeName("IVAN", enabled)
		if candidate.Value != want {
			t.Fatal("ROI candidate did not use the configured name normalizer")
		}
		result, err := ExtractWithFieldEvidence(input, Options{NormalizeNames: enabled}, &evidence)
		if err != nil {
			t.Fatal("valid name normalizer mode rejected sidecar")
		}
		if result.Fields["given_name"].State != "uncertain" {
			t.Fatal("ROI evidence must never produce recognized state")
		}
	}
}
