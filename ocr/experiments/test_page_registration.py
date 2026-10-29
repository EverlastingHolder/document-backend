from __future__ import annotations

import unittest

import cv2
import numpy as np

from ocr.experiments.page_registration import (
    _build_pages,
    _order_quad,
    _same_quad,
    _red_band_seam,
    _side_boundary_profile,
    _source_to_working_points,
    _working_to_source_points,
    map_page_quad_to_original,
    register_spread,
    warp_registered_page,
)


def _synthetic_spread(
    *,
    size: tuple[int, int] = (1000, 900),
    quad: np.ndarray | None = None,
    red_seam: bool = True,
) -> tuple[np.ndarray, list[list[list[float]]]]:
    width, height = size
    image = np.full((height, width, 3), 245, dtype=np.uint8)
    if quad is None:
        quad = np.array([[150, 70], [850, 95], [825, 830], [175, 810]], dtype=np.int32)
    cv2.fillConvexPoly(image, quad, (235, 230, 220))
    cv2.polylines(image, [quad], True, (35, 30, 25), 12, lineType=cv2.LINE_AA)
    if red_seam:
        cv2.line(image, (int(quad[0, 0] + 15), 460), (int(quad[1, 0] - 15), 478), (190, 35, 30), 16, lineType=cv2.LINE_AA)
    text_quads = []
    for row, y in enumerate((155, 250, 355, 555, 655, 750)):
        x = 330 if row % 2 == 0 else 400
        text_quads.append([[x, y], [x + 100, y], [x + 100, y + 18], [x, y + 18]])
    return image, text_quads


