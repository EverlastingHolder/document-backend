package passport

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func templateFixture() (Observations, TemplateEvidence) {
	o := syntheticObservations()
	o.Passes[0].Kind = "lower_page"
	o.Passes = append(o.Passes, OCRPass{ID: "upper", Kind: "upper_page", Crop: Rect{0, 0, 1000, 500}, TransformToImage: Matrix3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, Status: "ok"})
	addLine(&o, "family", "АЛЬФОВ", 620, 590)
	addLine(&o, "name", "ТЕСТ", 620, 640)
	addLine(&o, "patronymic", "АЛЬФОВИЧ", 620, 680)
	addLine(&o, "birth", "01.02.1990", 650, 730)
	addLine(&o, "issue", "02.03.2015", 250, 200)
	o.Lines[len(o.Lines)-1].PassID = "upper"
	o.Runtime = map[string]any{"registered_spread": map[string]any{"coordinate_space_role": "derived_registered_spread", "selected_candidate_source": "contour", "canonical_image_sha256": o.Image.SHA256}, "model_sha256": map[string]any{"recognizer": strings.Repeat("b", 64)}}
	r := TemplateRegion{FieldID: "given_name", Crop: Rect{540, 630, 900, 660}, Mode: "rec_only", AnchorIDs: []string{"name", "birth"}, Reader: "rapidocr", ReaderMode: "rec_only", ReaderModelSHA256: strings.Repeat("b", 64), Status: "ok", Records: []TemplateRecord{{TextRaw: "ТЕСТ", Score: .9, Box: Rect{540, 630, 900, 660}, GeometryScope: "entire_crop"}}}
	e := TemplateEvidence{SchemaVersion: TemplateEvidenceSchema, TemplateID: templateID, Image: o.Image, Anchors: map[string]string{"issue_date": "issue", "birth_date": "birth", "surname": "family", "given_name": "name", "patronymic": "patronymic"}, Regions: []TemplateRegion{r}}
	return o, e
}

