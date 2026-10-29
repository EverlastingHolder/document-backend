package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"passport-backend/internal/passport"
)

func main() {
	inputPath := flag.String("input", "", "OCR observations JSON file")
	outputPath := flag.String("output", "", "private extraction report JSON file")
	fieldEvidencePath := flag.String("field-evidence", "", "optional private supported-name ROI evidence sidecar JSON file")
	templateEvidencePath := flag.String("template-evidence", "", "optional private registered template crop proposals")
	vlPlanOutputPath := flag.String("vl-plan-output", "", "optional private Go-generated VL crop plan JSON file")
	vlEvidencePath := flag.String("vl-evidence", "", "optional private PaddleOCR-VL evidence sidecar JSON file")
	vlModelSHA256 := flag.String("vl-model-sha256", "", "expected local PaddleOCR-VL model-tree SHA-256")
	vlPolicy := flag.String("vl-policy", "union", "VL merge policy: union or experimental primary")
	passSet := flag.String("passes", "original", "pass selection: original or all")
	normalizeNames := flag.Bool("normalize-names", false, "propose known visual Latin-to-Cyrillic name normalization")
	mrzPolicy := flag.String("mrz-policy", "off", "MRZ field policy: off or augment")
	flag.Parse()
	if *inputPath == "" || *outputPath == "" {
		fail("input and output are required")
	}
	if err := runWithAllEvidenceAndVL(*inputPath, *outputPath, *fieldEvidencePath, *templateEvidencePath, *vlPlanOutputPath, *vlEvidencePath, *vlModelSHA256, *vlPolicy, *passSet, *normalizeNames, *mrzPolicy); err != nil {
		fail("offline extraction failed")
	}
	// Deliberately omit image IDs, OCR text, extracted values, confidence, and
	// filesystem paths from stdout.
	fmt.Println("completed fields=11 review_required=true")
}

func run(inputPath, outputPath, passSet string, normalizeNames bool, mrzPolicy string) error {
	return runWithFieldEvidence(inputPath, outputPath, "", passSet, normalizeNames, mrzPolicy)
}

func runWithFieldEvidence(inputPath, outputPath, fieldEvidencePath, passSet string, normalizeNames bool, mrzPolicy string) error {
	return runWithAllEvidence(inputPath, outputPath, fieldEvidencePath, "", passSet, normalizeNames, mrzPolicy)
}

func runWithAllEvidence(inputPath, outputPath, fieldEvidencePath, templateEvidencePath, passSet string, normalizeNames bool, mrzPolicy string) error {
	return runWithAllEvidenceAndVL(inputPath, outputPath, fieldEvidencePath, templateEvidencePath, "", "", "", "union", passSet, normalizeNames, mrzPolicy)
}

func runWithAllEvidenceAndVL(inputPath, outputPath, fieldEvidencePath, templateEvidencePath, vlPlanOutputPath, vlEvidencePath, vlModelSHA256, vlPolicy, passSet string, normalizeNames bool, mrzPolicy string) error {
	if vlEvidencePath != "" && vlModelSHA256 == "" {
		return fmt.Errorf("vl-model-sha256 is required with vl-evidence")
	}
	if sameNonemptyPath(vlPlanOutputPath, vlEvidencePath) || sameNonemptyPath(vlPlanOutputPath, outputPath) ||
		sameNonemptyPath(vlPlanOutputPath, inputPath) || sameNonemptyPath(vlPlanOutputPath, fieldEvidencePath) || sameNonemptyPath(vlPlanOutputPath, templateEvidencePath) || sameNonemptyPath(outputPath, vlEvidencePath) {
		return fmt.Errorf("VL plan and extraction output paths must differ from inputs")
	}
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()
	var input passport.Observations
	decoder := json.NewDecoder(in)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return err
	}
	if err := ensureEOF(decoder); err != nil {
		return err
	}
	var evidence *passport.FieldEvidence
	if fieldEvidencePath != "" {
		info, err := os.Stat(fieldEvidencePath)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("field evidence file must be private")
		}
		evidenceFile, err := os.Open(fieldEvidencePath)
		if err != nil {
			return err
		}
		defer evidenceFile.Close()
		evidenceDecoder := json.NewDecoder(evidenceFile)
		evidenceDecoder.DisallowUnknownFields()
		var parsed passport.FieldEvidence
		if err := evidenceDecoder.Decode(&parsed); err != nil {
			return err
		}
		if err := ensureEOF(evidenceDecoder); err != nil {
			return err
		}
		evidence = &parsed
	}
	result, err := passport.ExtractWithFieldEvidence(input, passport.Options{PassSet: passSet, NormalizeNames: normalizeNames, MRZPolicy: mrzPolicy}, evidence)
	if err != nil {
		return err
	}
	if templateEvidencePath != "" {
		info, err := os.Stat(templateEvidencePath)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("template evidence file must be private")
		}
		file, err := os.Open(templateEvidencePath)
		if err != nil {
			return err
		}
		defer file.Close()
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		var template passport.TemplateEvidence
		if err := decoder.Decode(&template); err != nil {
			return err
		}
		if err := ensureEOF(decoder); err != nil {
			return err
		}
		result, err = passport.ApplyTemplateEvidence(input, passport.Options{PassSet: passSet, NormalizeNames: normalizeNames, MRZPolicy: mrzPolicy}, result, &template)
		if err != nil {
			return err
		}
	}
	if vlPlanOutputPath != "" || vlEvidencePath != "" {
		plan, err := passport.BuildVLFieldPlan(input, result)
		if err != nil {
			return err
		}
		if vlPlanOutputPath != "" {
			if err := writePrivateJSONFile(vlPlanOutputPath, plan); err != nil {
				return err
			}
		}
		if vlEvidencePath != "" {
			file, err := openPrivateJSON(vlEvidencePath)
			if err != nil {
				return err
			}
			defer file.Close()
			decoder := json.NewDecoder(file)
			decoder.DisallowUnknownFields()
			var evidence passport.VLFieldEvidence
			if err := decoder.Decode(&evidence); err != nil {
				return err
			}
			if err := ensureEOF(decoder); err != nil {
				return err
			}
			result, err = passport.ApplyVLFieldEvidence(input, passport.Options{PassSet: passSet, NormalizeNames: normalizeNames, MRZPolicy: mrzPolicy}, result, &evidence, passport.VLFieldOptions{ModelTreeSHA256: vlModelSHA256, Policy: vlPolicy})
			if err != nil {
				return err
			}
		}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		return err
	}
	info, err := os.Stat(outputDir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("output directory must be private")
	}
	out, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := out.Chmod(0600); err != nil {
		out.Close()
		return err
	}
	if _, err := out.Write(encoded); err != nil {
		out.Close()
		return err
	}
	if _, err := out.Write([]byte("\n")); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sameNonemptyPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return filepath.Clean(absA) == filepath.Clean(absB)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func openPrivateJSON(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("evidence file must be private")
	}
	return os.Open(path)
}

func writePrivateJSONFile(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	outputDir := filepath.Dir(path)
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		return err
	}
	info, err := os.Stat(outputDir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("private output directory required")
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := out.Chmod(0600); err != nil {
		out.Close()
		return err
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values")
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
}
