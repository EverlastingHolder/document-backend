package passport

import (
	"encoding/json"
	"strings"
	"testing"
)

func vlEvidenceFixture(t *testing.T) (Observations, Options, Extraction, VLFieldPlan, VLReaderDescriptor) {
	t.Helper()
	input, template := templateFixture()
	opts := Options{PassSet: "all", NormalizeNames: true}
	base, err := Extract(input, opts)
	if err != nil {
		t.Fatal(err)
	}
	base, err = ApplyTemplateEvidence(input, opts, base, &template)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildVLFieldPlan(input, base)
	if err != nil {
		t.Fatal(err)
	}
	reader := VLReaderDescriptor{
		ModelID: vlModelID, ModelTreeSHA256: strings.Repeat("a", 64), Backend: "native", Device: "cpu",
		PipelineVersion: vlPipelineVersion, UseLayoutDetection: false, UseDocPreprocessor: false,
	}
	return input, opts, base, plan, reader
}

func vlEvidenceForPlan(plan VLFieldPlan, reader VLReaderDescriptor, values map[string]string) VLFieldEvidence {
	evidence := VLFieldEvidence{SchemaVersion: VLFieldEvidenceSchema, Image: plan.Image, PlanSHA256: plan.PlanSHA256, Reader: reader}
	for _, region := range plan.Regions {
		text := values[region.FieldID]
		if text == "" {
			text = "ГОРОД АЛЬФА"
		}
		evidence.Regions = append(evidence.Regions, VLFieldRead{
			RegionID: region.ID, Region: region, Status: "ok", Blocks: []VLFieldBlock{{TextRaw: text}},
			GeometryScope: vlGeometryScope, ElapsedSeconds: 1.25,
		})
	}
	return evidence
}

func TestVLNilEvidenceIsByteIdentical(t *testing.T) {
	input, opts, base, _, reader := vlEvidenceFixture(t)
	result, err := ApplyVLFieldEvidence(input, opts, base, nil, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(base)
	got, _ := json.Marshal(result)
	if string(got) != string(want) {
		t.Fatal("nil VL evidence changed baseline")
	}
}

func TestVLUnionAgreementMergesProvenanceAndRemainsUncertain(t *testing.T) {
	input, opts, base, plan, reader := vlEvidenceFixture(t)
	if len(plan.Regions) == 0 {
		t.Fatal("fixture did not produce VL regions")
	}
	values := map[string]string{"given_name": "ТЕСТ"}
	evidence := vlEvidenceForPlan(plan, reader, values)
	result, err := ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256})
	if err != nil {
		t.Fatal(err)
	}
	field := result.Fields["given_name"]
	if field.Value == nil || *field.Value != "ТЕСТ" || field.State != "uncertain" || len(field.Candidates) != 1 {
		t.Fatalf("agreement should remain one uncertain candidate: %#v", field)
	}
	candidate := field.Candidates[0]
	if len(candidate.VLEvidence) != 1 || len(candidate.TemplateEvidence) == 0 || candidate.Raw != "ТЕСТ" || candidate.VLEvidence[0].Read.Blocks[0].TextRaw != "ТЕСТ" {
		t.Fatal("agreement did not retain both reader provenance records and raw VL block")
	}
}

func TestVLBlocksStayInVendorOrderAndRawProvenance(t *testing.T) {
	input, opts, base, plan, reader := vlEvidenceFixture(t)
	evidence := vlEvidenceForPlan(plan, reader, nil)
	found := false
	for i := range evidence.Regions {
		if evidence.Regions[i].Region.FieldID != "given_name" {
			continue
		}
		evidence.Regions[i].Blocks = []VLFieldBlock{{TextRaw: "ТЕ"}, {TextRaw: "СТ"}}
		found = true
	}
	if !found {
		t.Fatal("fixture did not plan given_name")
	}
	result, err := ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256})
	if err != nil {
		t.Fatal(err)
	}
	field := result.Fields["given_name"]
	for _, candidate := range field.Candidates {
		if candidate.Value == "ТЕ СТ" {
			if candidate.Raw != "ТЕ СТ" || len(candidate.VLEvidence) != 1 || candidate.VLEvidence[0].Read.Blocks[0].TextRaw != "ТЕ" || candidate.VLEvidence[0].Read.Blocks[1].TextRaw != "СТ" {
				t.Fatal("VL blocks were reordered or original block text was lost")
			}
			return
		}
	}
	t.Fatal("joined VL block candidate missing")
}

