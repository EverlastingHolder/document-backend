from __future__ import annotations

import unittest

from ocr.experiments.field_evidence_adapter import FieldEvidenceAdapterError, build_sidecar


def fixture():
    image_sha = "a" * 64
    observations = {
        "schema_version": "passport-ocr-observations/v1",
        "image": {"id": "sample-01", "width": 1000, "height": 1000, "sha256": image_sha, "coordinate_frame": "exif_normalized_pixels"},
        "document": {"bounds": [0, 0, 1000, 1000], "seam_y": 500},
        "passes": [{"id": "original", "kind": "original", "rotation_degrees": 0, "status": "ok"}],
        "lines": [
            {"id": "surname", "pass_id": "original", "text_raw": "ALPHA", "quad": [[700, 600], [760, 600], [760, 610], [700, 610]]},
            {"id": "given", "pass_id": "original", "text_raw": "BETA", "quad": [[701, 620], [761, 620], [761, 630], [701, 630]]},
            {"id": "patronymic", "pass_id": "original", "text_raw": "GAMMA", "quad": [[699, 640], [759, 640], [759, 650], [699, 650]]},
            {"id": "dob", "pass_id": "original", "text_raw": "01.02.2000", "quad": [[700, 690], [760, 690], [760, 700], [700, 700]]},
        ],
    }
    extraction = {"fields": {field: {"evidence": {"line_ids": [line]}, "candidates": [{"line_ids": [line]}]} for field, line in (("surname", "surname"), ("given_name", "given"), ("patronymic", "patronymic"))}}
    plan = {"hypotheses": [{
        "id": "given_name-roi-01", "field_id": "given_name", "role_quality": {"state": "supported_row"},
        "proposal_method": "viz_layout_v1", "source_pass_ids": ["original"], "source_line_ids": ["given"],
        "crop_bounds": [690, 618, 820, 638], "coordinate_frame": "exif_normalized_pixels",
        "adjacent_anchors": [{"role": "birth_date", "line_id": "dob", "pass_id": "original", "relation": "upper_row_boundary"}],
        "reader_plan": [{"reader": "rapidocr", "mode": "rec_only"}],
    }]}
    readers = [{
        "hypothesis_id": "given_name-roi-01", "field_id": "given_name", "reader": "rapidocr", "status": "ok", "candidate_count": 1,
        "outputs": [{"text_raw": "SYNTHETIC_TOKEN", "raw_confidence": 0.8, "geometry_kind": "entire_crop", "box_source": [690, 618, 820, 638]}],
    }]
    run = {"model_sha256": {"eslav_PP-OCRv5_rec_mobile.onnx": "b" * 64}}
    return observations, extraction, plan, readers, run


class FieldEvidenceAdapterTests(unittest.TestCase):
    def test_builds_bound_one_candidate_sidecar_from_cached_reader_output(self):
        sidecar = build_sidecar(*fixture())
        record = sidecar["evidence"][0]
        self.assertEqual("passport-field-evidence/v1", sidecar["schema_version"])
        self.assertEqual(["surname"], record["name_row_line_ids"][0])
        self.assertEqual(["given"], record["name_row_line_ids"][1])
        self.assertEqual(["patronymic"], record["name_row_line_ids"][2])
        self.assertEqual("entire_crop", record["geometry_scope"])
        self.assertEqual("b" * 64, record["reader_model_sha256"])

    def test_rejects_multiple_rapidocr_candidates_with_explicit_code(self):
        args = list(fixture())
        args[3][0]["candidate_count"] = 2
        with self.assertRaises(FieldEvidenceAdapterError) as error:
            build_sidecar(*args)
        self.assertEqual("rapidocr_rec_only_candidate_count_not_one", error.exception.code)

    def test_rejects_glyph_geometry_and_crop_mismatch(self):
        for mutate in (lambda item: item.update(geometry_kind="glyph_box"), lambda item: item.update(box_source=[691, 618, 819, 638])):
            args = list(fixture())
            mutate(args[3][0]["outputs"][0])
            with self.assertRaises(FieldEvidenceAdapterError):
                build_sidecar(*args)

    def test_rejects_invalid_row_anchor_and_mrz_overlap(self):
        args = list(fixture())
        args[2]["hypotheses"][0]["adjacent_anchors"][0]["line_id"] = "missing"
        with self.assertRaises(FieldEvidenceAdapterError):
            build_sidecar(*args)
        args = list(fixture())
        args[0]["lines"].append({"id": "mrz", "pass_id": "original", "text_raw": "<<MRZ<<", "quad": [[700, 620], [800, 620], [800, 635], [700, 635]]})
        with self.assertRaises(FieldEvidenceAdapterError) as error:
            build_sidecar(*args)
        self.assertEqual("roi_intersects_mrz", error.exception.code)


if __name__ == "__main__":
    unittest.main()
