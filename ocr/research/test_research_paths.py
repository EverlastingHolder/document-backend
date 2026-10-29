"""Portability checks for repository code and ignored local research data."""
from __future__ import annotations

import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

_REPO_ROOT = Path(__file__).resolve().parents[2]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))

from ocr.experiments.local_paths import (
    ARCHIVE_OUTPUT_ROOT,
    ARCHIVE_WORK_ROOT,
    PRIVATE_RESEARCH_ROOT,
    REPO_ROOT,
    WORK_ROOT,
    _env_path,
)


class ResearchPathTests(unittest.TestCase):
    def test_private_work_and_output_roots_are_separate_from_source_tree(self) -> None:
        self.assertEqual(WORK_ROOT, ARCHIVE_WORK_ROOT)
        self.assertEqual(ARCHIVE_WORK_ROOT, PRIVATE_RESEARCH_ROOT / "work")
        self.assertEqual(ARCHIVE_OUTPUT_ROOT, PRIVATE_RESEARCH_ROOT / "outputs")
        self.assertFalse(ARCHIVE_WORK_ROOT.is_relative_to(REPO_ROOT / "ocr"))

    def test_private_root_override_resolves_a_temporary_directory(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            override = Path(temp) / "private research"
            with patch.dict(os.environ, {"PASSPORT_OCR_RESEARCH_ROOT": str(override)}):
                resolved = _env_path(
                    "PASSPORT_OCR_RESEARCH_ROOT",
                    REPO_ROOT / ".local" / "ocr-research",
                )
            self.assertEqual(resolved, override.resolve())


if __name__ == "__main__":
    unittest.main()