func TestVLUnionConflictAbstainsAndPrimaryKeepsContradiction(t *testing.T) {
	input, opts, base, plan, reader := vlEvidenceFixture(t)
	if len(plan.Regions) == 0 {
		t.Fatal("fixture did not produce VL regions")
	}
	values := map[string]string{"given_name": "ПЕТР"}
	evidence := vlEvidenceForPlan(plan, reader, values)
	union, err := ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256, Policy: "union"})
	if err != nil {
		t.Fatal(err)
	}
	if field := union.Fields["given_name"]; field.Value != nil || field.State != "uncertain" || len(field.Candidates) < 2 {
		t.Fatalf("union conflict should abstain and preserve both candidates: %#v", field)
	}
	primary, err := ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256, Policy: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	field := primary.Fields["given_name"]
	if field.Value == nil || *field.Value != "ПЕТР" || field.State != "uncertain" || len(field.Candidates) < 2 || !hasIssue(field.Issues, "vl_viz_conflict_requires_review") {
		t.Fatalf("primary should expose VL proposal while retaining contradiction: %#v", field)
	}
}

func TestVLCapReachedAndUnknownAreReviewOnly(t *testing.T) {
	input, opts, base, plan, reader := vlEvidenceFixture(t)
	if len(plan.Regions) == 0 {
		t.Fatal("fixture did not produce VL regions")
	}
	evidence := vlEvidenceForPlan(plan, reader, nil)
	for i := range evidence.Regions {
		capReached := true
		evidence.Regions[i].CapReached = &capReached
	}
	result, err := ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256})
	if err != nil {
		t.Fatal(err)
	}
	for _, region := range plan.Regions {
		if field := result.Fields[region.FieldID]; !hasIssue(field.Issues, "vl_generation_cap_reached_no_proposal") {
			t.Fatalf("cap-reached result should be rejected for %s", region.FieldID)
		}
	}
	evidence = vlEvidenceForPlan(plan, reader, nil)
	result, err = ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256})
	if err != nil {
		t.Fatal(err)
	}
	for _, region := range plan.Regions {
		if field := result.Fields[region.FieldID]; !hasIssue(field.Issues, "vl_generation_cap_unknown_requires_review") {
			t.Fatalf("unknown cap status should be explicit for %s", region.FieldID)
		}
	}
}

func TestVLRejectsPlanReaderAndRegionTampering(t *testing.T) {
	input, opts, base, plan, reader := vlEvidenceFixture(t)
	if len(plan.Regions) == 0 {
		t.Fatal("fixture did not produce VL regions")
	}
	evidence := vlEvidenceForPlan(plan, reader, nil)
	modelOptions := VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256}
	bad := evidence
	bad.PlanSHA256 = strings.Repeat("f", 64)
	if _, err := ApplyVLFieldEvidence(input, opts, base, &bad, modelOptions); err == nil {
		t.Fatal("accepted stale plan hash")
	}
	bad = evidence
	bad.Reader.UseLayoutDetection = true
	if _, err := ApplyVLFieldEvidence(input, opts, base, &bad, modelOptions); err == nil {
		t.Fatal("accepted unsupported reader config")
	}
	bad = evidence
	bad.Regions[0].Region.Crop[0]++
	if _, err := ApplyVLFieldEvidence(input, opts, base, &bad, modelOptions); err == nil {
		t.Fatal("accepted Python-mutated crop echo")
	}
	bad = evidence
	bad.Regions[0].RegionID = "unplanned"
	if _, err := ApplyVLFieldEvidence(input, opts, base, &bad, modelOptions); err == nil {
		t.Fatal("accepted unknown region")
	}
	if _, err := ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: strings.Repeat("b", 64)}); err == nil {
		t.Fatal("accepted unexpected model tree identity")
	}
}

func TestVLRejectsChangedBaselineAndPreservesIt(t *testing.T) {
	input, opts, baseline, plan, reader := vlEvidenceFixture(t)
	evidence := vlEvidenceForPlan(plan, reader, nil)
	field := baseline.Fields["given_name"]
	field.Issues = append(field.Issues, "test_baseline_changed_after_plan")
	baseline.Fields["given_name"] = field
	before, _ := json.Marshal(baseline)
	_, err := ApplyVLFieldEvidence(input, opts, baseline, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256})
	if err == nil {
		t.Fatal("accepted evidence generated for a different baseline")
	}
	after, _ := json.Marshal(baseline)
	if string(after) != string(before) {
		t.Fatal("rejected application mutated the supplied baseline")
	}
}

