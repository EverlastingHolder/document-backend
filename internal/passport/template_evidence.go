package passport

// This private experiment adds proposals from anchor-refined template crops.
// A template is a layout hypothesis, never permission to recognize a field.
import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
)

const TemplateEvidenceSchema = "passport-template-evidence/v1"
const templateID = "ru-internal-spread-anchored/v1"

type TemplateEvidence struct {
	SchemaVersion string            `json:"schema_version"`
	TemplateID    string            `json:"template_id"`
	Image         ImageObservation  `json:"image"`
	Anchors       map[string]string `json:"anchors"`
	Regions       []TemplateRegion  `json:"regions"`
}

type TemplateRecord struct {
	TextRaw       string  `json:"text_raw"`
	Score         float64 `json:"score"`
	Box           Rect    `json:"box"`
	GeometryScope string  `json:"geometry_scope"`
	ReaderMode    string  `json:"reader_mode,omitempty"`
}

type TemplateRegion struct {
	FieldID           string             `json:"field_id"`
	Crop              Rect               `json:"crop"`
	Mode              string             `json:"mode"`
	AnchorIDs         []string           `json:"anchor_ids"`
	Reader            string             `json:"reader"`
	ReaderMode        string             `json:"reader_mode"`
	ReaderModelSHA256 string             `json:"reader_model_sha256"`
	Status            string             `json:"status"`
	ElapsedSeconds    float64            `json:"elapsed_seconds"`
	Records           []TemplateRecord   `json:"records"`
	Alternatives      [][]TemplateRecord `json:"alternatives,omitempty"`
}

type TemplateProvenance struct {
	TemplateID string            `json:"template_id"`
	Image      ImageObservation  `json:"image"`
	Anchors    map[string]string `json:"anchors"`
	Region     TemplateRegion    `json:"region"`
}

