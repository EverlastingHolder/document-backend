"""Pure helper tests for the bounded detector-resolution runner."""

from __future__ import annotations

import unittest

import numpy as np

import run_resolution_comparison as runner


class ResolutionRunnerTests(unittest.TestCase):
    def test_larger_limit_is_fixed_target_and_requires_lower_installed_default(self) -> None:
        self.assertEqual(runner.larger_limit(736), 1280)
        for value in (1280, 1920):
            with self.subTest(value=value), self.assertRaises(runner.ResolutionComparisonError):
                runner.larger_limit(value)

    def test_bad_installed_default_fails_closed(self) -> None:
        for value in (True, 735.5, "736"):
            with self.subTest(value=value), self.assertRaises(runner.ResolutionComparisonError):
                runner.larger_limit(value)  # type: ignore[arg-type]

    def test_installed_resize_estimate_includes_global_cap_and_detector_minimum(self) -> None:
        config = {
            "global_use_preprocess_img": True,
            "global_min_side_len": 30,
            "global_max_side_len": 2000,
            "global_use_vertical_padding": True,
            "global_min_height": 30,
            "global_width_height_ratio": 8,
            "limit_side_len": 736,
            "limit_type": "min",
        }
        baseline = runner.estimate_detector_pixels(536, 118, config)
        larger = runner.estimate_detector_pixels(536, 118, {**config, "limit_side_len": 1280})
        self.assertEqual(baseline, 3328 * 736)
        self.assertEqual(larger, 5824 * 1280)

    def test_global_resize_is_skipped_inside_bounds_and_matches_cap_and_min_branches(self) -> None:
        config = {
            "global_use_preprocess_img": True,
            "global_min_side_len": 30,
            "global_max_side_len": 2000,
            "global_use_vertical_padding": True,
            "global_min_height": 30,
            "global_width_height_ratio": 8,
            "limit_side_len": 736,
            "limit_type": "min",
        }
        # 536x118 is inside Global min/max, so only detector resize applies.
        self.assertEqual(runner.estimate_detector_pixels(536, 118, config), 3328 * 736)
        # Global max branch truncates before stride rounding: 2560x1280 -> 1984x992.
        self.assertEqual(runner.estimate_detector_pixels(2560, 1280, config), 1984 * 992)
        # Global min branch: 20x10 -> 64x32, then detector minimum -> 1472x736.
        self.assertEqual(runner.estimate_detector_pixels(20, 10, config), 1472 * 736)

    def test_estimator_matches_installed_resize_stages_without_model_session(self) -> None:
        from rapidocr.ch_ppocr_det.utils import DetPreProcess
        from rapidocr.utils.process_img import apply_vertical_padding, resize_image_within_bounds

        config = {
            "global_use_preprocess_img": True,
            "global_min_side_len": 30,
            "global_max_side_len": 2000,
            "global_use_vertical_padding": True,
            "global_min_height": 30,
            "global_width_height_ratio": 8,
            "limit_side_len": 736,
            "limit_type": "min",
        }
        for width, height, side in ((536, 118, 736), (2560, 1280, 736), (20, 10, 736), (536, 118, 1280)):
            with self.subTest(width=width, height=height, side=side):
                actual = np.zeros((height, width, 3), dtype=np.uint8)
                actual, _, _ = resize_image_within_bounds(
                    actual, config["global_min_side_len"], config["global_max_side_len"]
                )
                actual, _ = apply_vertical_padding(
                    actual, {}, config["global_width_height_ratio"], config["global_min_height"]
                )
                detector_shape = DetPreProcess(side, "min").resize(actual).shape
                estimated_pixels = runner.estimate_detector_pixels(width, height, {**config, "limit_side_len": side})
                self.assertEqual(estimated_pixels, detector_shape[0] * detector_shape[1])

    def test_wrong_detector_mode_is_not_estimated(self) -> None:
        config = {"global_use_preprocess_img": True, "global_min_side_len": 30,
                  "global_max_side_len": 2000, "global_use_vertical_padding": True,
                  "global_min_height": 30, "global_width_height_ratio": 8,
                  "limit_side_len": 736, "limit_type": "max"}
        with self.assertRaises(runner.ResolutionComparisonError):
            runner.estimate_detector_pixels(536, 118, config)

    def test_count_helper_returns_only_public_bucket_counts(self) -> None:
        fields = {name: {"suggested_correct": False, "value_present": False} for name in runner.REQUIRED_FIELDS}
        fields[runner.REQUIRED_FIELDS[0]] = {"suggested_correct": True, "value_present": True}
        self.assertEqual(runner.public_required_counts({"fields": fields}), {"correct": 1, "wrong": 0, "abstain": 9})


if __name__ == "__main__":
    unittest.main()
