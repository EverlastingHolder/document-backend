#!/usr/bin/env python3
"""Offline posthoc scoring for one completed five-image OCR baseline."""
from __future__ import annotations

import argparse
from datetime import datetime
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
SOURCES = {
    'sample-01': TASK / 'work/ocr-passport-tests/inputs/image-01.jpg',
    'sample-02': TASK / 'work/ocr-passport-tests/inputs/image-02.jpg',
    'sample-03': TASK / 'work/ocr-new-examples/inputs/sample03.jpg',
    'sample-04': TASK / 'work/ocr-new-examples/inputs/sample04.jpg',
    'sample-05': TASK / 'work/ocr-new-examples/inputs/sample05.jpg',
}
FIELDS = ('surname', 'given_name', 'gender', 'birth_date', 'birth_place', 'series', 'number', 'issue_date', 'issuer', 'department_code')
OPTIONAL_FIELDS = ('patronymic',)
ALL_FIELDS = FIELDS + OPTIONAL_FIELDS
SCHEMA = 'passport-five-image-inference/v1'
AUDIT_SCHEMA = 'passport-five-image-integrity-audit/v1'
GO = Path(os.environ.get("PASSPORT_GO_BIN", shutil.which("go") or "go"))
GO_DEP_COMMAND = "go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/ocr-eval"
EXPECTED_GO_CLOSURE = ['passport-backend/cmd/ocr-eval', 'passport-backend/internal/mrz', 'passport-backend/internal/passport']
EXPECTED_CHANGED_PATHS = {
    str(REPO / 'internal/api/response.go'),
    str(REPO / 'internal/api/response_test.go'),
}


class ScoreError(ValueError):
    """Safe error containing a stable code only."""


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def canonical(value: str, field: str) -> str:
    """Reuse the existing comparison's conservative normalization semantics."""
    value = ' '.join(unicodedata.normalize('NFC', value).strip().split()).casefold()
    if field == 'gender':
        return {'m': 'm', 'm.': 'm', 'муж': 'm', 'муж.': 'm', 'f': 'f', 'f.': 'f', 'жен': 'f', 'жен.': 'f'}.get(value, value)
    if field in {'series', 'number', 'department_code'}:
        digits = ''.join(character for character in value if character.isdigit())
        if digits and all(character in set('0123456789 -') for character in value):
            return digits
    if field in {'birth_date', 'issue_date'}:
        for pattern in ('%Y-%m-%d', '%d.%m.%Y', '%d/%m/%Y', '%d-%m-%Y'):
            try:
                return datetime.strptime(value, pattern).date().isoformat()
            except ValueError:
                pass
    return value


def _json(path: Path, code: str) -> Any:
    try:
        return json.loads(path.read_text(encoding='utf-8'))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ScoreError(code) from exc


