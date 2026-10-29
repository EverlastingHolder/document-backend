package passport

import (
	"fmt"
	"testing"
)

func sourceTileObservations() Observations {
	input := syntheticObservations()
	input.SchemaVersion = ObservationsSchemaV3
	input.Passes = append(input.Passes, OCRPass{
		ID: "tile-1", Kind: "source_tile", Crop: Rect{100, 200, 500, 400},
		TransformToImage: Matrix3{{1, 0, 100}, {0, 1, 200}, {0, 0, 1}}, Status: "ok",
	})
	return input
}

func TestV3AcceptsSourceTileAndExtractsSourceCoordinatesWithTileProvenance(t *testing.T) {
	input := sourceTileObservations()
	input.Lines = append(input.Lines, OCRLine{
		ID: "tile-surname", PassID: "tile-1", TextRaw: "ФАМИЛИЯ: ПЕТРОВ", Score: 0.9,
		Quad: Quad{{150, 250}, {310, 250}, {310, 270}, {150, 270}},
	})
	if err := input.Validate(); err != nil {
		t.Fatalf("valid v3 source tile rejected: %v", err)
	}
	defaultResult, err := Extract(input, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if defaultResult.Fields["surname"].Value != nil {
		t.Fatal("default original-only pass set consumed source_tile observations")
	}
	result, err := Extract(input, Options{PassSet: "all"})
	if err != nil {
		t.Fatal(err)
	}
	field := result.Fields["surname"]
	if field.Value == nil || *field.Value != "ПЕТРОВ" {
		t.Fatalf("source tile line was not used as source-frame VIZ: %#v", field)
	}
	if len(field.Evidence.PassIDs) != 1 || field.Evidence.PassIDs[0] != "tile-1" ||
		len(field.Evidence.LineIDs) != 1 || field.Evidence.LineIDs[0] != "tile-surname" {
		t.Fatalf("source provenance not preserved: %#v", field.Evidence)
	}
}

func TestV1AndV2RejectSourceTileKind(t *testing.T) {
	for _, version := range []string{ObservationsSchema, ObservationsSchemaV2} {
		t.Run(version, func(t *testing.T) {
			input := sourceTileObservations()
			input.SchemaVersion = version
			if err := input.Validate(); err == nil {
				t.Fatal("legacy observations schema accepted source_tile")
			}
		})
	}
}

func TestSourceTileRejectsRotationTranslationAndOutOfBoundsCrop(t *testing.T) {
	mutations := []struct {
		name   string
		change func(*OCRPass)
	}{
		{name: "rotation", change: func(p *OCRPass) { p.RotationDegrees = 90 }},
		{name: "translation", change: func(p *OCRPass) { p.TransformToImage[0][2]++ }},
		{name: "outside image", change: func(p *OCRPass) { p.Crop[2] = 1001 }},
		{name: "fractional crop", change: func(p *OCRPass) { p.Crop[0] = 100.5 }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			input := sourceTileObservations()
			mutation.change(&input.Passes[1])
			if err := input.Validate(); err == nil {
				t.Fatal("invalid source tile geometry was accepted")
			}
		})
	}
}

func TestSourceTileLinesMustStayInsideTheirSuccessfulCrop(t *testing.T) {
	input := sourceTileObservations()
	input.Lines = append(input.Lines, OCRLine{
		ID: "tile-line", PassID: "tile-1", TextRaw: "SYNTHETIC", Score: 0.8,
		Quad: Quad{{90, 250}, {150, 250}, {150, 270}, {90, 270}},
	})
	if err := input.Validate(); err == nil {
		t.Fatal("source tile line extending outside its crop was accepted")
	}
	input.Lines[0].Quad = Quad{{150, 250}, {180, 250}, {180, 270}, {150, 270}}
	input.Passes[1].Status = "error"
	if err := input.Validate(); err == nil {
		t.Fatal("source tile error pass with a line was accepted")
	}
}

func TestV3AllowsAtMostSixSourceTiles(t *testing.T) {
	input := sourceTileObservations()
	for index := 2; index <= 6; index++ {
		input.Passes = append(input.Passes, OCRPass{
			ID: fmt.Sprintf("tile-%d", index), Kind: "source_tile", Crop: Rect{100, 200, 500, 400},
			TransformToImage: Matrix3{{1, 0, 100}, {0, 1, 200}, {0, 0, 1}}, Status: "ok",
		})
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("six source tiles rejected: %v", err)
	}
	input.Passes = append(input.Passes, OCRPass{
		ID: "tile-7", Kind: "source_tile", Crop: Rect{100, 200, 500, 400},
		TransformToImage: Matrix3{{1, 0, 100}, {0, 1, 200}, {0, 0, 1}}, Status: "ok",
	})
	if err := input.Validate(); err == nil {
		t.Fatal("seven source tiles were accepted")
	}
}

func TestV3PreservesMRZValidationAndReaderEnum(t *testing.T) {
	input, _ := mrzTestObservation(t, true)
	input.SchemaVersion = ObservationsSchemaV3
	if err := input.Validate(); err != nil {
		t.Fatalf("v3 should retain v2 MRZ validation: %v", err)
	}
	input.Passes[1].Reader = "unsupported-reader"
	if err := input.Validate(); err == nil {
		t.Fatal("v3 accepted unsupported MRZ reader")
	}
	input.Passes[1].Reader = "rapidocr"
	input.Passes[1].IssueCode = "unsupported-issue"
	if err := input.Validate(); err == nil {
		t.Fatal("v3 accepted unsupported MRZ issue code")
	}
	input.Passes[1].IssueCode = "rapidocr_error"
	input.Passes[1].Status = "error"
	if err := input.Validate(); err != nil {
		t.Fatalf("known MRZ reader and issue enums rejected in v3: %v", err)
	}
	input.Passes[1].Kind = "source_tile"
	if err := input.Validate(); err == nil {
		t.Fatal("source tile accepted MRZ-only reader metadata")
	}
}

func TestV3MRZValidAndInvalidPairsKeepV2Checks(t *testing.T) {
	valid, pair := mrzTestObservation(t, true)
	valid.SchemaVersion = ObservationsSchemaV3
	result, err := Extract(valid, Options{PassSet: "all", MRZPolicy: "augment"})
	if err != nil || result.MRZ == nil || result.MRZ.Status != "valid" {
		t.Fatalf("v3 failed to retain valid v2 MRZ behavior: result=%#v err=%v", result.MRZ, err)
	}
	invalid := valid
	invalid.Lines = mrzTestLines([2]string{pair[0], pair[1][:9] + "0" + pair[1][10:]})
	result, err = Extract(invalid, Options{PassSet: "all", MRZPolicy: "augment"})
	if err != nil || result.MRZ == nil || result.MRZ.Status != "invalid" {
		t.Fatalf("v3 failed to retain v2 invalid-check behavior: result=%#v err=%v", result.MRZ, err)
	}
}
