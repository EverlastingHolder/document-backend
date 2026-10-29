#!/usr/bin/env python3
"""Local paired, cached-page OCR experiment. Gold is opened after all Go runs."""
from __future__ import annotations
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT))
from ocr.experiments import run_algorithm_comparison as metrics
from ocr.experiments.run_registration_first_comparison import _gold_index_by_source_sha
from ocr.experiments.registration_first_pipeline import _write_private_json, _private_directory
from ocr.experiments.template_field_pipeline import read_template, sha, verify_template_cpu, build_template_passport


def load(path):
    return json.loads(path.read_text(encoding='utf-8'))


def posthoc(gold_path, completed):
    if (len(completed)!=2 or {row['sample_id'] for row in completed}!={'sample-01','sample-02'}
            or len({row['source_sha'] for row in completed})!=2
            or any(set(row['arms'])!={'source_baseline','registered_baseline','template_augmented'} for row in completed)):
        raise ValueError('all_six_extractions_required_before_reference')
    index=_gold_index_by_source_sha(gold_path,{row['source_sha'] for row in completed})
    rows=[]
    for row in completed:
        fields=index[row['source_sha']][1]['fields']
        scored={}
        for arm, extraction in row['arms'].items():
            score=metrics.score_extraction(extraction,fields)
            for field, result in extraction['fields'].items():
                expected=fields[field]['literal']
                score['fields'][field]['correct_candidate_present']=any(metrics.canonical(c['value'],field)==metrics.canonical(expected,field) for c in result['candidates'])
                score['fields'][field]['candidate_count']=len(result['candidates'])
            scored[arm]=score
        rows.append({'sample_id':row['sample_id'],'template_status':row['plan']['status'],
                     'skip_reason':row['plan'].get('reason'),'regions':[{k:r[k] for k in ('field_id','mode','status','records')} for r in row['plan'].get('actual_readers',[])],
                     'ocr_seconds':row['ocr_seconds'],'scores':scored})
    return rows


