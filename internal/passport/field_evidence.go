package passport

import (
	"errors"
	"math"
	"regexp"
	"strings"
)

const FieldEvidenceSchema = "passport-field-evidence/v1"

var fieldEvidenceSHA = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// FieldEvidence is a private sidecar. It never adds a synthetic OCR pass or line
// to observations v1/v2.
type FieldEvidence struct {
	SchemaVersion string                `json:"schema_version"`
	Image         FieldEvidenceImage    `json:"image"`
	Evidence      []FieldEvidenceRecord `json:"evidence"`
}

type FieldEvidenceImage struct {
	SHA256          string `json:"sha256"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	CoordinateFrame string `json:"coordinate_frame"`
}

type FieldEvidenceAnchor struct {
	Role     string `json:"role"`
	LineID   string `json:"line_id"`
	PassID   string `json:"pass_id"`
	Relation string `json:"relation"`
}

type FieldEvidenceRecord struct {
	FieldID           string                `json:"field_id"`
	RoleQuality       string                `json:"role_quality"`
	ProposalMethod    string                `json:"proposal_method"`
	SourcePassIDs     []string              `json:"source_pass_ids"`
	SourceLineIDs     []string              `json:"source_line_ids"`
	NameRowLineIDs    [][]string            `json:"name_row_line_ids"`
	AdjacentAnchors   []FieldEvidenceAnchor `json:"adjacent_anchors"`
	ROIBounds         Rect                  `json:"roi_bounds"`
	GeometryScope     string                `json:"geometry_scope"`
	Reader            string                `json:"reader"`
	ReaderMode        string                `json:"reader_mode"`
	ReaderModelSHA256 string                `json:"reader_model_sha256"`
	RawText           string                `json:"raw_text"`
	Score             float64               `json:"score"`
	Quad              Quad                  `json:"quad"`
}

// FieldEvidenceProvenance is retained on the candidate without mislabeling the
// crop reader output as a line from one of the input observation passes.
type FieldEvidenceProvenance struct {
	ImageSHA256       string                `json:"image_sha256"`
	ImageWidth        int                   `json:"image_width"`
	ImageHeight       int                   `json:"image_height"`
	ProposalMethod    string                `json:"proposal_method"`
	RoleQuality       string                `json:"role_quality"`
	SourcePassIDs     []string              `json:"source_pass_ids"`
	SourceLineIDs     []string              `json:"source_line_ids"`
	NameRowLineIDs    [][]string            `json:"name_row_line_ids"`
	AdjacentAnchors   []FieldEvidenceAnchor `json:"adjacent_anchors"`
	ROIBounds         Rect                  `json:"roi_bounds"`
	GeometryScope     string                `json:"geometry_scope"`
	Reader            string                `json:"reader"`
	ReaderMode        string                `json:"reader_mode"`
	ReaderModelSHA256 string                `json:"reader_model_sha256"`
	Score             float64               `json:"score"`
	Quad              Quad                  `json:"quad"`
}

func (e *FieldEvidence) ValidateAgainst(input Observations) error {
	if e == nil || e.SchemaVersion != FieldEvidenceSchema || len(e.Evidence) != 1 {
		return errors.New("invalid field evidence envelope")
	}
	if e.Image.SHA256 != input.Image.SHA256 || e.Image.Width != input.Image.Width || e.Image.Height != input.Image.Height ||
		e.Image.CoordinateFrame != input.Image.CoordinateFrame || e.Image.CoordinateFrame != "exif_normalized_pixels" {
		return errors.New("field evidence image mismatch")
	}
	record := e.Evidence[0]
	if record.FieldID != "given_name" || record.RoleQuality != "supported_row" || record.ProposalMethod != "viz_layout_v1" ||
		record.Reader != "rapidocr" || record.ReaderMode != "rec_only" || record.GeometryScope != "entire_crop" ||
		!fieldEvidenceSHA.MatchString(record.ReaderModelSHA256) || !nameLike(cleanValue(record.RawText)) || strings.ContainsAny(record.RawText, "\r\n") ||
		math.IsNaN(record.Score) || math.IsInf(record.Score, 0) || record.Score < 0 || record.Score > 1 {
		return errors.New("invalid supported-name evidence")
	}
	if !validRect(record.ROIBounds, float64(input.Image.Width), float64(input.Image.Height)) || len(record.NameRowLineIDs) != 3 || len(record.AdjacentAnchors) != 1 {
		return errors.New("invalid field evidence geometry or anchors")
	}
	doc := input.Document.Bounds
	if record.ROIBounds[0] < doc[0] || record.ROIBounds[1] < doc[1] || record.ROIBounds[2] > doc[2] || record.ROIBounds[3] > doc[3] {
		return errors.New("field evidence ROI outside document")
	}
	if input.Document.SeamY == nil {
		return errors.New("field evidence page seam unavailable")
	}
	passes := make(map[string]OCRPass, len(input.Passes))
	for _, pass := range input.Passes {
		passes[pass.ID] = pass
	}
	lines := make(map[string]OCRLine, len(input.Lines))
	for _, line := range input.Lines {
		lines[line.ID] = line
	}
	if len(record.SourcePassIDs) != 1 || record.SourcePassIDs[0] == "" {
		return errors.New("invalid source pass references")
	}
	if len(uniqueStrings(record.SourcePassIDs)) != len(record.SourcePassIDs) || len(uniqueStrings(record.SourceLineIDs)) != len(record.SourceLineIDs) {
		return errors.New("duplicate source reference")
	}
	pass, exists := passes[record.SourcePassIDs[0]]
	if !exists || pass.Status != "ok" || pass.RotationDegrees != 0 || pass.Kind == "mrz" ||
		(pass.Kind != "original" && pass.Kind != "document" && pass.Kind != "upper_page" && pass.Kind != "lower_page") {
		return errors.New("unsupported source pass")
	}
	rowBoxes := make([]Rect, 3)
	allRowIDs := make([]string, 0, 3)
	for rowIndex, ids := range record.NameRowLineIDs {
		if len(ids) == 0 {
			return errors.New("empty name row reference")
		}
		if len(uniqueStrings(ids)) != len(ids) {
			return errors.New("duplicate line id within name row")
		}
		rowLines := make([]OCRLine, 0, len(ids))
		for _, id := range ids {
			line, ok := lines[id]
			if !ok || line.PassID != pass.ID || !nameLike(cleanValue(line.TextRaw)) || hasAnyLabel(line.TextRaw, allLabelTerms()) {
				return errors.New("invalid name row reference")
			}
			rowLines = append(rowLines, line)
			allRowIDs = append(allRowIDs, id)
		}
		box, ok := unionLineBoxes(rowLines)
		if !ok || (box[1]+box[3])/2 <= *input.Document.SeamY || box[0] < doc[0]+0.42*(doc[2]-doc[0]) {
			return errors.New("name row outside lower-page name column")
		}
		rowBoxes[rowIndex] = box
	}
	if !sameUniqueStrings(record.SourceLineIDs, record.NameRowLineIDs[1]) {
		return errors.New("source lines must identify the middle name row")
	}
	if len(allRowIDs) != len(uniqueStrings(allRowIDs)) {
		return errors.New("duplicate name row reference")
	}
	for i := 1; i < len(rowBoxes); i++ {
		prev, current := rowBoxes[i-1], rowBoxes[i]
		prevY, currentY := (prev[1]+prev[3])/2, (current[1]+current[3])/2
		prevH, currentH := prev[3]-prev[1], current[3]-current[1]
		if currentY <= prevY || current[1]-prev[3] > math.Max(8, 2.5*math.Max(prevH, currentH)) {
			return errors.New("name rows are not ordered and adjacent")
		}
	}
	leftMin, leftMax := rowBoxes[0][0], rowBoxes[0][0]
	for _, box := range rowBoxes[1:] {
		leftMin = math.Min(leftMin, box[0])
		leftMax = math.Max(leftMax, box[0])
	}
	if leftMax-leftMin > 0.12*(doc[2]-doc[0]) {
		return errors.New("name rows are not aligned")
	}
	middle := rowBoxes[1]
	if record.ROIBounds[0] > middle[0] || record.ROIBounds[1] > middle[1] || record.ROIBounds[2] < middle[2] || record.ROIBounds[3] < middle[3] {
		return errors.New("ROI does not contain middle name row")
	}
	anchor := record.AdjacentAnchors[0]
	if anchor.Role != "birth_date" || anchor.Relation != "upper_row_boundary" || anchor.PassID != pass.ID {
		return errors.New("invalid birth-date anchor")
	}
	dateLine, ok := lines[anchor.LineID]
	if !ok || dateLine.PassID != pass.ID || !validDateLine(dateLine.TextRaw) {
		return errors.New("invalid birth-date reference")
	}
	dateBox, ok := lineBounds(dateLine)
	if !ok || dateBox[1] <= middle[3] || record.ROIBounds[3] > dateBox[1] {
		return errors.New("ROI crosses birth-date boundary")
	}
	for _, line := range input.Lines {
		p := passes[line.PassID]
		if p.Kind != "mrz" && !looksLikeMRZLine(line, doc) {
			continue
		}
		box, ok := lineBounds(line)
		if ok && rectsOverlap(record.ROIBounds, box) {
			return errors.New("ROI crosses MRZ evidence")
		}
	}
	if !quadInsideRect(record.Quad, record.ROIBounds) || !quadMatchesRect(record.Quad, record.ROIBounds) {
		return errors.New("reader geometry must represent the entire crop")
	}
	return nil
}

func (r FieldEvidenceRecord) provenance(image FieldEvidenceImage) FieldEvidenceProvenance {
	return FieldEvidenceProvenance{
		ImageSHA256: strings.ToLower(image.SHA256), ImageWidth: image.Width, ImageHeight: image.Height,
		ProposalMethod: r.ProposalMethod, RoleQuality: r.RoleQuality,
		SourcePassIDs: append([]string(nil), r.SourcePassIDs...), SourceLineIDs: append([]string(nil), r.SourceLineIDs...),
		NameRowLineIDs: cloneLineIDs(r.NameRowLineIDs), AdjacentAnchors: append([]FieldEvidenceAnchor(nil), r.AdjacentAnchors...),
		ROIBounds: r.ROIBounds, GeometryScope: r.GeometryScope, Reader: r.Reader, ReaderMode: r.ReaderMode,
		ReaderModelSHA256: strings.ToLower(r.ReaderModelSHA256), Score: r.Score, Quad: r.Quad,
	}
}

func (e *FieldEvidence) candidate(raw string, normalizeNames bool) (Candidate, error) {
	if e == nil || len(e.Evidence) != 1 {
		return Candidate{}, errors.New("invalid field evidence")
	}
	cleaned := cleanValue(raw)
	value, rules, _ := normalizeName(cleaned, normalizeNames)
	rules = append(rules, "supported_name_roi_candidate")
	return Candidate{Value: value, Raw: cleaned, RawVariants: []string{cleaned}, Rules: uniqueStrings(rules), Context: "field_evidence:supported_name_roi", FieldEvidence: []FieldEvidenceProvenance{e.Evidence[0].provenance(e.Image)}}, nil
}

// ExtractWithFieldEvidence runs the unchanged baseline extractor, then folds the
// optional candidate into the field candidate resolution invariant. The ROI
// candidate can only make given_name uncertain; it never authorizes recognition.
func ExtractWithFieldEvidence(input Observations, options Options, evidence *FieldEvidence) (Extraction, error) {
	if err := input.Validate(); err != nil {
		return Extraction{}, err
	}
	if evidence != nil {
		if err := evidence.ValidateAgainst(input); err != nil {
			return Extraction{}, err
		}
	}
	baseline, err := Extract(input, options)
	if err != nil {
		return Extraction{}, err
	}
	if evidence == nil {
		return baseline, nil
	}
	candidate, err := evidence.candidate(evidence.Evidence[0].RawText, options.NormalizeNames)
	if err != nil {
		return Extraction{}, err
	}
	field := baseline.Fields["given_name"]
	field.Issues = appendUnique(field.Issues, "field_roi_candidate_requires_manual_review")
	field.Candidates = append(field.Candidates, candidate)
	baseline.Fields["given_name"] = resolveCandidateSet(input, "given_name", field.Candidates, field.Issues, true)
	return baseline, nil
}

func unionLineBoxes(lines []OCRLine) (Rect, bool) {
	var box Rect
	for i, line := range lines {
		current, ok := lineBounds(line)
		if !ok {
			return Rect{}, false
		}
		if i == 0 {
			box = current
			continue
		}
		box[0] = math.Min(box[0], current[0])
		box[1] = math.Min(box[1], current[1])
		box[2] = math.Max(box[2], current[2])
		box[3] = math.Max(box[3], current[3])
	}
	return box, len(lines) > 0
}

func lineBounds(line OCRLine) (Rect, bool) {
	box := Rect{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, point := range line.Quad {
		if math.IsNaN(point[0]) || math.IsNaN(point[1]) || math.IsInf(point[0], 0) || math.IsInf(point[1], 0) {
			return Rect{}, false
		}
		box[0] = math.Min(box[0], point[0])
		box[1] = math.Min(box[1], point[1])
		box[2] = math.Max(box[2], point[0])
		box[3] = math.Max(box[3], point[1])
	}
	return box, box[2] > box[0] && box[3] > box[1]
}

func sameUniqueStrings(a, b []string) bool {
	return len(a) == len(b) && strings.Join(uniqueStrings(a), "\x00") == strings.Join(uniqueStrings(b), "\x00")
}

func cloneLineIDs(ids [][]string) [][]string {
	copyIDs := make([][]string, len(ids))
	for i := range ids {
		copyIDs[i] = append([]string(nil), ids[i]...)
	}
	return copyIDs
}

func validDateLine(raw string) bool {
	_, ok := parseDate(strings.TrimSpace(raw))
	return ok
}

func looksLikeMRZLine(line OCRLine, document Rect) bool {
	if strings.Contains(line.TextRaw, "<") {
		return true
	}
	box, ok := lineBounds(line)
	if !ok {
		return false
	}
	return (box[2]-box[0]) > 0.5*(document[2]-document[0]) && (box[1]+box[3])/2 >= document[1]+0.84*(document[3]-document[1])
}

func rectsOverlap(a, b Rect) bool { return a[0] < b[2] && b[0] < a[2] && a[1] < b[3] && b[1] < a[3] }

func quadInsideRect(quad Quad, rect Rect) bool {
	for _, point := range quad {
		if math.IsNaN(point[0]) || math.IsNaN(point[1]) || math.IsInf(point[0], 0) || math.IsInf(point[1], 0) || point[0] < rect[0] || point[1] < rect[1] || point[0] > rect[2] || point[1] > rect[3] {
			return false
		}
	}
	return true
}

func quadMatchesRect(quad Quad, rect Rect) bool {
	box := Rect{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, point := range quad {
		box[0] = math.Min(box[0], point[0])
		box[1] = math.Min(box[1], point[1])
		box[2] = math.Max(box[2], point[0])
		box[3] = math.Max(box[3], point[1])
	}
	for i := range box {
		if math.Abs(box[i]-rect[i]) > 0.01 {
			return false
		}
	}
	want := [][2]float64{{rect[0], rect[1]}, {rect[2], rect[1]}, {rect[2], rect[3]}, {rect[0], rect[3]}}
	seen := make(map[int]bool, 4)
	for _, point := range quad {
		matched := -1
		for i, corner := range want {
			if math.Abs(point[0]-corner[0]) <= 0.01 && math.Abs(point[1]-corner[1]) <= 0.01 {
				matched = i
				break
			}
		}
		if matched < 0 || seen[matched] {
			return false
		}
		seen[matched] = true
	}
	return len(seen) == 4
}
