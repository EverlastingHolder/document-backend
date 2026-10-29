from __future__ import annotations

import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import score_five_images as scorer


class ScorerTests(unittest.TestCase):
    def make_baseline(self, root: Path):
        sources = {}
        inputs = {}
        samples = []
        for index in range(1, 6):
            sample_id = f'sample-{index:02d}'
            filename = 'image-01.jpg' if index == 1 else 'image-02.jpg' if index == 2 else f'sample0{index}.jpg'
            source = root / 'sources' / filename
            source.parent.mkdir(parents=True, exist_ok=True)
            source.write_bytes(f'synthetic-source-{index}'.encode())
            sources[sample_id] = source
            source_hash = scorer.sha256(source)
            inputs[sample_id] = source_hash
            sample_dir = root / 'run' / sample_id
            sample_dir.mkdir(parents=True, exist_ok=True)
            if index in (1, 3):
                registered_hash = 'b' * 64
                image_id = f'{sample_id}:registered-spread'
                observation = {'image': {'id': image_id, 'sha256': registered_hash},
                               'runtime': {'registered_spread': {'status': 'accepted',
                                'coordinate_space_role': 'derived_registered_spread',
                                'source_image_sha256': source_hash, 'canonical_image_sha256': registered_hash}}}
            else:
                image_id = sample_id
                observation = {'image': {'id': image_id, 'sha256': source_hash}, 'runtime': {'registered_spread': None}}
            extraction = {'image_id': image_id}
            obs_path = sample_dir / 'observations.json'
            ext_path = sample_dir / 'go-extraction.json'
            obs_path.write_text(json.dumps(observation))
            ext_path.write_text(json.dumps(extraction))
            samples.append({'sample_id': sample_id, 'status': 'completed',
                            'observation_sha256': scorer.sha256(obs_path), 'extraction_sha256': scorer.sha256(ext_path)})
        hashes = {'synthetic-model': 'c' * 64}
        code = {'synthetic-source.py': 'd' * 64}
        manifest = {'schema_version': scorer.SCHEMA, 'status': 'completed', 'reference_data_read': False,
                    'reference_data_used_for_runtime_or_planning': False,
                    'inputs_sha256': {'before': inputs, 'after': dict(inputs)},
                    'models_sha256': {'before': hashes, 'after': dict(hashes)},
                    'source_code_sha256': {'before': code, 'after': dict(code)},
                    'integrity': {'inputs_unchanged': True, 'models_unchanged': True, 'source_code_unchanged': True},
                    'samples': samples}
        manifest_path = root / 'run' / 'manifest.json'
        manifest_path.write_text(json.dumps(manifest))
        return manifest_path, sources

    def test_duplicate_source_hash_is_rejected(self):
        rows = [{'source_sha256': 'a' * 64}] * 5
        with self.assertRaisesRegex(scorer.ScoreError, 'duplicate_source_hash'):
            scorer.index_reference({'images': {}}, rows, Path('.'), {'sample-01', 'sample-02'})

    def test_unready_manifest_is_rejected_before_reference_open(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = root / 'manifest.json'
            manifest.write_text(json.dumps({'schema_version': scorer.SCHEMA, 'status': 'running'}))
            missing_old_gold = root / 'old-must-not-be-opened.json'
            missing_new_gold = root / 'new-must-not-be-opened.json'
            with self.assertRaisesRegex(scorer.ScoreError, 'baseline_not_completed'):
                scorer.run(manifest, missing_old_gold, missing_new_gold, root / 'out')
            self.assertFalse((root / 'out').exists())

    def test_date_canonical_normalization(self):
        self.assertEqual(scorer.canonical('  01.02.2000 ', 'birth_date'), '2000-02-01')
        self.assertEqual(scorer.canonical('2000-02-01', 'birth_date'), '2000-02-01')

    def test_selected_value_status_is_independent_from_manual_review(self):
        expected = {'literal': '2000-02-01'}
        correct = scorer.score_field({'state': 'uncertain', 'value': '01.02.2000', 'candidates': []}, expected, 'birth_date')
        self.assertEqual(correct['status'], 'correct')
        self.assertTrue(correct['manual_review_required'])
        wrong = scorer.score_field({'state': 'uncertain', 'value': '02.01.2000', 'candidates': []}, expected, 'birth_date')
        self.assertEqual(wrong['status'], 'wrong')
        self.assertTrue(wrong['manual_review_required'])
        conflict = scorer.score_field({'state': 'uncertain', 'value': None, 'candidates': [{'value': '01.02.2000'}]}, expected, 'birth_date')
        self.assertEqual(conflict['status'], 'abstain')
        self.assertTrue(conflict['manual_review_required'])
        self.assertTrue(conflict['correct_candidate_available'])
        missing = scorer.score_field({'state': 'missing', 'value': '', 'candidates': []}, expected, 'birth_date')
        self.assertEqual(missing['status'], 'abstain')
        self.assertFalse(missing['manual_review_required'])
        empty_recognized = scorer.score_field({'state': 'recognized', 'value': None, 'candidates': []}, expected, 'birth_date')
        self.assertEqual(empty_recognized['status'], 'abstain')

    def test_current_manifest_hash_bundles_and_split_gold_mapping(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, sources = self.make_baseline(root)
            completed = scorer.validate_completed_baseline(manifest, sources)
            self.assertEqual(len(completed), 5)
            self.assertNotEqual('b' * 64, completed[0]['source_sha256'])
            old = {'images': {'image01': {'fields': {}}, 'image02': {'fields': {}}}}
            new = {'images': {'sample03': {'fields': {}}, 'sample04': {'fields': {}}, 'sample05': {'fields': {}}}}
            old_index = scorer.index_reference(old, completed, root, {'sample-01', 'sample-02'}, sources)
            new_index = scorer.index_reference(new, completed, root, {'sample-03', 'sample-04', 'sample-05'}, sources)
            self.assertEqual(len(old_index), 2)
            self.assertEqual(len(new_index), 3)

    def test_baseline_requires_registered_observation_source_lineage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, sources = self.make_baseline(root)
            sample01 = root / 'run/sample-01/observations.json'
            observation = json.loads(sample01.read_text())
            observation['runtime']['registered_spread']['source_image_sha256'] = 'f' * 64
            sample01.write_text(json.dumps(observation))
            manifest_data = json.loads(manifest.read_text())
            manifest_data['samples'][0]['observation_sha256'] = scorer.sha256(sample01)
            manifest.write_text(json.dumps(manifest_data))
            with self.assertRaisesRegex(scorer.ScoreError, 'observation_registered_lineage_mismatch'):
                scorer.validate_completed_baseline(manifest, sources)

    def test_integrity_audit_accepts_exact_binding_and_rejects_tampering(self):
        audit = {'schema_version': scorer.AUDIT_SCHEMA, 'original_manifest_sha256': 'a' * 64,
                 'changed_files': [{'path': 'api-file', 'before_sha256': 'b' * 64, 'after_sha256': 'c' * 64}],
                 'go_dependency_closure': {'command': scorer.GO_DEP_COMMAND, 'result': scorer.EXPECTED_GO_CLOSURE},
                 'runtime_code_sha256': {'runtime.go': 'd' * 64}}
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = root / 'manifest.json'; manifest.write_text('{}')
            audit_path = root / 'audit.json'; audit_path.write_text(json.dumps(audit))
            with patch.object(scorer, 'build_integrity_audit', return_value=audit):
                scorer.verify_integrity_audit(manifest, audit_path)
            for tamper in ('manifest_hash', 'runtime_hash', 'unlisted_change'):
                modified = json.loads(json.dumps(audit))
                if tamper == 'manifest_hash':
                    modified['original_manifest_sha256'] = 'e' * 64
                elif tamper == 'runtime_hash':
                    modified['runtime_code_sha256']['runtime.go'] = 'f' * 64
                else:
                    modified['changed_files'].append({'path': 'extra.go', 'before_sha256': '1' * 64, 'after_sha256': '2' * 64})
                audit_path.write_text(json.dumps(modified))
                with patch.object(scorer, 'build_integrity_audit', return_value=audit):
                    with self.assertRaisesRegex(scorer.ScoreError, 'integrity_audit_mismatch'):
                        scorer.verify_integrity_audit(manifest, audit_path)


if __name__ == '__main__':
    unittest.main()