def go_dependency_closure() -> list[str]:
    try:
        result = subprocess.run(
            [str(GO), 'list', '-deps', '-f', '{{if not .Standard}}{{.ImportPath}}{{end}}', './cmd/ocr-eval'],
            cwd=REPO, env={**os.environ, 'GOTOOLCHAIN': 'local', 'GOPROXY': 'off', 'GOSUMDB': 'off'},
            capture_output=True, text=True, timeout=20, check=True,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise ScoreError('dependency_audit_go_list_failed') from exc
    return sorted(line.strip() for line in result.stdout.splitlines() if line.strip())


def build_integrity_audit(manifest_path: Path, sources: dict[str, Path] = SOURCES) -> dict[str, Any]:
    """Build the one approved exception record, validating every referenced hash."""
    manifest = _json(manifest_path, 'manifest_invalid')
    if (not isinstance(manifest, dict) or manifest.get('schema_version') != SCHEMA
            or manifest.get('status') != 'failed' or manifest.get('failure_code') != 'source_code_changed_during_run'
            or manifest.get('failure_stage') != 'post_run_integrity' or manifest.get('reference_data_read') is not False
            or manifest.get('reference_data_used_for_runtime_or_planning') is not False):
        raise ScoreError('integrity_audit_not_eligible')
    bundles = {key: manifest.get(key) for key in ('inputs_sha256', 'models_sha256', 'source_code_sha256')}
    if any(not isinstance(value, dict) or not isinstance(value.get('before'), dict)
           or not isinstance(value.get('after'), dict) for value in bundles.values()):
        raise ScoreError('integrity_audit_manifest_shape_invalid')
    if bundles['inputs_sha256']['before'] != bundles['inputs_sha256']['after']:
        raise ScoreError('integrity_audit_inputs_changed')
    if bundles['models_sha256']['before'] != bundles['models_sha256']['after']:
        raise ScoreError('integrity_audit_models_changed')
    integrity = manifest.get('integrity')
    if not isinstance(integrity, dict) or integrity.get('inputs_unchanged') is not True or integrity.get('models_unchanged') is not True or integrity.get('source_code_unchanged') is not False:
        raise ScoreError('integrity_audit_flags_invalid')
    before, after = bundles['source_code_sha256']['before'], bundles['source_code_sha256']['after']
    changed = sorted(path for path in set(before) | set(after) if before.get(path) != after.get(path))
    if set(changed) != EXPECTED_CHANGED_PATHS:
        raise ScoreError('integrity_audit_unlisted_source_changes')
    for path, digest in after.items():
        # These exact two files are the audited, non-runtime changes. Their
        # run-time before/after pair comes from the immutable original manifest.
        if path in EXPECTED_CHANGED_PATHS:
            continue
        if not _valid_hash(digest) or not Path(path).is_file() or sha256(Path(path)) != digest:
            raise ScoreError('integrity_audit_source_hash_mismatch')
    if any('/internal/api/' in path for path in changed) is False:
        raise ScoreError('integrity_audit_change_not_api_only')
    closure = go_dependency_closure()
    if closure != EXPECTED_GO_CLOSURE:
        raise ScoreError('integrity_audit_go_closure_unexpected')
    changed_rel = {Path(path).resolve().relative_to(REPO.resolve()).as_posix() for path in changed}
    runtime_package_prefixes = ('cmd/ocr-eval/', 'internal/mrz/', 'internal/passport/')
    if any(any(path.startswith(prefix) for prefix in runtime_package_prefixes) for path in changed_rel):
        raise ScoreError('integrity_audit_changed_runtime_package')

    inputs = bundles['inputs_sha256']['before']
    if set(inputs) != set(sources):
        raise ScoreError('integrity_audit_source_set_invalid')
    for sample_id, path in sources.items():
        if not path.is_file() or sha256(path) != inputs[sample_id]:
            raise ScoreError('integrity_audit_source_hash_mismatch')
    for path, digest in bundles['models_sha256']['after'].items():
        if not Path(path).is_file() or sha256(Path(path)) != digest:
            raise ScoreError('integrity_audit_model_hash_mismatch')
    rows = manifest.get('samples')
    if not isinstance(rows, list) or len(rows) != 5 or {row.get('sample_id') for row in rows if isinstance(row, dict)} != set(sources):
        raise ScoreError('integrity_audit_sample_set_invalid')
    artifact_checks = []
    run_root = manifest_path.resolve().parent
    for row in rows:
        sample_id = row['sample_id']
        if row.get('status') != 'completed':
            raise ScoreError('integrity_audit_sample_not_completed')
        for filename, hash_key in (('observations.json', 'observation_sha256'), ('go-extraction.json', 'extraction_sha256')):
            path = run_root / sample_id / filename
            digest = row.get(hash_key)
            if not path.is_file() or not _valid_hash(digest) or sha256(path) != digest:
                raise ScoreError('integrity_audit_artifact_hash_mismatch')
            artifact_checks.append({'sample_id': sample_id, 'artifact': filename, 'sha256': digest})
    binary = run_root / 'ocr-eval'
    binary_hash = manifest.get('go_evaluator_sha256')
    if not binary.is_file() or not _valid_hash(binary_hash) or sha256(binary) != binary_hash:
        raise ScoreError('integrity_audit_binary_hash_mismatch')
    runtime_hashes = {path: after[path] for path in sorted(after) if path not in EXPECTED_CHANGED_PATHS}
    changed_hashes = [{'path': path, 'before_sha256': before.get(path), 'after_sha256': after.get(path)} for path in changed]
    postrun_drift = []
    for path in sorted(EXPECTED_CHANGED_PATHS):
        current = Path(path)
        if not current.is_file() or path not in after:
            raise ScoreError('integrity_audit_allowed_file_missing')
        current_hash = sha256(current)
        if current_hash != after[path]:
            postrun_drift.append({'path': path, 'manifest_after_sha256': after[path], 'current_sha256': current_hash,
                                  'classification': 'postrun_drift_unrelated'})
    return {
        'schema_version': AUDIT_SCHEMA,
        'original_manifest_sha256': sha256(manifest_path),
        'original_status': manifest['status'],
        'original_failure_code': manifest['failure_code'],
        'accepted_measurement_status': 'dependency_audited',
        'changed_files': changed_hashes,
        'postrun_drift_unrelated': postrun_drift,
        'go_dependency_closure': {'command': GO_DEP_COMMAND, 'result': closure},
        'runtime_code_sha256': runtime_hashes,
        'inputs_sha256': inputs,
        'models_sha256': bundles['models_sha256']['after'],
        'artifacts': artifact_checks,
        'go_evaluator_sha256': binary_hash,
        'explanation': 'Only the two API response files changed; go list confirms neither belongs to cmd/ocr-eval runtime dependency closure. Input, model, runtime source, artifact, and evaluator hashes match the original manifest.',
    }


def write_private(path: Path, payload: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w', encoding='utf-8') as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(payload)


def create_integrity_audit(manifest_path: Path, audit_path: Path) -> None:
    audit = build_integrity_audit(manifest_path)
    write_private(audit_path, json.dumps(audit, ensure_ascii=False, indent=2) + '\n')


def verify_integrity_audit(manifest_path: Path, audit_path: Path, sources: dict[str, Path] = SOURCES) -> None:
    supplied = _json(audit_path, 'integrity_audit_invalid')
    expected = build_integrity_audit(manifest_path, sources)
    if supplied != expected:
        raise ScoreError('integrity_audit_mismatch')


def _valid_hash(value: Any) -> bool:
    return isinstance(value, str) and re.fullmatch(r'[0-9a-fA-F]{64}', value) is not None


def _lineage_hashes(value: Any) -> set[str]:
    aliases = {'source_sha256', 'source_image_sha256', 'original_sha256', 'input_sha256', 'source_hash', 'source_image_hash'}
    parent_aliases = {'source_image', 'original_image', 'input_image', 'source'}
    found: set[str] = set()
    def visit(node: Any, parent_key: str = '') -> None:
        if isinstance(node, dict):
            for key, child in node.items():
                normalized = str(key).casefold()
                if normalized in aliases and _valid_hash(child):
                    found.add(child.lower())
                elif normalized == 'sha256' and parent_key in parent_aliases and _valid_hash(child):
                    found.add(child.lower())
                visit(child, normalized)
        elif isinstance(node, list):
            for child in node:
                visit(child, parent_key)
    visit(value)
    return found


def validate_completed_baseline(manifest_path: Path, sources: dict[str, Path] = SOURCES,
                                integrity_audit_path: Path | None = None) -> list[dict[str, Any]]:
    """Verify manifest, five real sources, and every artifact before any gold read."""
    manifest = _json(manifest_path, 'manifest_invalid')
    if (not isinstance(manifest, dict) or manifest.get('schema_version') != SCHEMA
            or manifest.get('reference_data_read') is not False
            or manifest.get('reference_data_used_for_runtime_or_planning') is not False):
        raise ScoreError('baseline_not_completed_or_reference_boundary_invalid')
    integrity = manifest.get('integrity')
    bundles_for_gate = (manifest.get('inputs_sha256'), manifest.get('models_sha256'), manifest.get('source_code_sha256'))
    if manifest.get('status') == 'completed':
        if (not isinstance(integrity, dict) or any(integrity.get(key) is not True for key in
                ('inputs_unchanged', 'models_unchanged', 'source_code_unchanged'))):
            raise ScoreError('baseline_integrity_flags_invalid')
        if any(not isinstance(bundle, dict) or bundle.get('before') != bundle.get('after') for bundle in bundles_for_gate):
            raise ScoreError('baseline_integrity_hashes_mismatch')
    elif manifest.get('status') == 'failed' and manifest.get('failure_code') == 'source_code_changed_during_run':
        if integrity_audit_path is None:
            raise ScoreError('integrity_audit_required')
        verify_integrity_audit(manifest_path, integrity_audit_path, sources)
    else:
        raise ScoreError('baseline_not_completed_or_reference_boundary_invalid')
    if set(sources) != {f'sample-{i:02d}' for i in range(1, 6)}:
        raise ScoreError('source_sample_set_invalid')
    inputs = manifest.get('inputs_sha256')
    models = manifest.get('models_sha256')
    code = manifest.get('source_code_sha256')
    integrity = manifest.get('integrity')
    samples = manifest.get('samples')
    if not all(isinstance(item, dict) and isinstance(item.get('before'), dict) and isinstance(item.get('after'), dict)
               and item['before'] == item['after'] for item in (inputs, models)):
        raise ScoreError('baseline_integrity_hashes_mismatch')
    if (not isinstance(code, dict) or not isinstance(code.get('before'), dict) or not isinstance(code.get('after'), dict)
            or set(inputs['before']) != set(sources) or not models['before'] or not code['before']
            or not all(_valid_hash(value) for group in (inputs, models, code) for side in ('before', 'after') for value in group[side].values())
            or not isinstance(samples, list) or len(samples) != 5):
        raise ScoreError('baseline_manifest_sample_set_invalid')
    rows_by_id = {}
    for row in samples:
        if not isinstance(row, dict) or row.get('sample_id') in rows_by_id:
            raise ScoreError('baseline_manifest_sample_duplicate_or_invalid')
        rows_by_id[row.get('sample_id')] = row
    if set(rows_by_id) != set(sources):
        raise ScoreError('baseline_manifest_sample_set_invalid')

    # Hash all paths and verify all declared digests before parsing OCR JSON.
    artifact_paths: dict[str, tuple[Path, str]] = {}
    source_digests = {}
    root = manifest_path.parent.resolve()
    for sample_id, source in sources.items():
        source = source.resolve()
        if not source.is_file():
            raise ScoreError('source_missing')
        source_digest = sha256(source)
        if inputs['before'].get(sample_id) != source_digest:
            raise ScoreError('source_hash_mismatch')
        source_digests[sample_id] = source_digest
        row = rows_by_id[sample_id]
        if row.get('status') != 'completed':
            raise ScoreError('sample_not_completed')
        extraction_path = (root / sample_id / 'go-extraction.json').resolve()
        observation_path = (root / sample_id / 'observations.json').resolve()
        if root not in extraction_path.parents or root not in observation_path.parents:
            raise ScoreError('artifact_path_invalid')
        for path, hash_key, error_code in ((extraction_path, 'extraction_sha256', 'extraction_hash_mismatch'), (observation_path, 'observation_sha256', 'observation_hash_mismatch')):
            if not path.is_file():
                raise ScoreError('artifact_missing')
            actual = sha256(path)
            if not isinstance(row.get(hash_key), str) or row[hash_key] != actual:
                raise ScoreError(error_code)
            artifact_paths[f'{sample_id}:{hash_key}'] = (path, actual)
    completed = []
    for sample_id in sources:
        row = rows_by_id[sample_id]
        extraction = _json(root / sample_id / 'go-extraction.json', 'extraction_invalid')
        observation = _json(root / sample_id / 'observations.json', 'observation_invalid')
        image = observation.get('image') if isinstance(observation, dict) else None
        runtime = observation.get('runtime') if isinstance(observation, dict) else None
        registered = runtime.get('registered_spread') if isinstance(runtime, dict) else None
        expected_image_id = f'{sample_id}:registered-spread' if isinstance(registered, dict) else sample_id
        if not isinstance(image, dict) or image.get('id') != expected_image_id or not _valid_hash(image.get('sha256')):
            raise ScoreError('observation_image_lineage_mismatch')
        if isinstance(registered, dict):
            if (registered.get('status') != 'accepted' or registered.get('coordinate_space_role') != 'derived_registered_spread'
                    or registered.get('source_image_sha256') != source_digests[sample_id]
                    or registered.get('canonical_image_sha256') != image.get('sha256')):
                raise ScoreError('observation_registered_lineage_mismatch')
        elif image.get('sha256') != source_digests[sample_id]:
            raise ScoreError('observation_source_image_hash_mismatch')
        if not isinstance(extraction, dict) or extraction.get('image_id') != expected_image_id:
            raise ScoreError('extraction_image_id_mismatch')
        completed.append({'sample_id': sample_id, 'source_sha256': source_digests[sample_id], 'extraction': extraction})
    for sample_id in sources:
        for filename, key in (('go-extraction.json', 'extraction_sha256'), ('observations.json', 'observation_sha256')):
            if sha256(root / sample_id / filename) != rows_by_id[sample_id][key]:
                raise ScoreError('artifact_changed_during_validation')
    return completed


def _reference_entry_hashes(entry: Any, source_root: Path, key: str, completed_by_id: dict[str, dict[str, Any]], sources: dict[str, Path]) -> set[str]:
    if not isinstance(entry, dict):
        raise ScoreError('reference_entry_invalid')
    hashes: set[str] = set()
    for hash_key in ('source_sha256', 'sha256'):
        value = entry.get(hash_key)
        if isinstance(value, str) and re.fullmatch(r'[0-9a-fA-F]{64}', value):
            hashes.add(value.lower())
    for nested_key in ('source_image', 'image', 'source'):
        nested = entry.get(nested_key)
        if isinstance(nested, dict):
            value = nested.get('sha256') or nested.get('source_sha256')
            if isinstance(value, str) and re.fullmatch(r'[0-9a-fA-F]{64}', value):
                hashes.add(value.lower())
    for path_key in ('source_image_path', 'source_path', 'image_path'):
        value = entry.get(path_key)
        if isinstance(value, str):
            path = Path(value).expanduser()
            if not path.is_absolute():
                path = source_root / path
            if path.is_file():
                hashes.add(sha256(path.resolve()))
    if not hashes:
        normalized_key = re.sub(r'[^a-z0-9]', '', key.casefold())
        keyed_ids = [sample_id for sample_id, path in sources.items()
                     if normalized_key in {re.sub(r'[^a-z0-9]', '', sample_id.casefold()),
                                           re.sub(r'[^a-z0-9]', '', path.stem.casefold())}]
        if len(keyed_ids) == 1:
            hashes.add(completed_by_id[keyed_ids[0]]['source_sha256'])
    return hashes


def index_reference(gold: Any, completed: list[dict[str, Any]], source_root: Path, expected_ids: set[str], sources: dict[str, Path] = SOURCES) -> dict[str, dict[str, Any]]:
    images = gold.get('images') if isinstance(gold, dict) else None
    if not isinstance(images, (dict, list)):
        raise ScoreError('reference_images_invalid')
    needed = {row['source_sha256'] for row in completed}
    if len(needed) != 5:
        raise ScoreError('duplicate_source_hash')
    completed_by_id = {row['sample_id']: row for row in completed}
    expected_hashes = {completed_by_id[sample_id]['source_sha256'] for sample_id in expected_ids}
    matches: dict[str, list[tuple[str, dict[str, Any]]]] = {digest: [] for digest in expected_hashes}
    records = images.items() if isinstance(images, dict) else ((str(entry.get('image_id', index)) if isinstance(entry, dict) else str(index), entry) for index, entry in enumerate(images))
    for key, entry in records:
        entry_hashes = _reference_entry_hashes(entry, source_root, str(key), completed_by_id, sources)
        if len(entry_hashes) > 1:
            raise ScoreError('reference_entry_hash_ambiguous')
        for digest in entry_hashes:
            if digest in needed and digest not in expected_hashes:
                raise ScoreError('reference_contains_wrong_sample_group')
            if digest in expected_hashes:
                matches[digest].append((str(key), entry))
    if any(len(items) != 1 for items in matches.values()):
        raise ScoreError('reference_hash_mapping_not_unique')
    return {digest: items[0][1] for digest, items in matches.items()}


def score_field(result: Any, expected: Any, field: str) -> dict[str, Any]:
    if not isinstance(result, dict) or result.get('state') not in {'recognized', 'uncertain', 'missing'}:
        raise ScoreError('extraction_field_invalid')
    if not isinstance(expected, dict) or not isinstance(expected.get('literal'), str):
        raise ScoreError('reference_field_invalid')
    state = result['state']
    value = result.get('value')
    if value is not None and not isinstance(value, str):
        raise ScoreError('extraction_value_invalid')
    literal = expected['literal']
    selected_present = bool(value and value.strip())
    selected_equal = selected_present and canonical(value, field) == canonical(literal, field)
    if not selected_present:
        selected_status = 'abstain'
    elif selected_equal:
        selected_status = 'correct'
    else:
        selected_status = 'wrong'
    candidates = result.get('candidates')
    if not isinstance(candidates, list):
        raise ScoreError('extraction_candidates_invalid')
    correct_candidate = bool(literal.strip()) and any(
        isinstance(candidate, dict) and isinstance(candidate.get('value'), str) and canonical(candidate['value'], field) == canonical(literal, field)
        for candidate in candidates
    )
    return {'status': selected_status, 'state': state, 'selected_correct': selected_status == 'correct',
            'manual_review_required': state == 'uncertain',
            'correct_candidate_available': bool(correct_candidate), 'expected_present': bool(literal.strip()),
            'selected_present': selected_present}


def build_report(completed: list[dict[str, Any]], reference_by_hash: dict[str, dict[str, Any]], audit_accepted: bool = False) -> dict[str, Any]:
    samples = []
    totals = {field: {'correct': 0, 'wrong': 0, 'abstain': 0, 'manual_review': 0, 'correct_candidate_available': 0} for field in FIELDS}
    optional_totals = {field: dict(correct=0, wrong=0, abstain=0, manual_review=0, correct_candidate_available=0) for field in OPTIONAL_FIELDS}
    for row in completed:
        reference = reference_by_hash[row['source_sha256']]
        ref_fields = reference.get('fields')
        extraction_fields = row['extraction'].get('fields') if isinstance(row['extraction'], dict) else None
        if not isinstance(ref_fields, dict) or not isinstance(extraction_fields, dict):
            raise ScoreError('field_map_invalid')
        scored = {}
        for field in ALL_FIELDS:
            expected_field = ref_fields.get(field)
            if field in OPTIONAL_FIELDS and expected_field is None:
                expected_field = {'literal': ''}
            metrics = score_field(extraction_fields.get(field), expected_field, field)
            scored[field] = metrics
            bucket = totals if field in FIELDS else optional_totals
            bucket[field][metrics['status']] += 1
            if metrics['manual_review_required']:
                bucket[field]['manual_review'] += 1
            if metrics['correct_candidate_available']:
                bucket[field]['correct_candidate_available'] += 1
        samples.append({'sample_id': row['sample_id'], 'fields': scored})
    return {'schema_version': 'passport-five-image-score/v2', 'status': 'completed', 'audit_accepted': audit_accepted,
            'dataset_role': 'development_photos_not_heldout',
            'limitation': 'sample-03-repeats-an-existing-document; samples-04-and-05-are-other-documents-without-MRZ', 'samples': samples,
            'required_fields': list(FIELDS), 'totals': totals, 'optional_totals': optional_totals,
            'policy': {'selected_status_uses_value_and_gold_match_only': True,
                       'uncertain_state_adds_overlapping_manual_review_flag': True}}


def render_markdown(report: dict[str, Any]) -> str:
    lines = ['# Five image OCR baseline score', '',
             f"- Integrity audit accepted: **{str(report['audit_accepted']).lower()}**",
             '- Dataset role: development photos, not held out; sample-03 repeats an existing document, while samples-04 and -05 are other documents without MRZ.',
             '- Correct/wrong/abstain classify the selected value. Manual review is a separate overlapping flag for uncertain state.', '',
             '| Sample | Selected correct | Selected wrong | Selected abstain | Manual review flags | Correct candidate available |', '|---|---:|---:|---:|---:|---:|']
    grand = {key: 0 for key in ('correct', 'wrong', 'abstain', 'manual_review', 'correct_candidate_available')}
    for sample in report['samples']:
        required_values = [sample['fields'][field] for field in FIELDS]
        counts = {key: sum(value['status'] == key for value in required_values) for key in ('correct', 'wrong', 'abstain')}
        counts['manual_review'] = sum(value['manual_review_required'] for value in required_values)
        counts['correct_candidate_available'] = sum(value['correct_candidate_available'] for value in required_values)
        for key in grand:
            grand[key] += counts[key]
        lines.append(f"| {sample['sample_id']} | {counts['correct']} | {counts['wrong']} | {counts['abstain']} | {counts['manual_review']} | {counts['correct_candidate_available']} |")
    lines.append(f"| **Total** | {grand['correct']} | {grand['wrong']} | {grand['abstain']} | {grand['manual_review']} | {grand['correct_candidate_available']} |")
    lines += ['', 'Required field totals:', '', '| Field ID | Selected correct | Selected wrong | Selected abstain | Manual review flags | Correct candidate available |', '|---|---:|---:|---:|---:|---:|']
    for field, counts in report['totals'].items():
        lines.append(f"| {field} | {counts['correct']} | {counts['wrong']} | {counts['abstain']} | {counts['manual_review']} | {counts['correct_candidate_available']} |")
    optional = report['optional_totals']['patronymic']
    lines += ['', 'Optional patronymic:', '',
              f"- Selected correct: {optional['correct']}; selected wrong: {optional['wrong']}; selected abstain: {optional['abstain']}; manual review flags: {optional['manual_review']}; correct candidate available: {optional['correct_candidate_available']}."]
    lines += ['', 'Patronymic is optional and reported separately in the JSON. No field values, source paths, or source hashes are included.']
    return '\n'.join(lines) + '\n'


def run(manifest: Path, old_gold_path: Path, new_gold_path: Path, output_dir: Path,
        integrity_audit_path: Path | None = None) -> None:
    # Important ordering: no reference file open occurs until all baseline checks pass.
    completed = validate_completed_baseline(manifest, integrity_audit_path=integrity_audit_path)
    old_gold = _json(old_gold_path, 'old_reference_invalid')
    new_gold = _json(new_gold_path, 'new_reference_invalid')
    reference = {}
    reference.update(index_reference(old_gold, completed, old_gold_path.resolve().parent, {'sample-01', 'sample-02'}))
    reference.update(index_reference(new_gold, completed, new_gold_path.resolve().parent, {'sample-03', 'sample-04', 'sample-05'}))
    report = build_report(completed, reference, audit_accepted=integrity_audit_path is not None)
    output_dir.mkdir(parents=True, exist_ok=False, mode=0o700)
    output_dir.chmod(0o700)
    write_private(output_dir / 'five-image-baseline.json', json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    write_private(output_dir / 'five-image-baseline.md', render_markdown(report))
    print('completed samples=5 required_fields=10')


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', type=Path, required=True, help='Completed driver manifest.json')
    parser.add_argument('--old-gold', type=Path, required=True, help='Old reference JSON for samples 01 and 02')
    parser.add_argument('--new-gold', type=Path, required=True, help='New reference JSON for samples 03 to 05')
    parser.add_argument('--output-dir', type=Path, required=True, help='New output directory; existing directories are refused')
    parser.add_argument('--integrity-audit', type=Path, help='Private, manifest-bound audit for the approved API-only source diff')
    args = parser.parse_args()
    os.umask(0o077)
    try:
        run(args.manifest.expanduser().resolve(), args.old_gold.expanduser().resolve(), args.new_gold.expanduser().resolve(),
            args.output_dir.expanduser().resolve(), args.integrity_audit.expanduser().resolve() if args.integrity_audit else None)
        return 0
    except ScoreError as error:
        print(f'failed status={error}', file=sys.stderr)
        return 2
    except Exception:
        print('failed status=unexpected_internal_error', file=sys.stderr)
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
