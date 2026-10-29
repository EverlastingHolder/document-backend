from __future__ import annotations

import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import score_source_tile_comparison as scorer


class SourceTileScorerTests(unittest.TestCase):
    def test_value_scoring_is_state_independent_and_review_overlaps(self):
        gold = {'literal': '2000-02-01'}
        correct = scorer.score_field({'state': 'uncertain', 'value': '01.02.2000', 'candidates': []}, gold, 'birth_date')
        self.assertEqual(correct['selected_status'], 'correct')
        self.assertTrue(correct['manual_review_required'])
        wrong = scorer.score_field({'state': 'uncertain', 'value': '02.01.2000', 'candidates': []}, gold, 'birth_date')
        self.assertEqual(wrong['selected_status'], 'wrong')
        self.assertTrue(wrong['manual_review_required'])
        conflict = scorer.score_field({'state': 'uncertain', 'value': None, 'candidates': [{'value': '01.02.2000'}]}, gold, 'birth_date')
        self.assertEqual(conflict['selected_status'], 'abstain')
        self.assertTrue(conflict['manual_review_required'])
        self.assertTrue(conflict['correct_candidate_available'])
        empty_recognized = scorer.score_field({'state': 'recognized', 'value': '', 'candidates': []}, gold, 'birth_date')
        self.assertEqual(empty_recognized['selected_status'], 'abstain')
        self.assertFalse(empty_recognized['manual_review_required'])

    def test_incomplete_run_fails_before_gold_open(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            manifest = root / 'manifest.json'
            manifest.write_text(json.dumps({'schema_version': scorer.EXPERIMENT_SCHEMA, 'status': 'running'}))
            with self.assertRaisesRegex(scorer.ScoreError, 'experiment_not_completed'):
                scorer.run(manifest, root / 'out')
            self.assertFalse((root / 'out').exists())

    def test_artifact_hash_field_accepts_singular_or_plural_but_not_both(self):
        digest = 'a' * 64
        self.assertEqual(scorer._artifact_digest({'observation_sha256': digest}, 'observations.json'), digest)
        self.assertEqual(scorer._artifact_digest({'observations_sha256': digest}, 'observations.json'), digest)
        self.assertIsNone(scorer._artifact_digest({'observation_sha256': digest, 'observations_sha256': digest}, 'observations.json'))

    def test_recomputed_baseline_counts_and_field_gains_losses_and_candidates(self):
        references = {}
        source_hashes = {}
        before_extractions = {}
        after_rows = []
        slots = [("correct", "right", "right") for _ in range(27)]
        slots += [("wrong", "wrong", "wrong") for _ in range(9)]
        slots += [("abstain", None, None) for _ in range(14)]
        for index, sample in enumerate(scorer.SAMPLES):
            source_hash = f'{index + 1:064x}'
            source_hashes[sample] = source_hash
            fields = {}
            reference_fields = {}
            for field_index, field in enumerate(scorer.FIELDS):
                category, _before, _after = slots[index * 10 + field_index]
                reference_fields[field] = {'literal': 'right'}
                value = 'right' if category == 'correct' else ('wrong' if category == 'wrong' else None)
                fields[field] = {'state': 'uncertain' if category == 'correct' else ('missing' if value is None else 'recognized'),
                                'value': value, 'candidates': [{'value': 'right'}] if category == 'correct' else []}
            reference_fields['patronymic'] = {'literal': ''}
            fields['patronymic'] = {'state': 'missing', 'value': None, 'candidates': []}
            references[source_hash] = {'fields': reference_fields}
            before_extractions[sample] = {'fields': fields}
            after_fields = json.loads(json.dumps(fields))
            if sample == 'sample-01':
                after_fields[scorer.FIELDS[0]] = {'state': 'uncertain', 'value': 'wrong', 'candidates': [{'value': 'right'}]}
            after_rows.append({'sample_id': sample, 'extraction': {'fields': after_fields}})
        after_rows[2]['extraction']['fields'][scorer.FIELDS[7]] = {'state': 'recognized', 'value': 'right', 'candidates': [{'value': 'right'}]}
        report = scorer.build_comparison(after_rows, before_extractions, references, source_hashes)
        self.assertEqual({key: report['before_required'][key] for key in ('correct', 'wrong', 'abstain')},
                         {'correct': 27, 'wrong': 9, 'abstain': 14})
        self.assertEqual(report['per_field'][scorer.FIELDS[0]]['loss'], 1)
        self.assertEqual(report['per_field'][scorer.FIELDS[7]]['gain'], 1)
        self.assertEqual(report['before_required']['manual_review_flags'], 27)
        self.assertGreater(report['before_required']['correct_candidate_available'], 0)

    def test_baseline_count_guard_rejects_changed_reference_or_extraction(self):
        references = {}
        hashes = {}
        before = {}
        after = []
        for index, sample in enumerate(scorer.SAMPLES):
            digest = f'{index + 1:064x}'
            hashes[sample] = digest
            fields = {field: {'state': 'recognized', 'value': 'right', 'candidates': []} for field in scorer.FIELDS}
            fields['patronymic'] = {'state': 'missing', 'value': None, 'candidates': []}
            references[digest] = {'fields': {field: {'literal': 'right'} for field in scorer.FIELDS}}
            before[sample] = {'fields': fields}
            after.append({'sample_id': sample, 'extraction': {'fields': json.loads(json.dumps(fields))}})
        with self.assertRaisesRegex(scorer.ScoreError, 'recomputed_frozen_baseline_metric_mismatch'):
            scorer.build_comparison(after, before, references, hashes)

    def test_source_tile_worker_requires_v3_observations_schema(self):
        row = {'stage': 'source_tiles_and_go_extraction', 'observations_schema': 'passport-ocr-observations/v3',
               'tile_count': 2, 'tile_pass_statuses': ['ok', 'ok']}
        scorer.validate_tile_sample_row(row)
        row['tile_pass_statuses'] = ['ok', 'error']
        scorer.validate_tile_sample_row(row)
        row['tile_pass_statuses'] = ['ok']
        with self.assertRaisesRegex(scorer.ScoreError, 'source_tile_worker_status_invalid'):
            scorer.validate_tile_sample_row(row)
        row['tile_count'], row['tile_pass_statuses'] = 7, ['ok'] * 7
        with self.assertRaisesRegex(scorer.ScoreError, 'source_tile_worker_status_invalid'):
            scorer.validate_tile_sample_row(row)
        row['tile_count'], row['tile_pass_statuses'] = 2, ['ok', 'skipped']
        with self.assertRaisesRegex(scorer.ScoreError, 'source_tile_worker_status_invalid'):
            scorer.validate_tile_sample_row(row)
        row['tile_count'], row['tile_pass_statuses'] = 2, ['ok', 'ok']
        row['observations_schema'] = 'passport-ocr-observations/v2'
        with self.assertRaisesRegex(scorer.ScoreError, 'source_tile_worker_status_invalid'):
            scorer.validate_tile_sample_row(row)

    def _preflight_fixture(self, root: Path):
        preflight_dir = root / 'fixed-preflight-v2'
        evaluator = preflight_dir / 'ocr-eval'
        evaluator.parent.mkdir(parents=True)
        evaluator.write_bytes(b'evaluator')
        parity_hashes = {}
        for sample in scorer.SAMPLES:
            path = preflight_dir / 'baseline-parity' / sample / 'go-extraction.json'
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps({'sample_id': sample}))
            parity_hashes[sample] = scorer.sha256(path)
        hash_groups = {
            'input_sha256': {f'sample-{i:02d}': f'{i:064x}' for i in range(1, 6)},
            'model_sha256': {'model-a': 'a' * 64},
            'go_source_sha256': {'go.go': 'b' * 64},
            'python_source_sha256': {'driver.py': 'c' * 64},
        }
        preflight = {'schema_version': scorer.EXPERIMENT_SCHEMA, 'status': 'preflight_completed',
                     'baseline_parity_gate': 'passed', 'reference_data_read': False,
                     'reference_data_used_for_runtime_or_planning': False,
                     'baseline_audit_sha256': 'd' * 64, 'baseline_manifest_sha256': 'e' * 64,
                     'go_evaluator_sha256': scorer.sha256(evaluator),
                     'baseline_parity_extraction_sha256': parity_hashes}
        manifest = {'baseline_audit_sha256': 'd' * 64, 'baseline_manifest_sha256': 'e' * 64,
                    'go_evaluator_sha256': scorer.sha256(evaluator),
                    'baseline_parity_extraction_sha256': parity_hashes}
        for key, value in hash_groups.items():
            preflight[key] = {'before': value, 'after': value}
            manifest[key] = {'before': value, 'after': value}
        manifest_path = preflight_dir / 'manifest.json'
        manifest_path.write_text(json.dumps(preflight))
        return preflight_dir, manifest_path, manifest, evaluator

    def test_preflight_reuse_is_bound_to_fixed_preflight_manifest_and_hashes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            preflight_dir, manifest_path, manifest, evaluator = self._preflight_fixture(root)
            with patch.object(scorer, 'PREFLIGHT_DIR', preflight_dir), patch.object(scorer, 'PREFLIGHT_MANIFEST', manifest_path):
                actual_evaluator, parity_root = scorer.validate_preflight_binding(manifest, root / 'after-run')
                self.assertEqual(actual_evaluator, evaluator)
                self.assertEqual(parity_root, preflight_dir / 'baseline-parity')
                evaluator.write_bytes(b'wrong evaluator')
                with self.assertRaisesRegex(scorer.ScoreError, 'preflight_evaluator_hash_mismatch'):
                    scorer.validate_preflight_binding(manifest, root / 'after-run')

    def test_preflight_rejects_parity_hash_and_runtime_binding_mismatch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            preflight_dir, manifest_path, manifest, _evaluator = self._preflight_fixture(root)
            with patch.object(scorer, 'PREFLIGHT_DIR', preflight_dir), patch.object(scorer, 'PREFLIGHT_MANIFEST', manifest_path):
                parity = preflight_dir / 'baseline-parity' / 'sample-05' / 'go-extraction.json'
                parity.write_text('tampered')
                with self.assertRaisesRegex(scorer.ScoreError, 'preflight_parity_hash_mismatch'):
                    scorer.validate_preflight_binding(manifest, root / 'after-run')
                parity.write_text(json.dumps({'sample_id': 'sample-05'}))
                manifest['go_source_sha256']['after'] = {'go.go': 'f' * 64}
                with self.assertRaisesRegex(scorer.ScoreError, 'preflight_runtime_binding_mismatch'):
                    scorer.validate_preflight_binding(manifest, root / 'after-run')


if __name__ == '__main__':
    unittest.main()
