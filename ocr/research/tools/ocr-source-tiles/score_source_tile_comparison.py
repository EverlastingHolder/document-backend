#!/usr/bin/env python3
"""Posthoc score source-tile experiment after parity, hash, and identity gates pass."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

_CODE_ROOT = Path(__file__).resolve().parents[4]
if str(_CODE_ROOT) not in sys.path:
    sys.path.insert(0, str(_CODE_ROOT))
from ocr.experiments.local_paths import (
    ARCHIVE_OUTPUT_ROOT,
    ARCHIVE_WORK_ROOT,
    PRIVATE_RESEARCH_ROOT,
    RAPIDOCR_MODEL_DIR,
    REPO_ROOT,
    VL_MODEL_DIR,
)
import unicodedata
from typing import Any

TASK = PRIVATE_RESEARCH_ROOT
REPO = REPO_ROOT
WORK = TASK / 'work/ocr-source-tiles'
PREFLIGHT_DIR = WORK / 'preflight-v2'
PREFLIGHT_MANIFEST = PREFLIGHT_DIR / 'manifest.json'
BASELINE = TASK / 'work/ocr-five-image-eval/baseline-v2'
BASELINE_MANIFEST = BASELINE / 'manifest.json'
BASELINE_AUDIT = TASK / 'work/ocr-five-image-eval/baseline-v2-integrity-audit.json'
BASELINE_AUDIT_SHA256 = '9b3b25aa085b18096294cd05c94f50cf58c5c984d6e69edfcff08afc1363a0a3'
OLD_GOLD = TASK / 'work/ocr-passport-tests/ground-truth.json'
NEW_GOLD = TASK / 'work/ocr-five-image-eval/reference-new-images.json'
GO = Path(os.environ.get("PASSPORT_GO_BIN", shutil.which("go") or "go"))
SAMPLES = tuple(f'sample-{i:02d}' for i in range(1, 6))
TARGETS = ('sample-02', 'sample-04', 'sample-05')
FIELDS = ('surname', 'given_name', 'gender', 'birth_date', 'birth_place', 'series', 'number', 'issue_date', 'issuer', 'department_code')
OPTIONAL_FIELDS = ('patronymic',)
ALL_FIELDS = FIELDS + OPTIONAL_FIELDS
PINNED_MODELS = {
    RAPIDOCR_MODEL_DIR / "ch_PP-OCRv5_det_mobile.onnx": '4d97c44a20d30a81aad087d6a396b08f786c4635742afc391f6621f5c6ae78ae',
    RAPIDOCR_MODEL_DIR / "eslav_PP-OCRv5_rec_mobile.onnx": '08705d6721849b1347d26187f15a5e362c431963a2a62bfff4feac578c489aab',
}
SOURCES = {
    'sample-01': TASK / 'work/ocr-passport-tests/inputs/image-01.jpg',
    'sample-02': TASK / 'work/ocr-passport-tests/inputs/image-02.jpg',
    'sample-03': TASK / 'work/ocr-new-examples/inputs/sample03.jpg',
    'sample-04': TASK / 'work/ocr-new-examples/inputs/sample04.jpg',
    'sample-05': TASK / 'work/ocr-new-examples/inputs/sample05.jpg',
}
API_EXCLUSIONS = {str(REPO / 'internal/api/response.go'), str(REPO / 'internal/api/response_test.go')}
BASELINE_SCHEMA = 'passport-five-image-inference/v1'
EXPERIMENT_SCHEMA = 'passport-source-tile-experiment/v1'
AUDIT_SCHEMA = 'passport-five-image-integrity-audit/v1'


class ScoreError(ValueError):
    """Stable, sanitized scoring/preflight failure code."""


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def valid_hash(value: Any) -> bool:
    return isinstance(value, str) and re.fullmatch(r'[0-9a-fA-F]{64}', value) is not None


def load_json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding='utf-8'))
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise ScoreError(code) from error


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(',', ':'))


def canonical(value: str, field: str) -> str:
    value = ' '.join(unicodedata.normalize('NFC', value).strip().split()).casefold()
    if field == 'gender':
        return {'m': 'm', 'm.': 'm', 'муж': 'm', 'муж.': 'm', 'f': 'f', 'f.': 'f', 'жен': 'f', 'жен.': 'f'}.get(value, value)
    if field in {'series', 'number', 'department_code'}:
        digits = ''.join(ch for ch in value if ch.isdigit())
        if digits and all(ch in set('0123456789 -') for ch in value):
            return digits
    if field in {'birth_date', 'issue_date'}:
        from datetime import datetime
        for pattern in ('%Y-%m-%d', '%d.%m.%Y', '%d/%m/%Y', '%d-%m-%Y'):
            try:
                return datetime.strptime(value, pattern).date().isoformat()
            except ValueError:
                pass
    return value


def score_field(result: Any, expected: Any, field: str) -> dict[str, Any]:
    if not isinstance(result, dict) or result.get('state') not in {'recognized', 'uncertain', 'missing'}:
        raise ScoreError('extraction_field_invalid')
    if not isinstance(expected, dict) or not isinstance(expected.get('literal'), str):
        raise ScoreError('reference_field_invalid')
    value = result.get('value')
    if value is not None and not isinstance(value, str):
        raise ScoreError('extraction_value_invalid')
    literal = expected['literal']
    has_selected = bool(value and value.strip())
    selected_equal = has_selected and canonical(value, field) == canonical(literal, field)
    status = 'abstain' if not has_selected else ('correct' if selected_equal else 'wrong')
    candidates = result.get('candidates')
    if not isinstance(candidates, list):
        raise ScoreError('extraction_candidates_invalid')
    correct_candidate = bool(literal.strip()) and any(
        isinstance(candidate, dict) and isinstance(candidate.get('value'), str)
        and canonical(candidate['value'], field) == canonical(literal, field)
        for candidate in candidates
    )
    return {
        'selected_status': status,
        'manual_review_required': result['state'] == 'uncertain',
        'correct_candidate_available': bool(correct_candidate),
    }


def current_go_source_manifest() -> dict[str, str]:
    """Mirror the runner's Go package closure and source hash collection."""
    try:
        result = subprocess.run(
            [str(GO), 'list', '-deps', '-json', './cmd/ocr-eval'], cwd=REPO,
            env={**os.environ, 'GOTOOLCHAIN': 'local', 'GOPROXY': 'off', 'GOSUMDB': 'off'},
            text=True, capture_output=True, timeout=30, check=True,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise ScoreError('go_dependency_closure_failed') from error
    decoder = json.JSONDecoder()
    text = result.stdout
    offset = 0
    files: set[Path] = {REPO / 'go.mod', REPO / 'go.sum'}
    imported = set()
    while offset < len(text):
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset >= len(text):
            break
        package, offset = decoder.raw_decode(text, offset)
        if package.get('Standard') or not package.get('Dir'):
            continue
        imported.add(package.get('ImportPath'))
        directory = Path(package['Dir'])
        for key in ('GoFiles', 'CgoFiles', 'EmbedFiles'):
            files.update(directory / name for name in package.get(key, []))
    expected_imports = {'passport-backend/cmd/ocr-eval', 'passport-backend/internal/mrz', 'passport-backend/internal/passport'}
    if imported != expected_imports or any('/internal/api' in item for item in imported):
        raise ScoreError('go_dependency_closure_unexpected')
    if not (REPO / 'go.mod').is_file():
        raise ScoreError('go_runtime_source_missing')
    return {str(path): sha256(path) for path in sorted(files) if path.is_file()}


def expected_python_source_paths() -> set[Path]:
    return {
        REPO / 'ocr/experiments/source_tile_pipeline.py',
        REPO / 'ocr/experiments/region_pipeline.py',
        REPO / 'ocr/experiments/registration_first_pipeline.py',
        REPO / 'ocr/experiments/mrz_pipeline.py',
        REPO / 'ocr/experiments/requirements.txt',
        REPO / 'ocr/experiments/local_paths.py',
        REPO / "ocr/research/tools/ocr-five-image-eval/run_five_images.py",
        REPO / "ocr/research/tools/ocr-source-tiles/run_source_tile_comparison.py",
    }


def current_python_source_manifest() -> dict[str, str]:
    paths = expected_python_source_paths()
    if any(not path.is_file() for path in paths):
        raise ScoreError('python_runtime_source_missing')
    return {str(path): sha256(path) for path in sorted(paths)}


def verify_historical_baseline() -> tuple[dict[str, Any], dict[str, dict[str, Any]]]:
    if not BASELINE_MANIFEST.is_file() or not BASELINE_AUDIT.is_file() or sha256(BASELINE_AUDIT) != BASELINE_AUDIT_SHA256:
        raise ScoreError('historical_baseline_audit_pin_mismatch')
    manifest = load_json(BASELINE_MANIFEST, 'historical_baseline_manifest_invalid')
    audit = load_json(BASELINE_AUDIT, 'historical_baseline_audit_invalid')
    if (manifest.get('schema_version') != BASELINE_SCHEMA or manifest.get('status') != 'failed'
            or manifest.get('failure_code') != 'source_code_changed_during_run'
            or manifest.get('reference_data_read') is not False
            or manifest.get('reference_data_used_for_runtime_or_planning') is not False):
        raise ScoreError('historical_baseline_status_invalid')
    if (audit.get('schema_version') != AUDIT_SCHEMA or audit.get('original_status') != 'failed'
            or audit.get('original_failure_code') != 'source_code_changed_during_run'
            or audit.get('accepted_measurement_status') != 'dependency_audited'
            or audit.get('original_manifest_sha256') != sha256(BASELINE_MANIFEST)):
        raise ScoreError('historical_baseline_audit_binding_invalid')
    code = manifest.get('source_code_sha256', {})
    before, after = code.get('before'), code.get('after')
    if not isinstance(before, dict) or not isinstance(after, dict):
        raise ScoreError('historical_baseline_source_manifest_invalid')
    changed = {path for path in set(before) | set(after) if before.get(path) != after.get(path)}
    if changed != API_EXCLUSIONS:
        raise ScoreError('historical_baseline_source_diff_invalid')
    integrity = manifest.get('integrity', {})
    if (manifest.get('inputs_sha256', {}).get('before') != manifest.get('inputs_sha256', {}).get('after')
            or manifest.get('models_sha256', {}).get('before') != manifest.get('models_sha256', {}).get('after')
            or integrity.get('inputs_unchanged') is not True
            or integrity.get('models_unchanged') is not True
            or integrity.get('source_code_unchanged') is not False):
        raise ScoreError('historical_baseline_integrity_invalid')
    if any(Path(path).resolve().relative_to(REPO.resolve()).as_posix().startswith(('cmd/ocr-eval/', 'internal/mrz/', 'internal/passport/')) for path in changed):
        raise ScoreError('historical_api_change_in_runtime_closure')
    expected_runtime = {path: digest for path, digest in after.items() if path not in API_EXCLUSIONS}
    if audit.get('runtime_code_sha256') != expected_runtime:
        raise ScoreError('historical_audit_runtime_hashes_invalid')
    expected_changes = [
        {'path': path, 'before_sha256': before.get(path), 'after_sha256': after.get(path)}
        for path in sorted(API_EXCLUSIONS)
    ]
    if audit.get('changed_files') != expected_changes:
        raise ScoreError('historical_audit_changed_file_pairs_invalid')
    if audit.get('go_dependency_closure', {}).get('result') != [
        'passport-backend/cmd/ocr-eval', 'passport-backend/internal/mrz', 'passport-backend/internal/passport'
    ]:
        raise ScoreError('historical_audit_dependency_closure_invalid')
    drift = audit.get('postrun_drift_unrelated', [])
    if not isinstance(drift, list) or any(
        not isinstance(row, dict) or row.get('path') not in API_EXCLUSIONS
        or row.get('classification') != 'postrun_drift_unrelated'
        or not valid_hash(row.get('manifest_after_sha256')) or not valid_hash(row.get('current_sha256'))
        for row in drift
    ):
        raise ScoreError('historical_audit_postrun_drift_invalid')
    input_before = manifest.get('inputs_sha256', {}).get('before')
    model_before = manifest.get('models_sha256', {}).get('before')
    audit_models = audit.get('models_sha256')
    if not isinstance(audit_models, dict):
        raise ScoreError('historical_audit_source_model_binding_invalid')
    normalized_audit_models = {}
    for path, digest in audit_models.items():
        if not isinstance(path, str) or not Path(path).is_absolute() or not valid_hash(digest):
            raise ScoreError('historical_audit_source_model_binding_invalid')
        name = Path(path).name
        if not name or name in normalized_audit_models:
            raise ScoreError('historical_audit_source_model_binding_invalid')
        normalized_audit_models[name] = digest
    expected_audit_models = {Path(path).name: digest for path, digest in model_before.items()}
    if audit.get('inputs_sha256') != input_before or normalized_audit_models != expected_audit_models:
        raise ScoreError('historical_audit_source_model_binding_invalid')
    sample_rows = manifest.get('samples')
    if not isinstance(sample_rows, list) or len(sample_rows) != 5 or {r.get('sample_id') for r in sample_rows if isinstance(r, dict)} != set(SAMPLES):
        raise ScoreError('historical_baseline_sample_set_invalid')
    frozen = {r['sample_id']: r for r in sample_rows}
    audit_artifacts = {(r.get('sample_id'), Path(r.get('artifact', '')).name): r.get('sha256') for r in audit.get('artifacts', []) if isinstance(r, dict)}
    expected_artifacts = {(sample, name): frozen[sample].get(key) for sample in SAMPLES for name, key in (('observations.json', 'observation_sha256'), ('go-extraction.json', 'extraction_sha256'))}
    if audit_artifacts != expected_artifacts:
        raise ScoreError('historical_audit_artifact_map_invalid')
    if audit.get('go_evaluator_sha256') != manifest.get('go_evaluator_sha256'):
        raise ScoreError('historical_audit_binary_binding_invalid')
    frozen_binary = BASELINE / 'ocr-eval'
    if not frozen_binary.is_file() or sha256(frozen_binary) != audit.get('go_evaluator_sha256'):
        raise ScoreError('historical_binary_hash_mismatch')
    source_input = manifest.get('inputs_sha256', {}).get('before', {})
    if set(source_input) != set(SOURCES):
        raise ScoreError('historical_baseline_input_set_invalid')
    model_manifest = manifest.get('models_sha256', {}).get('before', {})
    if set(model_manifest) != {str(path) for path in PINNED_MODELS}:
        raise ScoreError('historical_baseline_model_set_invalid')
    for sample_id, source in SOURCES.items():
        if not source.is_file() or sha256(source) != source_input[sample_id]:
            raise ScoreError('historical_source_hash_mismatch')
    for model, pinned in PINNED_MODELS.items():
        if not model.is_file() or model_manifest.get(str(model)) != pinned or sha256(model) != pinned:
            raise ScoreError('historical_model_hash_mismatch')
    baseline_rows = {}
    for sample_id in SAMPLES:
        row = frozen[sample_id]
        if row.get('status') != 'completed':
            raise ScoreError('historical_sample_not_completed')
        sample_dir = BASELINE / sample_id
        expected = {}
        for name, key in (('observations.json', 'observation_sha256'), ('go-extraction.json', 'extraction_sha256')):
            path = sample_dir / name
            digest = row.get(key)
            if not path.is_file() or not valid_hash(digest) or sha256(path) != digest:
                raise ScoreError('historical_artifact_hash_mismatch')
            expected[name] = path
        baseline_rows[sample_id] = expected
    for sample_id in SAMPLES:
        observation = load_json(baseline_rows[sample_id]['observations.json'], 'historical_observation_invalid')
        image = observation.get('image') if isinstance(observation, dict) else None
        runtime = observation.get('runtime') if isinstance(observation, dict) else None
        registered = runtime.get('registered_spread') if isinstance(runtime, dict) else None
        source_hash = source_input[sample_id]
        if not isinstance(image, dict) or not valid_hash(image.get('sha256')):
            raise ScoreError('historical_image_identity_invalid')
        if isinstance(registered, dict):
            if (image.get('id') != f'{sample_id}:registered-spread' or registered.get('status') != 'accepted'
                    or registered.get('source_image_sha256') != source_hash
                    or registered.get('canonical_image_sha256') != image.get('sha256')):
                raise ScoreError('historical_registered_lineage_invalid')
        elif image.get('id') != sample_id or image.get('sha256') != source_hash:
            raise ScoreError('historical_source_image_lineage_invalid')
    return manifest, baseline_rows


def _artifact_digest(row: dict[str, Any], kind: str) -> str | None:
    keys = ('observation_sha256', 'observations_sha256') if kind == 'observations.json' else ('extraction_sha256', 'extractions_sha256')
    values = [row.get(key) for key in keys if row.get(key) is not None]
    if len(values) != 1 or not valid_hash(values[0]):
        return None
    return values[0]


def validate_preflight_binding(manifest: dict[str, Any], run_root: Path) -> tuple[Path, Path]:
    """Bind evaluator and parity artifacts to the fixed, completed local preflight."""
    preflight = load_json(PREFLIGHT_MANIFEST, 'preflight_manifest_invalid')
    if (preflight.get('schema_version') != EXPERIMENT_SCHEMA
            or preflight.get('status') != 'preflight_completed'
            or preflight.get('baseline_parity_gate') != 'passed'
            or preflight.get('reference_data_read') is not False
            or preflight.get('reference_data_used_for_runtime_or_planning') is not False):
        raise ScoreError('preflight_status_invalid')
    for key in ('baseline_audit_sha256', 'baseline_manifest_sha256'):
        if preflight.get(key) != manifest.get(key):
            raise ScoreError('preflight_baseline_binding_mismatch')
    for key in ('input_sha256', 'model_sha256', 'go_source_sha256', 'python_source_sha256'):
        group = preflight.get(key)
        experiment_group = manifest.get(key)
        if (not isinstance(group, dict) or not isinstance(experiment_group, dict)
                or group.get('before') != experiment_group.get('before')
                or group.get('after') != experiment_group.get('after')
                or group.get('before') != group.get('after')):
            raise ScoreError('preflight_runtime_binding_mismatch')
    preflight_evaluator = PREFLIGHT_DIR / 'ocr-eval'
    evaluator_hash = preflight.get('go_evaluator_sha256')
    if (not valid_hash(evaluator_hash) or evaluator_hash != manifest.get('go_evaluator_sha256')
            or not preflight_evaluator.is_file() or sha256(preflight_evaluator) != evaluator_hash):
        raise ScoreError('preflight_evaluator_hash_mismatch')
    parity_hashes = manifest.get('baseline_parity_extraction_sha256')
    if (not isinstance(parity_hashes, dict) or set(parity_hashes) != set(SAMPLES)
            or preflight.get('baseline_parity_extraction_sha256') != parity_hashes):
        raise ScoreError('preflight_parity_manifest_binding_mismatch')
    parity_root = PREFLIGHT_DIR / 'baseline-parity'
    for sample in SAMPLES:
        path = parity_root / sample / 'go-extraction.json'
        if (not valid_hash(parity_hashes[sample]) or not path.is_file()
                or sha256(path) != parity_hashes[sample]):
            raise ScoreError('preflight_parity_hash_mismatch')
    return preflight_evaluator, parity_root


def validate_tile_sample_row(row: dict[str, Any]) -> None:
    if (row.get('stage') != 'source_tiles_and_go_extraction'
            or row.get('observations_schema') != 'passport-ocr-observations/v3'
            or not isinstance(row.get('tile_count'), int) or isinstance(row.get('tile_count'), bool)
            or not 1 <= row['tile_count'] <= 6 or not isinstance(row.get('tile_pass_statuses'), list)
            or len(row['tile_pass_statuses']) != row['tile_count']
            or any(status not in {'ok', 'error'} for status in row['tile_pass_statuses'])):
        raise ScoreError('source_tile_worker_status_invalid')


def validate_experiment(manifest_path: Path) -> tuple[list[dict[str, Any]], dict[str, dict[str, Any]], dict[str, dict[str, Any]]]:
    run_root = manifest_path.resolve().parent
    manifest = load_json(manifest_path, 'experiment_manifest_invalid')
    if (manifest.get('schema_version') != EXPERIMENT_SCHEMA or manifest.get('status') != 'completed'
            or manifest.get('baseline_parity_gate') != 'passed'
            or manifest.get('reference_data_read') is not False
            or manifest.get('reference_data_used_for_runtime_or_planning') is not False
            or set(manifest.get('targets', [])) != set(TARGETS)):
        raise ScoreError('experiment_not_completed_or_gate_invalid')
    if manifest.get('baseline_audit_sha256') != BASELINE_AUDIT_SHA256:
        raise ScoreError('experiment_baseline_audit_pin_mismatch')
    baseline_manifest, baseline_paths = verify_historical_baseline()
    if manifest.get('baseline_manifest_sha256') != sha256(BASELINE_MANIFEST):
        raise ScoreError('experiment_baseline_manifest_pin_mismatch')
    input_set = manifest.get('input_sha256')
    model_set = manifest.get('model_sha256')
    go_set = manifest.get('go_source_sha256')
    py_set = manifest.get('python_source_sha256')
    for name, group in (('input', input_set), ('model', model_set), ('go', go_set), ('python', py_set)):
        if not isinstance(group, dict) or not isinstance(group.get('before'), dict) or group.get('before') != group.get('after'):
            raise ScoreError(f'{name}_integrity_mismatch')
    expected_inputs = baseline_manifest['inputs_sha256']['before']
    if input_set['before'] != expected_inputs or set(input_set['before']) != set(SAMPLES):
        raise ScoreError('experiment_input_baseline_binding_mismatch')
    if any(not SOURCES[sample].is_file() or sha256(SOURCES[sample]) != input_set['before'][sample] for sample in SAMPLES):
        raise ScoreError('experiment_input_hash_mismatch')
    expected_model_hashes = {str(path): digest for path, digest in PINNED_MODELS.items()}
    if model_set['before'] != expected_model_hashes:
        raise ScoreError('experiment_model_pins_invalid')
    if any(not path.is_file() or sha256(path) != digest for path, digest in PINNED_MODELS.items()):
        raise ScoreError('experiment_model_hash_mismatch')

    current_go = current_go_source_manifest()
    if go_set['before'] != current_go:
        raise ScoreError('experiment_go_runtime_hash_mismatch')
    current_python = current_python_source_manifest()
    if py_set['before'] != current_python:
        raise ScoreError('experiment_python_runtime_hash_mismatch')
    evaluator, parity_root = validate_preflight_binding(manifest, run_root)

    baseline_parity = manifest.get('baseline_parity_extraction_sha256')
    if not isinstance(baseline_parity, dict) or set(baseline_parity) != set(SAMPLES):
        raise ScoreError('baseline_parity_artifact_map_invalid')
    for sample in SAMPLES:
        parity = parity_root / sample / 'go-extraction.json'
        if not parity.is_file() or not valid_hash(baseline_parity[sample]) or sha256(parity) != baseline_parity[sample]:
            raise ScoreError('baseline_parity_hash_mismatch')
        frozen = load_json(baseline_paths[sample]['go-extraction.json'], 'historical_extraction_invalid')
        current = load_json(parity, 'parity_extraction_invalid')
        if canonical_json(frozen) != canonical_json(current):
            raise ScoreError('baseline_parity_json_mismatch')

    rows = manifest.get('samples')
    if not isinstance(rows, list) or len(rows) != 5:
        raise ScoreError('experiment_sample_count_invalid')
    row_map = {}
    for row in rows:
        if not isinstance(row, dict) or row.get('sample_id') in row_map or row.get('status') != 'completed':
            raise ScoreError('experiment_sample_row_invalid')
        row_map[row['sample_id']] = row
    if set(row_map) != set(SAMPLES):
        raise ScoreError('experiment_sample_set_invalid')

    baseline_observations = {}
    baseline_extractions = {}
    for sample in SAMPLES:
        old_obs = load_json(baseline_paths[sample]['observations.json'], 'historical_observation_invalid')
        old_ext = load_json(baseline_paths[sample]['go-extraction.json'], 'historical_extraction_invalid')
        baseline_observations[sample] = old_obs
        baseline_extractions[sample] = old_ext

    after = []
    for sample in SAMPLES:
        row = row_map[sample]
        sample_dir = run_root / sample
        obs_path = sample_dir / 'observations.json'
        extraction_path = sample_dir / 'go-extraction.json'
        obs_digest = _artifact_digest(row, 'observations.json')
        extraction_digest = _artifact_digest(row, 'go-extraction.json')
        if (not obs_path.is_file() or not extraction_path.is_file() or not obs_digest or not extraction_digest
                or sha256(obs_path) != obs_digest or sha256(extraction_path) != extraction_digest):
            raise ScoreError('experiment_artifact_hash_mismatch')
        old_obs = baseline_observations[sample]
        new_obs = load_json(obs_path, 'experiment_observation_invalid')
        old_image = old_obs.get('image') if isinstance(old_obs, dict) else None
        new_image = new_obs.get('image') if isinstance(new_obs, dict) else None
        if (not isinstance(old_image, dict) or not isinstance(new_image, dict)
                or old_image.get('id') != new_image.get('id')
                or old_image.get('sha256') != new_image.get('sha256')):
            raise ScoreError('experiment_image_identity_or_source_hash_mismatch')
        old_runtime = old_obs.get('runtime', {})
        if sample in TARGETS and (not isinstance(old_runtime, dict) or old_runtime.get('registered_spread') is not None):
            raise ScoreError('source_tile_target_not_source_fallback')
        extraction = load_json(extraction_path, 'experiment_extraction_invalid')
        if not isinstance(extraction, dict) or extraction.get('image_id') != new_image.get('id'):
            raise ScoreError('experiment_extraction_identity_mismatch')
        if sample in TARGETS:
            validate_tile_sample_row(row)
            worker_status = load_json(sample_dir / 'worker-status.json', 'source_tile_worker_status_missing')
            if worker_status != row:
                raise ScoreError('source_tile_worker_status_mismatch')
            plan_path = sample_dir / 'source-tile-plan.json'
            if not plan_path.is_file() or plan_path.stat().st_mode & 0o077:
                raise ScoreError('source_tile_plan_missing')
        else:
            if row.get('route') != 'registered_template_baseline_parity_reuse':
                raise ScoreError('registered_baseline_reuse_route_invalid')
            if sha256(obs_path) != sha256(baseline_paths[sample]['observations.json']):
                raise ScoreError('registered_observation_not_reused')
        after.append({'sample_id': sample, 'extraction': extraction, 'observation': new_obs})

    # Recheck all bound artifacts after parsing to close the read/hash race.
    for sample in SAMPLES:
        row = row_map[sample]
        if sha256(run_root / sample / 'observations.json') != _artifact_digest(row, 'observations.json') or sha256(run_root / sample / 'go-extraction.json') != _artifact_digest(row, 'go-extraction.json'):
            raise ScoreError('experiment_artifact_changed_during_validation')
    return after, baseline_extractions, row_map


def _reference_hashes(entry: Any, source_root: Path) -> set[str]:
    if not isinstance(entry, dict):
        raise ScoreError('reference_entry_invalid')
    result = set()
    for key in ('source_sha256', 'sha256'):
        value = entry.get(key)
        if valid_hash(value):
            result.add(value.lower())
    for key in ('source_image_path', 'source_path', 'image_path'):
        value = entry.get(key)
        if isinstance(value, str):
            path = Path(value).expanduser()
            if not path.is_absolute():
                path = source_root / path
            if path.is_file():
                result.add(sha256(path.resolve()))
    return result


def index_gold(gold: Any, source_root: Path, expected_hashes: set[str]) -> dict[str, dict[str, Any]]:
    images = gold.get('images') if isinstance(gold, dict) else None
    if not isinstance(images, (dict, list)):
        raise ScoreError('reference_images_invalid')
    rows = images.items() if isinstance(images, dict) else enumerate(images)
    matches = {digest: [] for digest in expected_hashes}
    for _key, entry in rows:
        hashes = _reference_hashes(entry, source_root)
        if len(hashes) > 1:
            raise ScoreError('reference_entry_hash_ambiguous')
        for digest in hashes:
            if digest in matches:
                matches[digest].append(entry)
    if any(len(entries) != 1 for entries in matches.values()):
        raise ScoreError('reference_hash_mapping_not_unique')
    return {digest: entries[0] for digest, entries in matches.items()}


def score_extraction(extraction: dict[str, Any], reference: dict[str, Any]) -> dict[str, dict[str, Any]]:
    fields = extraction.get('fields')
    expected_fields = reference.get('fields')
    if not isinstance(fields, dict) or not isinstance(expected_fields, dict):
        raise ScoreError('field_maps_invalid')
    result = {}
    for field in ALL_FIELDS:
        expected = expected_fields.get(field)
        if field in OPTIONAL_FIELDS and expected is None:
            expected = {'literal': ''}
        result[field] = score_field(fields.get(field), expected, field)
    return result


def status_counts(field_scores: dict[str, dict[str, Any]], field_ids: tuple[str, ...]) -> dict[str, int]:
    subset = [field_scores[field] for field in field_ids]
    return {
        'correct': sum(row['selected_status'] == 'correct' for row in subset),
        'wrong': sum(row['selected_status'] == 'wrong' for row in subset),
        'abstain': sum(row['selected_status'] == 'abstain' for row in subset),
        'manual_review_flags': sum(row['manual_review_required'] for row in subset),
        'correct_candidate_available': sum(row['correct_candidate_available'] for row in subset),
    }


def transition(before: dict[str, Any], after: dict[str, Any]) -> str:
    old, new = before['selected_status'], after['selected_status']
    if old != 'correct' and new == 'correct':
        return 'gain'
    if old == 'correct' and new != 'correct':
        return 'loss'
    if old != new:
        return 'changed_wrong_abstain'
    return 'unchanged'


def build_comparison(after_rows: list[dict[str, Any]], before_extractions: dict[str, dict[str, Any]], references_by_hash: dict[str, dict[str, Any]], source_hashes: dict[str, str]) -> dict[str, Any]:
    sample_results = []
    before_total = {key: 0 for key in ('correct', 'wrong', 'abstain', 'manual_review_flags', 'correct_candidate_available')}
    after_total = dict(before_total)
    optional_before = {key: 0 for key in before_total}
    optional_after = dict(optional_before)
    field_totals = {field: {'gain': 0, 'loss': 0, 'changed_wrong_abstain': 0, 'unchanged': 0,
                            'candidate_available_before': 0, 'candidate_available_after': 0} for field in FIELDS}
    computed_baseline_rows = {}
    for row in after_rows:
        sample = row['sample_id']
        reference = references_by_hash[source_hashes[sample]]
        before_fields = score_extraction(before_extractions[sample], reference)
        after_fields = score_extraction(row['extraction'], reference)
        computed_baseline_rows[sample] = before_fields
        field_changes = {}
        for field in FIELDS:
            old, new = before_fields[field], after_fields[field]
            change = transition(old, new)
            field_totals[field][change] += 1
            field_totals[field]['candidate_available_before'] += int(old['correct_candidate_available'])
            field_totals[field]['candidate_available_after'] += int(new['correct_candidate_available'])
            field_changes[field] = {
                'before_status': old['selected_status'], 'after_status': new['selected_status'], 'change': change,
                'manual_review_before': old['manual_review_required'], 'manual_review_after': new['manual_review_required'],
                'correct_candidate_before': old['correct_candidate_available'], 'correct_candidate_after': new['correct_candidate_available'],
            }
        before_req = status_counts(before_fields, FIELDS)
        after_req = status_counts(after_fields, FIELDS)
        before_opt = status_counts(before_fields, OPTIONAL_FIELDS)
        after_opt = status_counts(after_fields, OPTIONAL_FIELDS)
        for key in before_total:
            before_total[key] += before_req[key]
            after_total[key] += after_req[key]
            optional_before[key] += before_opt[key]
            optional_after[key] += after_opt[key]
        sample_results.append({
            'sample_id': sample, 'required_before': before_req, 'required_after': after_req,
            'required_delta': {key: after_req[key] - before_req[key] for key in before_req},
            'patronymic_before': before_opt['correct'], 'patronymic_after': after_opt['correct'],
            'patronymic_status_before': before_fields['patronymic']['selected_status'],
            'patronymic_status_after': after_fields['patronymic']['selected_status'],
            'patronymic_manual_review_before': before_fields['patronymic']['manual_review_required'],
            'patronymic_manual_review_after': after_fields['patronymic']['manual_review_required'],
            'patronymic_candidate_before': before_fields['patronymic']['correct_candidate_available'],
            'patronymic_candidate_after': after_fields['patronymic']['correct_candidate_available'],
            'fields': field_changes,
        })
    expected_before = {'correct': 27, 'wrong': 9, 'abstain': 14}
    if {key: before_total[key] for key in expected_before} != expected_before:
        raise ScoreError('recomputed_frozen_baseline_metric_mismatch')
    return {
        'schema_version': 'passport-source-tile-comparison-score/v1', 'status': 'completed',
        'audit_accepted': True, 'baseline_parity_gate': 'passed',
        'dataset_role': 'development_photos_not_heldout',
        'limitation': 'sample-03-repeats-an-existing-document; samples-04-and-05-are-other-documents-without-MRZ',
        'required_field_ids': list(FIELDS), 'before_required': before_total, 'after_required': after_total,
        'required_delta': {key: after_total[key] - before_total[key] for key in before_total},
        'optional_patronymic': {'before': optional_before, 'after': optional_after,
                                 'delta': {key: optional_after[key] - optional_before[key] for key in optional_before}},
        'per_field': field_totals, 'samples': sample_results,
        'policy': {'selected_correct_wrong_abstain_uses_value_and_gold_only': True,
                   'manual_review_is_overlapping_uncertain_flag': True,
                   'candidate_availability_is_separate': True},
    }


def write_private(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w', encoding='utf-8') as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(content)


def render_markdown(report: dict[str, Any]) -> str:
    b, a, d = report['before_required'], report['after_required'], report['required_delta']
    lines = [
        '# Source-tile comparison', '',
        '- Baseline parity gate: passed; historical integrity audit accepted.',
        '- Dataset role: development photos, not held out; sample-03 repeats an existing document, samples-04 and -05 are other documents without MRZ.',
        '- Selected correct/wrong/abstain uses selected value only. Manual review flags overlap when state is uncertain; candidates are counted separately.', '',
        '| Sample | Before C/W/A | After C/W/A | Delta C/W/A | Review flags before→after | Candidates before→after |',
        '|---|---:|---:|---:|---:|---:|',
    ]
    for row in report['samples']:
        x, y, z = row['required_before'], row['required_after'], row['required_delta']
        lines.append(f"| {row['sample_id']} | {x['correct']}/{x['wrong']}/{x['abstain']} | {y['correct']}/{y['wrong']}/{y['abstain']} | {z['correct']:+d}/{z['wrong']:+d}/{z['abstain']:+d} | {x['manual_review_flags']}→{y['manual_review_flags']} | {x['correct_candidate_available']}→{y['correct_candidate_available']} |")
    lines.append(f"| **Total** | {b['correct']}/{b['wrong']}/{b['abstain']} | {a['correct']}/{a['wrong']}/{a['abstain']} | {d['correct']:+d}/{d['wrong']:+d}/{d['abstain']:+d} | {b['manual_review_flags']}→{a['manual_review_flags']} | {b['correct_candidate_available']}→{a['correct_candidate_available']} |")
    lines += ['', '## Required fields', '', '| Field ID | Gains | Losses | Other status changes | Candidate before→after |', '|---|---:|---:|---:|---:|']
    for field, stats in report['per_field'].items():
        lines.append(f"| {field} | {stats['gain']} | {stats['loss']} | {stats['changed_wrong_abstain']} | {stats['candidate_available_before']}→{stats['candidate_available_after']} |")
    optional = report['optional_patronymic']
    lines += ['', '## Optional patronymic', '',
              f"- Selected before→after: {optional['before']['correct']}/{optional['before']['wrong']}/{optional['before']['abstain']} → {optional['after']['correct']}/{optional['after']['wrong']}/{optional['after']['abstain']}.",
              f"- Manual-review flags: {optional['before']['manual_review_flags']}→{optional['after']['manual_review_flags']}; correct candidate available: {optional['before']['correct_candidate_available']}→{optional['after']['correct_candidate_available']}.",
              '', 'No field values, source paths, source hashes, or image content are included.']
    return '\n'.join(lines) + '\n'


def run(manifest_path: Path, output_dir: Path) -> None:
    # All integrity, parity, and identity checks finish before opening either gold file.
    after_rows, before_extractions, _row_map = validate_experiment(manifest_path)
    manifest = load_json(manifest_path, 'experiment_manifest_invalid')
    input_hashes = manifest['input_sha256']['before']
    old_gold = load_json(OLD_GOLD, 'old_reference_invalid')
    new_gold = load_json(NEW_GOLD, 'new_reference_invalid')
    old_refs = index_gold(old_gold, OLD_GOLD.parent, {input_hashes['sample-01'], input_hashes['sample-02']})
    new_refs = index_gold(new_gold, NEW_GOLD.parent, {input_hashes['sample-03'], input_hashes['sample-04'], input_hashes['sample-05']})
    references = {**old_refs, **new_refs}
    report = build_comparison(after_rows, before_extractions, references, input_hashes)
    output_dir.mkdir(parents=True, exist_ok=False, mode=0o700)
    output_dir.chmod(0o700)
    write_private(output_dir / 'source-tile-comparison.json', json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    write_private(output_dir / 'source-tile-comparison.md', render_markdown(report))
    print('completed samples=5 required_fields=10')


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', type=Path, required=True, help='Completed source-tile experiment manifest.json')
    parser.add_argument('--output-dir', type=Path, default=TASK / 'outputs/ocr-source-tile-comparison', help='New report directory; existing directory is refused')
    args = parser.parse_args()
    os.umask(0o077)
    try:
        run(args.manifest.expanduser().resolve(), args.output_dir.expanduser().resolve())
        return 0
    except ScoreError as error:
        print(f'failed status={error}', file=sys.stderr)
        return 2
    except Exception:
        print('failed status=unexpected_internal_error', file=sys.stderr)
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