class PageRegistrationTests(unittest.TestCase):
    def test_order_quad_returns_stable_clockwise_corners(self) -> None:
        ordered = _order_quad(np.array([[80, 80], [240, 100], [220, 260], [70, 230]]))
        self.assertIsNotNone(ordered)
        self.assertEqual(ordered.tolist(), [[80.0, 80.0], [240.0, 100.0], [220.0, 260.0], [70.0, 230.0]])

    def test_perspective_pages_map_back_to_source(self) -> None:
        source_quad = np.array([[140, 65], [865, 102], [820, 832], [172, 801]], dtype=np.float64)
        pages = _build_pages(source_quad, 0.54, (1000, 900))
        self.assertIsNotNone(pages)
        assert pages is not None
        upper, lower = pages
        for page in (upper, lower):
            width, height = page.output_size
            corners = [[0, 0], [width - 1, 0], [width - 1, height - 1], [0, height - 1]]
            mapped = map_page_quad_to_original(corners, page)
            self.assertIsNotNone(mapped)
            self.assertTrue(np.isfinite(np.asarray(mapped)).all())
            self.assertGreaterEqual(min(point[0] for point in mapped), 0)
            self.assertGreaterEqual(min(point[1] for point in mapped), 0)
            self.assertLessEqual(max(point[0] for point in mapped), 1000)
            self.assertLessEqual(max(point[1] for point in mapped), 900)
            matrix = np.asarray(page.original_to_page)
            inverse = np.asarray(page.page_to_original)
            np.testing.assert_allclose(matrix @ inverse, np.eye(3), atol=1e-5)

    def test_odd_rounded_resize_uses_actual_nonuniform_pixel_center_scales(self) -> None:
        source_width, source_height = 2561, 1703
        scale = 1200 / max(source_width, source_height)
        working_width = int(round(source_width * scale))
        working_height = int(round(source_height * scale))
        scale_x, scale_y = working_width / source_width, working_height / source_height
        self.assertNotEqual(scale_x, scale_y)
        source = np.array([[0, 0], [2560, 0], [2560, 1702], [1281, 851]], dtype=np.float64)
        working = _source_to_working_points(source, scale_x, scale_y)
        recovered = _working_to_source_points(working, scale_x, scale_y)
        np.testing.assert_allclose(recovered, source, atol=1e-10)

    def test_mapping_rejects_quads_outside_page_or_source_bounds(self) -> None:
        pages = _build_pages(np.array([[20, 20], [480, 20], [480, 580], [20, 580]], dtype=np.float64), 0.52, (500, 600))
        self.assertIsNotNone(pages)
        assert pages is not None
        page = pages[0]
        width, height = page.output_size
        self.assertIsNone(map_page_quad_to_original([[-30, 10], [10, 10], [10, 30], [-30, 30]], page))
        self.assertIsNone(map_page_quad_to_original([[10, 10], [width + 5, 10], [width + 5, 30], [10, 30]], page))
        # A continuous OCR box touching the far image edge may differ by at
        # most the documented one-pixel tolerance; large excursions are rejected.
        self.assertIsNone(map_page_quad_to_original([[10, 10], [width + 1.1, 10], [width + 1.1, 30], [10, 30]], page))

    def test_supported_physical_quad_and_red_seam_are_accepted_provisionally(self) -> None:
        image, _ = _synthetic_spread()
        before = image.copy()
        result = register_spread(image, text_quads=[])
        np.testing.assert_array_equal(image, before)
        self.assertEqual(result.status, "accepted")
        self.assertEqual(result.reason, "provisional_quad_and_seam_supported")
        self.assertEqual(len(result.pages), 2)
        self.assertEqual(result.selected_candidate_source, "contour")
        self.assertEqual({page.name for page in result.pages}, {"image_upper", "image_lower"})
        self.assertEqual(result.orientation_status, "orientation_unverified")
        self.assertTrue(bool(np.isfinite(np.asarray(result.source_quad)).all()))
        np.testing.assert_allclose(np.asarray(result.source_quad), [[150, 70], [850, 95], [825, 830], [175, 810]], atol=8.0)
        rectified = warp_registered_page(image, result.pages[0])
        self.assertEqual(rectified.shape[1], result.pages[0].output_size[0])

    def test_missing_red_seam_is_rejected_instead_of_assuming_midpoint(self) -> None:
        image, text_quads = _synthetic_spread(red_seam=False)
        result = register_spread(image, text_quads=text_quads)
        self.assertEqual(result.status, "rejected")
        self.assertEqual(result.reason, "supported_seam_not_found")
        self.assertIsNone(result.seam_ratio)

    def test_compact_red_stamp_is_not_accepted_as_a_seam(self) -> None:
        image = np.full((400, 800, 3), 235, dtype=np.uint8)
        cv2.rectangle(image, (280, 165), (520, 235), (190, 30, 25), -1)
        ratio, support, reason = _red_band_seam(image)
        self.assertIsNone(ratio)
        self.assertIsNone(support)
        self.assertEqual(reason, "supported_seam_not_found")

    def test_two_separated_supported_bands_are_ambiguous(self) -> None:
        image = np.full((400, 800, 3), 235, dtype=np.uint8)
        cv2.line(image, (35, 145), (765, 145), (190, 30, 25), 12)
        cv2.line(image, (35, 255), (765, 255), (190, 30, 25), 12)
        ratio, support, reason = _red_band_seam(image)
        self.assertIsNone(ratio)
        self.assertGreater(support, 0)
        self.assertEqual(reason, "seam_ambiguous")

    def test_long_dark_fold_is_supported_without_red_color(self) -> None:
        image = np.full((400, 800, 3), 235, dtype=np.uint8)
        cv2.line(image, (35, 200), (765, 200), (45, 45, 45), 10, lineType=cv2.LINE_AA)
        ratio, support, reason = _red_band_seam(image)
        self.assertEqual(reason, "supported_seam")
        self.assertIsNotNone(ratio)
        self.assertAlmostEqual(float(ratio), 0.5, delta=0.03)
        self.assertGreater(float(support or 0), 0.5)

    def test_black_frame_touching_image_edge_is_not_a_physical_page_candidate(self) -> None:
        image = np.full((900, 1000, 3), 245, dtype=np.uint8)
        cv2.rectangle(image, (0, 0), (999, 899), (10, 10, 10), 20)
        quad = np.array([[180, 70], [820, 92], [800, 830], [195, 810]], dtype=np.int32)
        cv2.fillConvexPoly(image, quad, (235, 230, 220))
        cv2.polylines(image, [quad], True, (35, 30, 25), 12, lineType=cv2.LINE_AA)
        cv2.line(image, (190, 460), (810, 478), (190, 35, 30), 16, lineType=cv2.LINE_AA)
        result = register_spread(image, text_quads=[])
        self.assertEqual(result.status, "accepted")
        assert result.source_quad is not None
        xs = [point[0] for point in result.source_quad]
        ys = [point[1] for point in result.source_quad]
        self.assertGreater(min(xs), 50)
        self.assertGreater(min(ys), 30)
        self.assertLess(max(xs), 950)
        self.assertLess(max(ys), 870)

    def test_boundary_profile_distinguishes_paper_texture_from_blank_background(self) -> None:
        image = np.full((300, 400, 3), 248, dtype=np.uint8)
        quad = np.array([[80, 40], [320, 48], [310, 260], [88, 250]], dtype=np.int32)
        cv2.fillConvexPoly(image, quad, (228, 222, 216))
        for y in range(80, 230, 8):
            cv2.line(image, (105, y), (290, y), (190, 185, 180), 1)
        contrast, texture = _side_boundary_profile(quad.astype(np.float64), image)
        self.assertGreater(min(contrast), 0.36)
        self.assertGreaterEqual(len(texture), 4)

    def test_polygon_dedup_keeps_minor_jitter_but_not_alternate_perspective(self) -> None:
        base = np.array([[100, 100], [800, 110], [780, 800], [120, 790]], dtype=np.float64)
        jitter = base + np.array([[0.3, -0.2], [0.2, 0.1], [-0.3, 0.2], [-0.1, -0.2]])
        alternate = np.array([[100, 180], [800, 110], [780, 800], [120, 710]], dtype=np.float64)
        self.assertTrue(_same_quad(base, jitter))
        self.assertFalse(_same_quad(base, alternate))

    def test_ninety_degree_spread_is_rejected_but_180_degree_status_stays_unverified(self) -> None:
        image = np.full((850, 1200, 3), 245, dtype=np.uint8)
        quad = np.array([[100, 160], [1100, 160], [1090, 690], [110, 690]], dtype=np.int32)
        cv2.fillConvexPoly(image, quad, (235, 230, 220))
        cv2.polylines(image, [quad], True, (35, 30, 25), 12, lineType=cv2.LINE_AA)
        text_quads = []
        for row, y in enumerate((220, 285, 350, 480, 545, 610)):
            x = 300 if row % 2 == 0 else 650
            text_quads.append([[x, y], [x + 100, y], [x + 100, y + 18], [x, y + 18]])
        result = register_spread(image, text_quads=text_quads)
        self.assertEqual(result.status, "rejected")
        self.assertEqual(result.reason, "orientation_outside_supported_range")

    def test_ocr_geometry_without_physical_contours_does_not_create_quad(self) -> None:
        image = np.full((900, 1000, 3), 245, dtype=np.uint8)
        text_quads = [
            [[300, y], [400, y], [400, y + 18], [300, y + 18]]
            for y in (150, 250, 350, 550, 650, 750)
        ]
        result = register_spread(image, text_quads=text_quads)
        self.assertEqual(result.status, "rejected")
        self.assertIsNone(result.source_quad)

    def test_separate_comparable_spreads_are_ambiguous(self) -> None:
        image = np.full((1000, 1200, 3), 245, dtype=np.uint8)
        text_quads = []
        for x0 in (45, 655):
            quad = np.array([[x0, 90], [x0 + 500, 100], [x0 + 490, 920], [x0 + 10, 910]], dtype=np.int32)
            cv2.fillConvexPoly(image, quad, (235, 230, 220))
            cv2.polylines(image, [quad], True, (25, 25, 25), 12, lineType=cv2.LINE_AA)
            cv2.line(image, (x0 + 20, 500), (x0 + 480, 510), (190, 35, 30), 16, lineType=cv2.LINE_AA)
            for row, y in enumerate((180, 300, 420, 600, 720, 830)):
                x = x0 + (100 if row % 2 == 0 else 210)
                text_quads.append([[x, y], [x + 90, y], [x + 90, y + 16], [x, y + 16]])
        result = register_spread(image, text_quads=text_quads)
        self.assertEqual(result.status, "ambiguous")
        self.assertEqual(result.reason, "multiple_comparable_or_nested_physical_boundaries")
        self.assertGreaterEqual(result.candidate_count, 2)


if __name__ == "__main__":
    unittest.main()