func (e *TemplateEvidence) ValidateAgainst(input Observations) error {
	bad := func() error { return errors.New("invalid template evidence or geometry") }
	if e == nil || e.SchemaVersion != TemplateEvidenceSchema || e.TemplateID != templateID || e.Image != input.Image || len(e.Regions) > 6 {
		return bad()
	}
	registered, ok := input.Runtime["registered_spread"].(map[string]any)
	if !ok || registered["coordinate_space_role"] != "derived_registered_spread" || registered["selected_candidate_source"] != "contour" || registered["canonical_image_sha256"] != input.Image.SHA256 || input.Document.SeamY == nil || input.Document.Method != "contour" {
		return bad()
	}
	seam, w, h := *input.Document.SeamY, float64(input.Image.Width), float64(input.Image.Height)
	ph := h - seam
	if seam <= .35*h || seam >= .65*h {
		return bad()
	}
	lines := map[string]OCRLine{}
	passes := map[string]OCRPass{}
	for _, p := range input.Passes {
		passes[p.ID] = p
	}
	for _, l := range input.Lines {
		lines[l.ID] = l
	}
	anchor := func(role, kind string) (OCRLine, Rect, bool) {
		l, exists := lines[e.Anchors[role]]
		p := passes[l.PassID]
		b, valid := lineBounds(l)
		return l, b, exists && valid && p.Kind == kind && p.Status == "ok" && p.RotationDegrees == 0
	}
	bd, db, validBirth := anchor("birth_date", "lower_page")
	ud, ub, validIssue := anchor("issue_date", "upper_page")
	if !validBirth || !validIssue || !validDateLine(bd.TextRaw) || !validDateLine(ud.TextRaw) || db[0] < .5*w || db[1] <= seam+.32*ph || db[1] >= seam+.62*ph || ub[1] <= .25*seam || ub[1] >= .62*seam {
		return bad()
	}
	nameRoles := []string{"surname", "given_name", "patronymic"}
	nameBoxes := map[string]Rect{}
	nameIDs := map[string]bool{}
	var previous Rect
	leftMin, leftMax := w, 0.0
	for i, role := range nameRoles {
		l, b, ok := anchor(role, "lower_page")
		if !ok || l.PassID != bd.PassID || !nameLike(cleanValue(l.TextRaw)) || hasAnyLabel(l.TextRaw, allLabelTerms()) || nameIDs[l.ID] || b[0] < .48*w || b[2] > .93*w || b[1] <= seam+.08*ph || b[3] >= db[1] || b[3]-b[1] >= .09*ph || (i > 0 && previous[3] >= b[1]) {
			return bad()
		}
		nameIDs[l.ID] = true
		nameBoxes[role] = b
		previous = b
		leftMin = math.Min(leftMin, b[0])
		leftMax = math.Max(leftMax, b[0])
	}
	if leftMax-leftMin > .16*w {
		return bad()
	}
	seen := map[string]bool{}
	for _, r := range e.Regions {
		if seen[r.FieldID] || !validRect(r.Crop, w, h) || r.Reader != "rapidocr" || r.Mode != r.ReaderMode || !fieldEvidenceSHA.MatchString(r.ReaderModelSHA256) || math.IsNaN(r.ElapsedSeconds) || math.IsInf(r.ElapsedSeconds, 0) || r.ElapsedSeconds < 0 {
			return bad()
		}
		models, ok := input.Runtime["model_sha256"].(map[string]any)
		if !ok || models["recognizer"] != r.ReaderModelSHA256 {
			return bad()
		}
		seen[r.FieldID] = true
		if r.Status != "ok" && r.Status != "empty" && r.Status != "error" {
			return bad()
		}
		var expected []string
		if b, isName := nameBoxes[r.FieldID]; isName {
			expected = []string{e.Anchors[r.FieldID], bd.ID}
			if r.Mode != "rec_only" || r.Crop[0] < .40*w || r.Crop[2] > .92*w || r.Crop[1] <= seam || r.Crop[3] >= db[1] || r.Crop[0] > b[0] || r.Crop[1] > b[1] || r.Crop[2] < b[2] || r.Crop[3] < b[3] {
				return bad()
			}
			for role, other := range nameBoxes {
				if role != r.FieldID && rectsOverlap(r.Crop, other) {
					return bad()
				}
			}
		} else {
			switch r.FieldID {
			case "gender":
				expected = []string{bd.ID}
				if r.Mode != "rec_only" || r.Crop[0] < .35*w || r.Crop[2] > .60*w || r.Crop[2] >= db[0] || math.Abs((r.Crop[1]+r.Crop[3])/2-(db[1]+db[3])/2) > .04*ph || r.Crop[3]-r.Crop[1] > .12*ph {
					return bad()
				}
			case "birth_place":
				expected = []string{bd.ID}
				if r.Mode != "det_rec" || r.Crop[0] < .42*w || r.Crop[2] > .93*w || r.Crop[1] <= db[3] || r.Crop[3] > seam+.78*ph {
					return bad()
				}
			case "issuer":
				dept, deptBox, valid := anchor("department_code", "upper_page")
				expected = []string{ud.ID, dept.ID}
				if !valid || dept.PassID != ud.PassID || !deptDash.MatchString(canonicalDepartment(dept.TextRaw)) || math.Abs(deptBox[1]-ub[1]) > .06*seam || r.Mode != "det_rec" || r.Crop[0] < .20*w || r.Crop[2] > .93*w || r.Crop[1] < .12*seam || r.Crop[3] >= ub[1] {
					return bad()
				}
			default:
				return bad()
			}
		}
		if !sameUniqueStrings(r.AnchorIDs, expected) {
			return bad()
		}
		for _, l := range input.Lines {
			if strings.Contains(l.TextRaw, "<") {
				b, ok := lineBounds(l)
				if ok && rectsOverlap(b, r.Crop) {
					return bad()
				}
			}
		}
		if r.Mode == "rec_only" && len(r.Records) > 1 || len(r.Records) > 12 || r.Status != "ok" && (len(r.Records) > 0 || len(r.Alternatives) > 0) {
			return bad()
		}
		if len(r.Alternatives) > 1 || (r.Mode == "rec_only" && len(r.Alternatives) > 0) {
			return bad()
		}
		groups := [][]TemplateRecord{r.Records}
		groups = append(groups, r.Alternatives...)
		for groupIndex, records := range groups {
			if groupIndex > 0 && (len(records) == 0 || len(records) > 4) {
				return bad()
			}
			for _, record := range records {
				b := record.Box
				if !validRect(b, w, h) || b[0] < r.Crop[0] || b[1] < r.Crop[1] || b[2] > r.Crop[2] || b[3] > r.Crop[3] || strings.TrimSpace(record.TextRaw) == "" || strings.ContainsAny(record.TextRaw, "\r\n<") || math.IsNaN(record.Score) || math.IsInf(record.Score, 0) || record.Score < 0 || record.Score > 1 {
					return bad()
				}
				if groupIndex > 0 {
					if record.GeometryScope != "row_crop" || record.ReaderMode != "rec_only" {
						return bad()
					}
				} else if (r.Mode == "rec_only" && (record.GeometryScope != "entire_crop" || b != r.Crop)) || (r.Mode == "det_rec" && record.GeometryScope != "detector_box") || (record.ReaderMode != "" && record.ReaderMode != r.Mode) {
					return bad()
				}
			}
		}
	}
	return nil
}

