package passport

const ExtractionSchema = "passport-extraction/v1"

type Options struct {
	NormalizeNames bool
	PassSet        string // "original" or "all"
	MRZPolicy      string // "off" or "augment"
}

type Extraction struct {
	SchemaVersion  string                 `json:"schema_version"`
	ImageID        string                 `json:"image_id"`
	ReviewRequired bool                   `json:"review_required"`
	Fields         map[string]FieldResult `json:"fields"`
	MRZ            *MRZDiagnostics        `json:"mrz,omitempty"`
}

type FieldResult struct {
	Value      *string     `json:"value"`
	State      string      `json:"state"`
	Raw        string      `json:"raw,omitempty"`
	Candidates []Candidate `json:"candidates"`
	Rules      []string    `json:"rules"`
	Issues     []string    `json:"issues"`
	Evidence   Evidence    `json:"evidence"`
	Context    string      `json:"context"`
}

type Candidate struct {
	Value            string                    `json:"value"`
	Raw              string                    `json:"raw"`
	RawVariants      []string                  `json:"raw_variants"`
	Rules            []string                  `json:"rules"`
	LineIDs          []string                  `json:"line_ids"`
	PassIDs          []string                  `json:"pass_ids"`
	Context          string                    `json:"context"`
	FieldEvidence    []FieldEvidenceProvenance `json:"field_evidence,omitempty"`
	TemplateEvidence []TemplateProvenance      `json:"template_evidence,omitempty"`
	VLEvidence       []VLFieldProvenance       `json:"vl_evidence,omitempty"`
}

type Evidence struct {
	LineIDs []string `json:"line_ids"`
	PassIDs []string `json:"pass_ids"`
}

func emptyField() FieldResult {
	return FieldResult{
		State:      "missing",
		Candidates: []Candidate{},
		Rules:      []string{},
		Issues:     []string{},
		Evidence:   Evidence{LineIDs: []string{}, PassIDs: []string{}},
	}
}