func TestVLRejectsBaselineImageMismatchAndPreservesIt(t *testing.T) {
	input, opts, baseline, plan, reader := vlEvidenceFixture(t)
	evidence := vlEvidenceForPlan(plan, reader, nil)
	baseline.ImageID = "different-image"
	before, _ := json.Marshal(baseline)
	_, err := ApplyVLFieldEvidence(input, opts, baseline, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256})
	if err == nil {
		t.Fatal("accepted baseline for another image")
	}
	after, _ := json.Marshal(baseline)
	if string(after) != string(before) {
		t.Fatal("rejected application mutated the supplied baseline")
	}
}

func TestVLFieldTextValidationIsFieldScoped(t *testing.T) {
	for _, legitimate := range []string{"ПОЛЯКОВ", "ИМЯРЕК", "НОМЕРОВ"} {
		if value, _, ok := vlFieldValue("given_name", legitimate, false); !ok || value != legitimate {
			t.Fatalf("rejected a legitimate name containing a label substring: %q", legitimate)
		}
	}
	for _, contamination := range []string{"ИМЯ ИВАН ОТЧЕСТВО ПЕТРОВИЧ", "ФАМИЛИЯ ИВАН ПОЛ", "ИВАН МЕСТО РОЖДЕНИЯ"} {
		if _, _, ok := vlFieldValue("given_name", contamination, false); ok {
			t.Fatalf("accepted a whole other-field label phrase: %q", contamination)
		}
	}
	if value, rules, ok := vlFieldValue("gender", "Пол МУЖ. Дата рождения", false); !ok || value != "M" || !equalStrings(rules, []string{"vl_explicit_gender_token_in_assigned_row"}) {
		t.Fatal("failed to accept one explicit sex token with known adjacent captions")
	}
	for _, valid := range []struct{ raw, want string }{{"Пол: ЖЕНСКИЙ", "F"}, {"ЖЕН.", "F"}, {"ПОЛ МУЖСКОЙ", "M"}, {"M", "M"}, {"F", "F"}} {
		value, _, ok := vlFieldValue("gender", valid.raw, false)
		if !ok || value != valid.want {
			t.Fatalf("failed to parse explicit gender token %q", valid.raw)
		}
	}
	for _, invalid := range []string{"ИВАН", "МУЖСКОЙ ПЕТР", "МУЖ. ЖЕН.", "МУЖ. ЖЕНСКИЙ", "МУЖ 01.02.1990", "МУЖ СЕРИЯ"} {
		if _, _, ok := vlFieldValue("gender", invalid, false); ok {
			t.Fatalf("accepted non-field or ambiguous gender text %q", invalid)
		}
	}
	if _, _, ok := vlFieldValue("gender", "ФАМИЛИЯ: МУЖ", false); ok {
		t.Fatal("accepted cross-field label in gender text")
	}
	if value, _, ok := vlFieldValue("gender", "ПОЛ: ЖЕНСКИЙ", false); !ok || value != "F" {
		t.Fatal("failed to strip its own label and canonicalize explicit gender")
	}
	if _, _, ok := vlFieldValue("given_name", "ИВАН PETER", true); ok {
		t.Fatal("accepted unknown Latin in name")
	}
	if value, _, ok := vlFieldValue("issuer", "КЕМ ВЫДАН: МВД РОССИИ", true); !ok || value != "МВД РОССИИ" {
		t.Fatal("failed to strip own issuer label or validate authority text")
	}
	if value, _, ok := vlFieldValue("issuer", "ПАСПОРТ ВЫДАН: МВД РОССИИ", true); !ok || value != "МВД РОССИИ" {
		t.Fatal("failed to strip the known passport-issued prefix")
	}
	for _, invalid := range []string{"МВД РОССИИ ПАСПОРТ ВЫДАН", "МВД РОССИИ ДАТА ВЫДАЧИ", "ПАСПОРТВЫДАНМВД РОССИИ"} {
		if _, _, ok := vlFieldValue("issuer", invalid, true); ok {
			t.Fatalf("accepted issuer header contamination %q", invalid)
		}
	}
	if _, _, ok := vlFieldValue("birth_place", "МЕСТО РОЖДЕНИЯ: 01.02.1990", true); ok {
		t.Fatal("accepted date as place")
	}
	if _, _, ok := vlFieldValue("given_name", "ПАСПОРТ РОССИЙСКОЙ ФЕДЕРАЦИИ", true); ok {
		t.Fatal("accepted document caption as a name")
	}
	if value, _, ok := vlFieldValue("birth_place", "ПОС. 1905 ГОДА", true); !ok || value != "ПОС. 1905 ГОДА" {
		t.Fatal("rejected a valid place containing a year")
	}
	if value, _, ok := vlFieldValue("birth_place", "СЕЛО 8 МАРТА", true); !ok || value != "СЕЛО 8 МАРТА" {
		t.Fatal("rejected a valid place containing a number")
	}
	if value, _, ok := vlFieldValue("issuer", "ОТДЕЛ № 1 УФМС РОССИИ", true); !ok || value != "ОТДЕЛ № 1 УФМС РОССИИ" {
		t.Fatal("rejected valid issuer text containing a department number")
	}
	for _, invalid := range []string{"ГОРОД 01.02.1990", "ГОРОД 01-02-1990", "ГОРОД 123-456", "ГОРОД 123456"} {
		if _, _, ok := vlFieldValue("birth_place", invalid, true); ok {
			t.Fatalf("accepted date or document number as a place: %q", invalid)
		}
	}
	if value, _, ok := vlFieldValue("issuer", "ПАСПОРТНО-ВИЗОВЫМ ОТДЕЛОМ МВД РОССИИ", true); !ok || value != "ПАСПОРТНО-ВИЗОВЫМ ОТДЕЛОМ МВД РОССИИ" {
		t.Fatal("rejected a legitimate issuer containing a passport-derived word")
	}
}

