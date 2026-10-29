import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
from ocr.experiments import run_template_field_comparison as runner
from ocr.experiments.run_registration_first_comparison import RunnerError


class TemplateRunnerTests(unittest.TestCase):
    def test_reference_never_opens_before_all_six_outputs(self):
        arms={key:{} for key in ('source_baseline','registered_baseline','template_augmented')}
        rows=[{'sample_id':'sample-01','source_sha':'a','arms':arms}, {'sample_id':'sample-02','source_sha':'b','arms':{}}]
        with mock.patch.object(runner,'_gold_index_by_source_sha') as reference:
            with self.assertRaises(ValueError):runner.posthoc(Path('unopened'),rows)
            reference.assert_not_called()

    def test_same_source_cannot_count_as_two_images(self):
        arms={key:{} for key in ('source_baseline','registered_baseline','template_augmented')}
        rows=[{'sample_id':f'sample-0{i}','source_sha':'same','arms':arms} for i in (1,2)]
        with mock.patch.object(runner,'_gold_index_by_source_sha') as reference:
            with self.assertRaises(ValueError):runner.posthoc(Path('unopened'),rows)
            reference.assert_not_called()

    def test_posthoc_source_mapping_rejects_duplicate_reference_entries(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);source=root/'source';source.write_bytes(b'synthetic-source')
            digest=hashlib.sha256(source.read_bytes()).hexdigest()
            gold=root/'gold.json';gold.write_text(json.dumps({'images':{key:{'source_image_path':str(source),'fields':{}} for key in ('one','two')}}))
            with self.assertRaises(RunnerError):
                runner._gold_index_by_source_sha(gold,{digest})

if __name__=='__main__':unittest.main()