def worker(args):
    os.umask(0o077)
    cache=args.cache_root.resolve(); work=args.work_root.resolve(); out=args.output_dir.resolve()
    _private_directory(work);_private_directory(out)
    models=ROOT/'ocr/experiments/models'
    metrics.check_toolchain(Path(sys.executable).resolve(),args.go.resolve(),models)
    env=os.environ.copy();env.update({'GOPROXY':'off','GOSUMDB':'off','GOTOOLCHAIN':'local'})
    evaluator=work/'ocr-eval'
    build=subprocess.run([str(args.go),'build','-o',str(evaluator),'./cmd/ocr-eval'],cwd=ROOT,env=env,capture_output=True,timeout=60)
    if build.returncode:raise RuntimeError('go_build_failed')
    evaluator.chmod(0o700)
    before={}; completed=[]; engine=None
    from ocr.experiments.region_pipeline import create_cpu_engine
    for i in (1,2):
        sample=f'sample-{i:02d}'; source=cache/sample; dest=work/sample;_private_directory(dest)
        obs_path=source/'observations.json';baseline_path=source/'baseline-observations.json'
        # Fallback cache deliberately has no duplicate baseline observation file.
        if not baseline_path.exists():baseline_path=obs_path
        obs=load(obs_path);base=load(baseline_path)
        for path in (obs_path,baseline_path):before[str(path)]=sha(path)
        lineage_path=source/'registration-lineage.json';image_path=source/'canonical-spread.png'
        registered=obs.get('runtime',{}).get('registered_spread')
        evidence_path=None;started=time.perf_counter()
        if args.fresh:
            if not args.image or len(args.image)!=2:raise ValueError('fresh_route_requires_two_images')
            image_input=args.image[i-1].resolve()
            if sha(image_input)!=base['image']['sha256']:raise ValueError('fresh_source_sha_mismatch')
            before[str(image_input)]=sha(image_input)
            fresh=build_template_passport(image_input,sample,models/metrics.MODEL_HASHES['det'][0],models/metrics.MODEL_HASHES['rec'][0],private_output_dir=dest)
            obs=fresh['observations']
            plan=fresh['template_plan']
            evidence_path=Path(fresh['template_evidence_path']) if fresh['template_evidence_path'] else None
            obs_path=dest/'fresh-observations.json';_write_private_json(obs_path,obs)
            if fresh['status']=='accepted':
                canonical=Path(fresh['canonical_image_path'])
                if sha(canonical)!=obs['image']['sha256']:raise ValueError('fresh_canonical_sha_mismatch')
        elif registered:
            lineage=load(lineage_path)
            if lineage['source_image']['sha256']!=base['image']['sha256']:raise ValueError('lineage_source_mismatch')
            for path in (lineage_path,image_path):before[str(path)]=sha(path)
            if engine is None:
                engine=create_cpu_engine(models/metrics.MODEL_HASHES['det'][0],models/metrics.MODEL_HASHES['rec'][0])
            plan,evidence=read_template(image_path,obs,lineage,engine=engine,provider_validator=verify_template_cpu)
            if plan['status']=='ready':
                evidence_path=dest/'template-evidence.json';_write_private_json(evidence_path,evidence)
        else:plan={'status':'skipped','reason':'physical_registration_unverified','regions':[]}
        elapsed=time.perf_counter()-started
        _write_private_json(dest/'plan.json',plan)
        arms={}
        for arm,inp in (('source_baseline',baseline_path),('registered_baseline',obs_path),('template_augmented',obs_path)):
            path=dest/f'{arm}.json'
            command=[str(evaluator),'--input',str(inp),'--output',str(path),'--passes=all','--normalize-names=true','--mrz-policy=augment']
            if arm=='template_augmented' and evidence_path is not None:command+=['--template-evidence',str(evidence_path)]
            run=subprocess.run(command,cwd=ROOT,env=env,capture_output=True,timeout=20)
            _write_private_json(dest/f'{arm}-execution.json',{'argv':command,'exit_code':run.returncode})
            if run.returncode:raise RuntimeError(f'go_{arm}_failed')
            extraction=load(path)
            expected=base if arm=='source_baseline' else obs
            if extraction['image_id']!=expected['image']['id']:raise ValueError('extraction_image_mismatch')
            arms[arm]=extraction
        completed.append({'sample_id':sample,'source_sha':base['image']['sha256'],'plan':plan,'ocr_seconds':elapsed,'arms':arms})
    for path,digest in before.items():
        if sha(Path(path))!=digest:raise ValueError('input_artifact_changed')
    # Neither planning, recognition nor Go has access to a reference value.
    rows=posthoc(args.gold.resolve(),completed)
    report={'schema_version':'passport-template-comparison/v1','status':'completed','created_at':datetime.now(timezone.utc).isoformat(),
            'scope':'two development images; six selected fields on verified registered layouts; all proposals require manual review',
            'reference_loaded_after_all_six_go_outputs':True,
            'fresh_registration_and_page_ocr':args.fresh,'source_baseline_ocr_reused_from_frozen_cache':True,
            'samples':rows,'totals':{}}
    for arm in ('source_baseline','registered_baseline','template_augmented'):
        report['totals'][arm]={k:sum(r['scores'][arm]['totals'][k] for r in rows) for k in ('required_suggested_correct','required_wrong_suggestion','required_missing_or_abstained')}
        report['totals'][arm]['required_correct_candidate_present']=sum(row['scores'][arm]['fields'][field]['correct_candidate_present'] for row in rows for field in metrics.REQUIRED_FIELDS)
    _write_private_json(out/'template-field-comparison.json',report)
    lines=['# Anchor-refined template OCR pilot','','Two development images; selected suggestions remain uncertain.','','| Arm | Correct | Wrong | Abstain |','|---|---:|---:|---:|']
    for arm,t in report['totals'].items():lines.append(f"| {arm} | {t['required_suggested_correct']} | {t['required_wrong_suggestion']} | {t['required_missing_or_abstained']} |")
    lines+=['','Crops complement page OCR; conflicts retain both candidates without choosing by score.','No raw OCR, reference values or source images are included here.']
    report_path=out/'template-field-comparison.md';report_path.write_text('\n'.join(lines)+'\n',encoding='utf-8');report_path.chmod(0o600)
    sources=[ROOT/name for name in ('internal/passport/template_evidence.go','internal/passport/result.go','internal/passport/extract.go','cmd/ocr-eval/main.go','ocr/experiments/run_template_field_comparison.py','ocr/experiments/template_field_pipeline.py','ocr/experiments/test_template_field_pipeline.py','internal/passport/template_evidence_test.go','ocr/experiments/test_template_field_runner.py','ocr/experiments/registration_first_pipeline.py','ocr/experiments/region_pipeline.py','ocr/schema/template-evidence-v1.schema.json')]
    _write_private_json(work/'manifest.json',{'inputs_before_after_sha256':before,'source_sha256':{str(p):sha(p) for p in sources},'go_binary_sha256':sha(evaluator),'gold_used_posthoc_only':True})
    print('completed samples=2 paired_arms=3')
    return 0


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--cache-root',type=Path,required=True);p.add_argument('--work-root',type=Path,required=True)
    p.add_argument('--output-dir',type=Path,required=True);p.add_argument('--gold',type=Path,required=True)
    p.add_argument('--go',type=Path,default=Path('/opt/homebrew/bin/go'));p.add_argument('--worker',action='store_true',help=argparse.SUPPRESS)
    p.add_argument('--fresh',action='store_true',help='Run the full registration/page/field route again on the two sources')
    p.add_argument('--image',action='append',type=Path)
    args=p.parse_args()
    if args.worker:
        try:return worker(args)
        except Exception as error:
            _private_directory(args.work_root)
            import traceback
            path=args.work_root/'failure-private.txt';path.write_text(traceback.format_exc());path.chmod(0o600)
            print('template comparison failed; private diagnostics saved',file=sys.stderr)
            return 1
    process=subprocess.Popen([sys.executable,str(Path(__file__).resolve()),*sys.argv[1:],'--worker'],cwd=ROOT,start_new_session=True)
    try:return process.wait(timeout=180)
    except subprocess.TimeoutExpired:
        for sig in (signal.SIGTERM,signal.SIGKILL):
            try:os.killpg(process.pid,sig)
            except ProcessLookupError:pass
            if sig==signal.SIGTERM:time.sleep(1)
        process.wait();return 124

if __name__=='__main__':raise SystemExit(main())