func TestTemplateMergeAgreesAndPreservesOwnProvenance(t *testing.T) {
	o, e := templateFixture()
	opts := Options{PassSet: "all", NormalizeNames: true}
	base, err := Extract(o, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTemplateEvidence(o, opts, base, &e)
	if err != nil {
		t.Fatal(err)
	}
	f := result.Fields["given_name"]
	if f.Value == nil || *f.Value != "ТЕСТ" || f.State != "uncertain" || len(f.Candidates) != 1 || len(f.Candidates[0].TemplateEvidence) != 1 {
		t.Fatal("agreement/provenance lost")
	}
	if len(f.Candidates[0].LineIDs) != 1 || f.Candidates[0].LineIDs[0] != "name" {
		t.Fatal("crop output forged a page OCR line")
	}
}

func TestTemplateConflictNeverOverridesExistingOrChoosesByScore(t *testing.T) {
	o, e := templateFixture()
	opts := Options{PassSet: "all"}
	base, _ := Extract(o, opts)
	e.Regions[0].Records[0].TextRaw = "ПЕТР"
	e.Regions[0].Records[0].Score = 1
	r, err := ApplyTemplateEvidence(o, opts, base, &e)
	if err != nil {
		t.Fatal(err)
	}
	f := r.Fields["given_name"]
	if f.Value != nil || f.State != "uncertain" || len(f.Candidates) != 2 {
		t.Fatal("conflict must abstain")
	}
	// Additional agreement with only one of two candidates cannot resolve it.
	r, err = ApplyTemplateEvidence(o, opts, r, &e)
	if err != nil || r.Fields["given_name"].Value != nil || len(r.Fields["given_name"].Candidates) != 2 {
		t.Fatal("repeated reader agreement resolved conflict")
	}
}

func TestTemplateNilIsBaselineIdenticalAndMissingAddsUncertain(t *testing.T) {
	o, e := templateFixture()
	base, _ := Extract(o, Options{PassSet: "all"})
	r, err := ApplyTemplateEvidence(o, Options{}, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(base)
	b, _ := json.Marshal(r)
	if string(a) != string(b) {
		t.Fatal("nil evidence changed baseline")
	}
	base.Fields["given_name"] = emptyField()
	r, err = ApplyTemplateEvidence(o, Options{PassSet: "all"}, base, &e)
	if err != nil || r.Fields["given_name"].State != "uncertain" || r.Fields["given_name"].Value == nil {
		t.Fatal("new proposal missing")
	}
}

func TestTemplateVisualNormalizationPreservesRawAndDoesNotMutateBaseline(t *testing.T) {
	o, e := templateFixture()
	opts := Options{PassSet: "all"}
	base, _ := Extract(o, opts)
	raw := "ГOP. AЛЬФA"
	e.Regions = []TemplateRegion{{FieldID: "birth_place", Crop: Rect{440, 750, 910, 870}, Mode: "det_rec", AnchorIDs: []string{"birth"}, Reader: "rapidocr", ReaderMode: "det_rec", ReaderModelSHA256: strings.Repeat("b", 64), Status: "ok", Records: []TemplateRecord{{TextRaw: raw, Score: .9, Box: Rect{500, 760, 800, 790}, GeometryScope: "detector_box", ReaderMode: "det_rec"}}}}
	before, _ := json.Marshal(base)
	result, err := ApplyTemplateEvidence(o, opts, base, &e)
	if err != nil {
		t.Fatal(err)
	}
	f := result.Fields["birth_place"]
	if f.Value == nil || *f.Value != "ГОР. АЛЬФА" || f.State != "uncertain" || f.Raw != raw || !hasRule(f.Rules, "template_cyrillic_visual_normalization_proposal") {
		t.Fatal("visual proposal or raw provenance lost")
	}
	after, _ := json.Marshal(base)
	if string(before) != string(after) {
		t.Fatal("template merge mutated baseline")
	}
	e.Regions[0].Records[0].TextRaw = "ГОРОД ZETA"
	unsupported, err := ApplyTemplateEvidence(o, opts, base, &e)
	if err != nil || unsupported.Fields["birth_place"].Value != nil || !hasIssue(unsupported.Fields["birth_place"].Issues, "template_nonvisual_latin_requires_review_no_proposal") {
		t.Fatal("unsupported Latin was guessed into Cyrillic")
	}
}

func TestTemplateRowAlternativeKeepsReaderGeometryAndConflict(t *testing.T) {
	o, e := templateFixture()
	opts := Options{PassSet: "all"}
	base, _ := Extract(o, opts)
	r := e.Regions[0]
	r.FieldID = "birth_place"
	r.Mode = "det_rec"
	r.ReaderMode = "det_rec"
	r.Crop = Rect{440, 750, 910, 870}
	r.AnchorIDs = []string{"birth"}
	r.Records = []TemplateRecord{{TextRaw: "ГОРОД АЛЬФА", Score: .99, Box: Rect{500, 760, 800, 790}, GeometryScope: "detector_box"}}
	r.Alternatives = [][]TemplateRecord{{{TextRaw: "ГОРОД БЕТА", Score: .2, Box: Rect{440, 755, 910, 795}, GeometryScope: "row_crop", ReaderMode: "rec_only"}}}
	e.Regions = []TemplateRegion{r}
	result, err := ApplyTemplateEvidence(o, opts, base, &e)
	if err != nil {
		t.Fatal(err)
	}
	f := result.Fields["birth_place"]
	if f.Value != nil || len(f.Candidates) != 2 || f.State != "uncertain" {
		t.Fatal("alternative selected by score or lost")
	}
	for _, c := range f.Candidates {
		if len(c.TemplateEvidence) != 1 || len(c.LineIDs) != 0 {
			t.Fatal("reader provenance forged")
		}
	}
	e.Regions[0].Alternatives[0][0].GeometryScope = "glyph_box"
	if e.ValidateAgainst(o) == nil {
		t.Fatal("invalid alternative geometry accepted")
	}
	e.Regions[0].Alternatives[0][0].GeometryScope = "row_crop"
	e.Regions[0].Status = "empty"
	e.Regions[0].Records = nil
	if e.ValidateAgainst(o) == nil {
		t.Fatal("failed/empty primary reader authorized an alternative")
	}
}

func TestTemplateRejectsUnboundUnorientedOrCrossingEvidence(t *testing.T) {
	for _, mutate := range []func(*Observations, *TemplateEvidence){
		func(o *Observations, e *TemplateEvidence) { e.Image.SHA256 = strings.Repeat("c", 64) },
		func(o *Observations, e *TemplateEvidence) { e.Anchors["birth_date"] = "issue" },
		func(o *Observations, e *TemplateEvidence) { e.Anchors["patronymic"] = "name" },
		func(o *Observations, e *TemplateEvidence) { delete(o.Runtime, "registered_spread") },
		func(o *Observations, e *TemplateEvidence) { e.Regions[0].Crop[3] = 745 },
		func(o *Observations, e *TemplateEvidence) { e.Regions[0].Records[0].Box[2] = 950 },
		func(o *Observations, e *TemplateEvidence) { e.Regions[0].Records[0].Score = math.NaN() },
		func(o *Observations, e *TemplateEvidence) { e.Regions[0].ReaderModelSHA256 = strings.Repeat("c", 64) },
		func(o *Observations, e *TemplateEvidence) { e.Regions[0].FieldID = "birth_date" },
		func(o *Observations, e *TemplateEvidence) { e.Regions = append(e.Regions, e.Regions[0]) },
		func(o *Observations, e *TemplateEvidence) { e.Regions[0].Records[0].GeometryScope = "glyph_box" },
		func(o *Observations, e *TemplateEvidence) { e.Regions[0].AnchorIDs = []string{"name", "name"} },
	} {
		o, e := templateFixture()
		mutate(&o, &e)
		if e.ValidateAgainst(o) == nil {
			t.Fatal("invalid evidence accepted")
		}
	}
}

func TestTemplateBlockJoinRequiresGeometricallyAdjacentRows(t *testing.T) {
	r := TemplateRegion{FieldID: "birth_place", Records: []TemplateRecord{{TextRaw: "ГОРОД", Box: Rect{500, 750, 700, 770}}, {TextRaw: "АЛЬФА", Box: Rect{540, 780, 720, 800}}}}
	if text, ok := templateText(r); !ok || text != "ГОРОД АЛЬФА" {
		t.Fatal("adjacent rows not joined")
	}
	r.Records[1].Box = Rect{800, 780, 950, 800}
	if _, ok := templateText(r); ok {
		t.Fatal("different columns joined")
	}
	r.Records[1].Box = Rect{540, 850, 720, 870}
	if _, ok := templateText(r); ok {
		t.Fatal("distant rows joined")
	}
}