// templateText joins only adjacent, aligned detector rows within ONE crop.
// Incoherent columns/rows are rejected, never reordered to fit a reference.
func templateText(r TemplateRegion) (string, bool) {
	if len(r.Records) == 0 {
		return "", false
	}
	records := append([]TemplateRecord(nil), r.Records...)
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i].Box, records[j].Box
		if math.Abs((a[1]+a[3])/2-(b[1]+b[3])/2) < .5*math.Min(a[3]-a[1], b[3]-b[1]) {
			return a[0] < b[0]
		}
		return a[1] < b[1]
	})
	parts := []string{}
	for i, record := range records {
		if i > 0 {
			a, b := records[i-1].Box, record.Box
			sameRow := math.Abs((a[1]+a[3])/2-(b[1]+b[3])/2) < .5*math.Min(a[3]-a[1], b[3]-b[1])
			if sameRow {
				if b[0]-a[2] > 2*math.Max(a[3]-a[1], b[3]-b[1]) {
					return "", false
				}
			} else {
				if b[1]-a[3] > 2.5*math.Max(a[3]-a[1], b[3]-b[1]) || math.Min(a[2], b[2]) <= math.Max(a[0], b[0]) {
					return "", false
				}
			}
		}
		parts = append(parts, cleanValue(record.TextRaw))
	}
	value := strings.Join(parts, " ")
	if r.FieldID == "surname" || r.FieldID == "given_name" || r.FieldID == "patronymic" {
		return value, nameLike(value) && !hasAnyLabel(value, allLabelTerms())
	}
	if r.FieldID == "gender" {
		v, ok := canonicalLayoutGender(value)
		return v, ok
	}
	if !hasLetters(value) || hasAnyLabel(value, allLabelTerms()) {
		return "", false
	}
	if r.FieldID == "issuer" && !hasAnyLabel(value, authorityKeywords) {
		return "", false
	}
	return value, true
}

func ApplyTemplateEvidence(input Observations, options Options, baseline Extraction, evidence *TemplateEvidence) (Extraction, error) {
	if evidence == nil {
		return baseline, nil
	}
	if err := input.Validate(); err != nil {
		return Extraction{}, err
	}
	if baseline.ImageID != input.Image.ID {
		return Extraction{}, errors.New("template baseline image mismatch")
	}
	if options.PassSet != "all" {
		return Extraction{}, errors.New("template proposals require all registered page passes")
	}
	if err := evidence.ValidateAgainst(input); err != nil {
		return Extraction{}, err
	}
	fields := make(map[string]FieldResult, len(baseline.Fields))
	for id, field := range baseline.Fields {
		fields[id] = field
	}
	baseline.Fields = fields
	baseline.ReviewRequired = true
	for _, region := range evidence.Regions {
		groups := [][]TemplateRecord{region.Records}
		groups = append(groups, region.Alternatives...)
		for _, records := range groups {
			region.Records = records
			region.Alternatives = nil
			value, ok := templateText(region)
			if !ok {
				field := baseline.Fields[region.FieldID]
				field.Issues = appendUnique(field.Issues, "template_reader_text_not_eligible_for_proposal")
				baseline.Fields[region.FieldID] = field
				continue
			}
			raw := value
			rules := []string{"registered_template_anchor_refined_crop"}
			if region.FieldID == "surname" || region.FieldID == "given_name" || region.FieldID == "patronymic" {
				var nr []string
				value, nr, _ = normalizeName(value, options.NormalizeNames)
				rules = append(rules, nr...)
			}
			if region.FieldID == "birth_place" || region.FieldID == "issuer" {
				// This Russian-form template proposes only visually identical
				// uppercase glyph substitutions. Raw text stays in provenance;
				// no transliteration, spelling correction or dictionary lookup.
				var nr []string
				value, nr, _ = normalizeName(value, true)
				rules = append(rules, nr...)
				if len(nr) > 0 {
					rules = append(rules, "template_cyrillic_visual_normalization_proposal")
				}
				if containsLatin(value) {
					field := baseline.Fields[region.FieldID]
					field.Issues = appendUnique(field.Issues, "template_nonvisual_latin_requires_review_no_proposal")
					baseline.Fields[region.FieldID] = field
					continue
				}
			}
			if region.FieldID == "gender" && len(records) == 1 {
				raw = cleanValue(records[0].TextRaw)
			}
			candidate := Candidate{Value: value, Raw: raw, RawVariants: []string{raw}, Rules: rules, Context: "template_crop:review_only", TemplateEvidence: []TemplateProvenance{{TemplateID: evidence.TemplateID, Image: evidence.Image, Anchors: evidence.Anchors, Region: region}}}
			field := baseline.Fields[region.FieldID]
			candidates := append(append([]Candidate(nil), field.Candidates...), candidate)
			baseline.Fields[region.FieldID] = resolveCandidateSet(input, region.FieldID, candidates, appendUnique(field.Issues, "template_candidate_requires_manual_review"), true)
		}
	}
	return baseline, nil
}

func mergeTemplateProvenance(a, b []TemplateProvenance) []TemplateProvenance {
	for _, source := range b {
		encoded, _ := json.Marshal(source)
		found := false
		for _, existing := range a {
			other, _ := json.Marshal(existing)
			if string(encoded) == string(other) {
				found = true
				break
			}
		}
		if !found {
			a = append(a, source)
		}
	}
	return a
}
