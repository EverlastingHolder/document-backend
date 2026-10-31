from __future__ import annotations

import unittest
from pathlib import Path
from types import ModuleType
import sys
import tempfile
from unittest import mock

from ocr.runtime import russiandocs_worker


class RussianDocsAdapterTests(unittest.TestCase):
    def test_adapt_maps_all_public_fields_and_preserves_leading_zeroes(self) -> None:
        values = russiandocs_worker.adapt({
            "Last_name_ru": "Ёжов",
            "First_name_ru": "Иван",
            "Middle_name_ru": "Иванович",
            "Sex_ru": "Муж.",
            "Birth_date": "01.02.2003",
            "Birth_place_ru": "Город",
            "Licence_number": "00 07 000042",
            "Issue_date": "2020-01-02",
            "Issue_organization_ru": "Орган",
            "Issue_organisation_code": "001-002",
        })
        self.assertEqual(set(values), set(russiandocs_worker.FIELDS))
        self.assertEqual(values["series"], "0007")
        self.assertEqual(values["number"], "000042")
        self.assertEqual(values["surname"], "Ёжов")

    def test_serial_split_rejects_letters_unicode_digits_and_wrong_length(self) -> None:
        self.assertEqual(russiandocs_worker.split_passport_number("00-07 000042"), ("0007", "000042"))
        for value in ("AB12-34 567890XYZ", "٠٠٠٠٠٠٠٠٠٠", "0007 42", "00070000420"):
            with self.subTest(value=value):
                self.assertEqual(russiandocs_worker.split_passport_number(value), (None, None))

    def test_adapt_treats_non_string_source_values_as_missing(self) -> None:
        values = russiandocs_worker.adapt({"Last_name_ru": ["unexpected"], "Sex_ru": {"value": "М"}})
        self.assertIsNone(values["surname"])
        self.assertIsNone(values["gender"])

    def test_only_pilot_internal_passport_doctypes_are_supported(self) -> None:
        self.assertTrue(russiandocs_worker.supported_passport_doctype("INTPASSPORT_1997"))
        self.assertTrue(russiandocs_worker.supported_passport_doctype("INTPASSPORT_2011"))
        for value in (None, "NONE", "DL_2020", "STS_2019", "INTPASSPORTADDR_ALL"):
            with self.subTest(value=value):
                self.assertFalse(russiandocs_worker.supported_passport_doctype(value))

    def test_model_paths_are_overridden_in_memory_before_pipeline_construction(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / "repo"
            default_models = repo / "document_processing" / "models"
            (default_models / "Borders").mkdir(parents=True)
            model_root = root / "cached" / "document_processing" / "models"
            (model_root / "Borders").mkdir(parents=True)
            package = ModuleType("document_processing")
            package.Pipeline = mock.Mock(return_value="pipeline")
            config = ModuleType("document_processing.config")
            config.DEFAULT_CFG = {"DocDetector": str(default_models / "Borders")}
            config.ROOT = repo / "document_processing"
            with mock.patch.dict(sys.modules, {
                "document_processing": package,
                "document_processing.config": config,
            }):
                pipeline = russiandocs_worker.load_pipeline(repo, model_root)
            self.assertEqual(pipeline, "pipeline")
            package.Pipeline.assert_called_once_with(device="cpu", ocr="accurate")
            self.assertEqual(config.DEFAULT_CFG["DocDetector"], str(model_root.resolve() / "Borders"))

    def test_model_config_paths_outside_repository_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / "repo"
            repo.mkdir()
            model_root = root / "cached"
            model_root.mkdir()
            package = ModuleType("document_processing")
            package.Pipeline = mock.Mock()
            config = ModuleType("document_processing.config")
            config.DEFAULT_CFG = {"DocDetector": str(root / "outside")}
            config.ROOT = repo / "document_processing"
            with mock.patch.dict(sys.modules, {
                "document_processing": package,
                "document_processing.config": config,
            }):
                with self.assertRaisesRegex(RuntimeError, "escaped repository"):
                    russiandocs_worker.load_pipeline(repo, model_root)
            package.Pipeline.assert_not_called()


if __name__ == "__main__":
    unittest.main()
