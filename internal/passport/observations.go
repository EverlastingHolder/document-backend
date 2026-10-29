package passport

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

const ObservationsSchema = "passport-ocr-observations/v1"
const ObservationsSchemaV2 = "passport-ocr-observations/v2"
const ObservationsSchemaV3 = "passport-ocr-observations/v3"

const maxSourceTilePasses = 6

// Observations is the private OCR-to-Go handoff. All coordinates use the
// EXIF-normalized original-image pixel frame, including coordinates emitted
// by crop and rotated passes.
type Observations struct {
	SchemaVersion string           `json:"schema_version"`
	Image         ImageObservation `json:"image"`
	Document      DocumentBounds   `json:"document"`
	Passes        []OCRPass        `json:"passes"`
	Lines         []OCRLine        `json:"lines"`
	Runtime       map[string]any   `json:"runtime,omitempty"`
}

type ImageObservation struct {
	ID              string `json:"id"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	SHA256          string `json:"sha256"`
	CoordinateFrame string `json:"coordinate_frame"`
}

type Rect [4]float64
type Point [2]float64
type Matrix3 [3][3]float64
type Quad [4]Point

type DocumentBounds struct {
	Bounds  Rect     `json:"bounds"`
	Method  string   `json:"method"`
	Quality string   `json:"quality"`
	SeamY   *float64 `json:"seam_y"`
}

type OCRPass struct {
	ID               string  `json:"id"`
	Kind             string  `json:"kind"`
	Crop             Rect    `json:"crop"`
	RotationDegrees  int     `json:"rotation_degrees"`
	TransformToImage Matrix3 `json:"transform_to_image"`
	ElapsedSeconds   float64 `json:"elapsed_seconds"`
	Status           string  `json:"status"`
	Reader           string  `json:"reader,omitempty"`
	IssueCode        string  `json:"issue_code,omitempty"`
}

type OCRLine struct {
	ID      string  `json:"id"`
	PassID  string  `json:"pass_id"`
	TextRaw string  `json:"text_raw"`
	Score   float64 `json:"score"`
	Quad    Quad    `json:"quad"`
}

func (o Observations) Validate() error {
	if o.SchemaVersion != ObservationsSchema && o.SchemaVersion != ObservationsSchemaV2 && o.SchemaVersion != ObservationsSchemaV3 {
		return fmt.Errorf("unsupported observations schema")
	}
	if o.Image.ID == "" || o.Image.Width < 1 || o.Image.Height < 1 || !isSHA256(o.Image.SHA256) {
		return errors.New("invalid image metadata")
	}
	if o.Image.CoordinateFrame != "exif_normalized_pixels" {
		return errors.New("unsupported coordinate frame")
	}
	if !validRect(o.Document.Bounds, float64(o.Image.Width), float64(o.Image.Height)) {
		return errors.New("invalid document bounds")
	}
	if (o.Document.Method != "contour" && o.Document.Method != "full_fallback" && o.Document.Method != "ocr_bbox_envelope") ||
		(o.Document.Quality != "candidate" && o.Document.Quality != "fallback") {
		return errors.New("invalid document detection metadata")
	}
	if o.Document.SeamY != nil && (math.IsNaN(*o.Document.SeamY) || math.IsInf(*o.Document.SeamY, 0) || *o.Document.SeamY < o.Document.Bounds[1] || *o.Document.SeamY > o.Document.Bounds[3]) {
		return errors.New("invalid page seam")
	}
	passIDs := make(map[string]struct{}, len(o.Passes))
	passesByID := make(map[string]OCRPass, len(o.Passes))
	sourceTileCount := 0
	for _, p := range o.Passes {
		if p.ID == "" || p.ID == "unknown" || !validPassKind(p.Kind) || (p.Status != "ok" && p.Status != "error") ||
			!validRect(p.Crop, float64(o.Image.Width), float64(o.Image.Height)) || math.IsNaN(p.ElapsedSeconds) || math.IsInf(p.ElapsedSeconds, 0) || p.ElapsedSeconds < 0 {
			return errors.New("invalid OCR pass")
		}
		if _, exists := passIDs[p.ID]; exists {
			return errors.New("duplicate OCR pass id")
		}
		if p.Kind == "mrz" && o.SchemaVersion != ObservationsSchemaV2 && o.SchemaVersion != ObservationsSchemaV3 {
			return errors.New("MRZ pass requires observations v2")
		}
		if p.Kind == "source_tile" {
			if o.SchemaVersion != ObservationsSchemaV3 {
				return errors.New("source_tile pass requires observations v3")
			}
			sourceTileCount++
			if sourceTileCount > maxSourceTilePasses || !validSourceTilePass(p) {
				return errors.New("invalid source_tile pass geometry")
			}
		}
		if p.Reader != "" && p.Reader != "rapidocr" && p.Reader != "tesseract-eng" {
			return errors.New("invalid OCR reader")
		}
		if p.Kind != "mrz" && (p.Reader != "" || p.IssueCode != "") {
			return errors.New("MRZ metadata requires an MRZ pass")
		}
		if o.SchemaVersion == ObservationsSchema && (p.Reader != "" || p.IssueCode != "") {
			return errors.New("MRZ pass metadata requires observations v2")
		}
		if p.IssueCode != "" && p.IssueCode != "tesseract_unavailable" && p.IssueCode != "tesseract_timeout" && p.IssueCode != "tesseract_failed" && p.IssueCode != "tesseract_tsv_invalid" && p.IssueCode != "rapidocr_error" {
			return errors.New("invalid OCR pass issue code")
		}
		passIDs[p.ID] = struct{}{}
		passesByID[p.ID] = p
	}
	lineIDs := make(map[string]struct{}, len(o.Lines))
	for _, l := range o.Lines {
		if l.ID == "" || l.PassID == "" || strings.TrimSpace(l.TextRaw) == "" || math.IsNaN(l.Score) || math.IsInf(l.Score, 0) || l.Score < 0 || l.Score > 1 {
			return errors.New("invalid OCR line")
		}
		if _, exists := passIDs[l.PassID]; !exists {
			return errors.New("OCR line refers to unknown pass")
		}
		if pass := passesByID[l.PassID]; pass.Kind == "source_tile" {
			if pass.Status != "ok" || !quadInsideRect(l.Quad, pass.Crop) {
				return errors.New("source_tile line is outside its successful crop")
			}
		}
		if _, exists := lineIDs[l.ID]; exists {
			return errors.New("duplicate OCR line id")
		}
		lineIDs[l.ID] = struct{}{}
		for _, pt := range l.Quad {
			if math.IsNaN(pt[0]) || math.IsNaN(pt[1]) || math.IsInf(pt[0], 0) || math.IsInf(pt[1], 0) || pt[0] < 0 || pt[1] < 0 || pt[0] > float64(o.Image.Width) || pt[1] > float64(o.Image.Height) {
				return errors.New("invalid OCR line geometry")
			}
		}
	}
	return nil
}

func validPassKind(kind string) bool {
	switch kind {
	case "original", "document", "upper_page", "lower_page", "right_margin_cw", "right_margin_ccw", "mrz", "source_tile":
		return true
	default:
		return false
	}
}

func validSourceTilePass(p OCRPass) bool {
	for _, coordinate := range p.Crop {
		if math.Trunc(coordinate) != coordinate {
			return false
		}
	}
	if p.RotationDegrees != 0 {
		return false
	}
	want := Matrix3{{1, 0, p.Crop[0]}, {0, 1, p.Crop[1]}, {0, 0, 1}}
	return p.TransformToImage == want
}

func validRect(r Rect, width, height float64) bool {
	for _, n := range r {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return false
		}
	}
	return r[0] >= 0 && r[1] >= 0 && r[2] > r[0] && r[3] > r[1] && r[2] <= width && r[3] <= height
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