func TestVLMergeDoesNotMutateBaselineCandidateBackingArrays(t *testing.T) {
	input, opts, base, _, reader := vlEvidenceFixture(t)
	field := base.Fields["given_name"]
	if len(field.Candidates) == 0 {
		t.Fatal("fixture has no incumbent name candidate")
	}
	field.Candidates = append([]Candidate(nil), field.Candidates...)
	variants := make([]string, 1, 4)
	variants[0] = "ТЕСТ"
	field.Candidates[0].RawVariants = variants
	base.Fields["given_name"] = field
	plan, err := BuildVLFieldPlan(input, base)
	if err != nil {
		t.Fatal(err)
	}
	evidence := vlEvidenceForPlan(plan, reader, map[string]string{"given_name": "ТЕСТ"})
	before, _ := json.Marshal(base)
	if _, err := ApplyVLFieldEvidence(input, opts, base, &evidence, VLFieldOptions{ModelTreeSHA256: reader.ModelTreeSHA256}); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(base)
	if string(before) != string(after) {
		t.Fatal("VL merge mutated baseline extraction")
	}
	backing := base.Fields["given_name"].Candidates[0].RawVariants[:cap(base.Fields["given_name"].Candidates[0].RawVariants)]
	for _, hidden := range backing[1:] {
		if hidden != "" {
			t.Fatal("VL merge wrote through a baseline candidate backing array")
		}
	}
}

func TestVLJSONRequiresDescriptorAndReadFieldsAndRejectsUnknowns(t *testing.T) {
	var reader VLReaderDescriptor
	if err := json.Unmarshal([]byte(`{"model_id":"PaddleOCR-VL-1.6-0.9B","model_tree_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","backend":"native","device":"cpu","pipeline_version":"v1.6","use_layout_detection":false}`), &reader); err == nil {
		t.Fatal("accepted reader descriptor missing use_doc_preprocessor")
	}
	if err := json.Unmarshal([]byte(`{"model_id":"PaddleOCR-VL-1.6-0.9B","model_tree_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","backend":"native","device":"cpu","pipeline_version":"v1.6","use_layout_detection":false,"use_doc_preprocessor":false,"extra":1}`), &reader); err == nil {
		t.Fatal("accepted unknown reader descriptor property")
	}
	var read VLFieldRead
	if err := json.Unmarshal([]byte(`{"region_id":"vl-given_name","region":{},"status":"error","blocks":[],"geometry_scope":"","elapsed_seconds":0}`), &read); err == nil {
		t.Fatal("accepted read missing nullable cap_reached")
	}
}
