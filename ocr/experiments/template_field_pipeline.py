"""Anchor-refined Russian internal-passport template on registered pages.

The template is a broad layout prior, not a pixel-perfect passport measurement.
It is enabled only on a SHA-bound, registered raster with independently observed
upright page anchors. No labels, reference values or dictionaries are invented.
"""
from __future__ import annotations

import copy
from datetime import datetime
import hashlib
import math
from pathlib import Path
import re
from typing import Any, Callable

import numpy as np
from PIL import Image

from ocr.experiments.field_roi_pipeline import rapidocr_reader

TEMPLATE_ID = "ru-internal-spread-anchored/v1"
FIELDS = ("surname", "given_name", "patronymic", "gender", "birth_place", "issuer")
DATE = re.compile(r"^(?:\d{2}[.-]\d{2}[.-]\d{4}|\d{4}-\d{2}-\d{2})$")
NAME = re.compile(r"^[А-ЯЁA-Z][А-ЯЁA-Z -]{1,55}$", re.I)
DEPARTMENT = re.compile(r"^\d{3}\s*[- ]\s*\d{3}$")
NAME_LABELS = (
    "ФАМИЛИЯ", "ИМЯ", "ОТЧЕСТВО", "ПОЛ", "ДАТАРОЖДЕНИЯ", "МЕСТОРОЖДЕНИЯ",
    "СЕРИЯ", "НОМЕР", "ДАТАВЫДАЧИ", "КЕМВЫДАН", "ОРГАНВЫДАВШИЙ", "КОДПОДРАЗДЕЛЕНИЯ",
)
LABEL_LOOKALIKE = str.maketrans({"A": "А", "B": "В", "C": "С", "E": "Е", "H": "Н",
                                  "K": "К", "M": "М", "O": "О", "P": "Р", "T": "Т",
                                  "X": "Х", "Y": "У"})


def verify_template_cpu(engine: Any, *, mode: str) -> dict:
    from ocr.experiments.region_pipeline import cpu_provider_report
    report = cpu_provider_report(engine)
    sessions = report.get("sessions", {})
    required = ("rec",) if mode == "rec_only" else ("det", "rec")
    if any(sessions.get(role) != ["CPUExecutionProvider"] for role in required):
        raise RuntimeError("template_reader_provider_mismatch")
    if any(sessions.get(role) != ["CPUExecutionProvider"] for role in ("det", "rec") if sessions.get(role) is not None):
        raise RuntimeError("template_reader_provider_mismatch")
    return report


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def box(line: dict) -> list[float]:
    q = np.asarray(line["quad"], dtype=float)
    if q.shape != (4, 2) or not np.isfinite(q).all():
        raise ValueError("invalid_anchor_geometry")
    return [float(q[:, 0].min()), float(q[:, 1].min()), float(q[:, 0].max()), float(q[:, 1].max())]


def date(text: str) -> bool:
    if not DATE.fullmatch(text.strip()):
        return False
    try:
        value = text.strip()
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}", value):
            datetime.strptime(value, "%Y-%m-%d")
        else:
            datetime.strptime(value.replace("-", "."), "%d.%m.%Y")
        return True
    except ValueError:
        return False


def _rect(x0: float, y0: float, x1: float, y1: float, w: int, h: int) -> list[int]:
    r = [max(0, math.floor(x0)), max(0, math.floor(y0)), min(w, math.ceil(x1)), min(h, math.ceil(y1))]
    if r[0] >= r[2] or r[1] >= r[3]:
        raise ValueError("empty_template_crop")
    return r


def _has_label(text: str) -> bool:
    canonical = "".join(char.translate(LABEL_LOOKALIKE) for char in text.upper() if char.isalnum())
    return any(term in canonical for term in NAME_LABELS)


def _name_crop(anchor: list[float], other_anchors: list[list[float]], *, seam: float,
               birth_y: float, name_left: float, padding: float, w: int, h: int) -> tuple[list[int] | None, str | None]:
    """Return an integer crop that satisfies the Go template geometry contract."""
    ax0, ay0, ax1, ay1 = anchor
    min_x0, max_x0 = math.ceil(.40 * w), math.floor(ax0)
    min_x1, max_x1 = math.ceil(ax1), math.floor(.92 * w)
    if min_x0 > max_x0 or min_x1 > max_x1:
        return None, "name_anchor_outside_integer_crop_limits"

    min_y0 = math.floor(seam) + 1
    max_y0 = math.floor(ay0)
    min_y1 = math.ceil(ay1)
    max_y1 = math.ceil(birth_y) - 1  # Go requires crop bottom < the DOB anchor top.
    for other in other_anchors:
        if other[3] <= ay0:
            min_y0 = max(min_y0, math.ceil(other[3]))
        elif other[1] >= ay1:
            max_y1 = min(max_y1, math.floor(other[1]))
        else:
            return None, "name_anchors_overlap"
    if min_y0 > max_y0 or min_y1 > max_y1:
        return None, "no_integer_crop_between_anchor_constraints"

    x0 = min(max(math.floor(name_left), min_x0), max_x0)
    x1 = min(max(math.ceil(max(.91 * w, ax1 + padding)), min_x1), max_x1)
    y0 = min(max(math.floor(ay0 - padding), min_y0), max_y0)
    y1 = min(max(math.ceil(ay1 + padding), min_y1), max_y1)
    if x0 >= x1 or y0 >= y1:
        return None, "empty_integer_name_crop"
    return [x0, y0, x1, y1], None


def plan_template(observations: dict, lineage: dict) -> dict:
    """Pure, reference-free planning; ambiguous/missing anchors disable template."""
    image = observations["image"]
    w, h = image["width"], image["height"]
    seam = observations["document"].get("seam_y")
    runtime = observations.get("runtime", {}).get("registered_spread", {})
    canonical = lineage.get("canonical_image", {})
    registration = lineage.get("registration", {})
    if (runtime.get("coordinate_space_role") != "derived_registered_spread"
            or runtime.get("selected_candidate_source") != "contour"
            or canonical.get("sha256") != image["sha256"]
            or canonical.get("id") != image["id"]
            or canonical.get("width") != w or canonical.get("height") != h
            or registration.get("status") != "accepted"
            or seam is None or not .35 * h < seam < .65 * h):
        return {"status": "skipped", "reason": "verified_registered_frame_required", "regions": []}
    passes = {p["id"]: p for p in observations["passes"] if p["status"] == "ok" and p["rotation_degrees"] == 0}
    lines = [l for l in observations["lines"] if l["pass_id"] in passes and "<" not in l["text_raw"]]
    upper = [l for l in lines if passes[l["pass_id"]]["kind"] == "upper_page"]
    lower = [l for l in lines if passes[l["pass_id"]]["kind"] == "lower_page"]
    ph = h - seam
    upper_dates = [l for l in upper if date(l["text_raw"]) and .25 * seam < box(l)[1] < .62 * seam]
    lower_dates = [l for l in lower if date(l["text_raw"]) and seam + .32 * ph < box(l)[1] < seam + .62 * ph and box(l)[0] > .5 * w]
    if len(upper_dates) != 1 or len(lower_dates) != 1:
        return {"status": "skipped", "reason": "upright_date_anchors_missing_or_ambiguous", "regions": []}
    ud, bd = upper_dates[0], lower_dates[0]
    ub, db = box(ud), box(bd)
    names = [l for l in lower if l["pass_id"] == bd["pass_id"] and NAME.fullmatch(l["text_raw"].strip())
             and box(l)[0] >= .48 * w and seam + .08 * ph < box(l)[1] < db[1]
             and box(l)[2] <= .93 * w and box(l)[3] - box(l)[1] < .09 * ph]
    names.sort(key=lambda l: box(l)[1])
    if (len(names) != 3 or max(box(l)[0] for l in names) - min(box(l)[0] for l in names) > .16 * w
            or any(box(names[i])[3] >= box(names[i + 1])[1] for i in range(2))
            or any(box(line)[3] >= db[1] or not NAME.fullmatch(line["text_raw"].strip())
                   or not any(char.isalpha() for char in line["text_raw"])
                   or len(line["text_raw"].strip()) > 60 or any(char.isdigit() for char in line["text_raw"])
                   or _has_label(line["text_raw"]) for line in names)):
        return {"status": "skipped", "reason": "upright_three_name_rows_missing_or_ambiguous", "regions": []}
    # Broad expected name lane; vertical borders are refined to observed rows.
    name_left = max(.40 * w, min(box(l)[0] for l in names) - .12 * w)
    anchors = {"issue_date": ud["id"], "birth_date": bd["id"],
               "surname": names[0]["id"], "given_name": names[1]["id"], "patronymic": names[2]["id"]}
    regions = []
    skipped_regions = []
    name_boxes = [box(line) for line in names]
    for index, (field, line) in enumerate(zip(FIELDS[:3], names)):
        b = box(line)
        padding = .30 * (b[3] - b[1])
        r, skip_reason = _name_crop(b, name_boxes[:index] + name_boxes[index + 1:],
                                    seam=seam, birth_y=db[1], name_left=name_left,
                                    padding=padding, w=w, h=h)
        if r is None:
            skipped_regions.append({"field_id": field, "reason": skip_reason})
            continue
        regions.append({"field_id": field, "crop": r, "mode": "rec_only", "anchor_ids": [line["id"], bd["id"]]})
    # A DOB anchors the field row. Exclude the date column, portrait and MRZ.
    regions.append({"field_id": "gender", "crop": _rect(.38*w, db[1]-.012*ph, min(.57*w, db[0]-.08*w), db[3]+.012*ph, w,h), "mode": "rec_only", "anchor_ids": [bd["id"]]})
    mrz = [box(l)[1] for l in observations["lines"] if l["pass_id"] == bd["pass_id"] and "<" in l["text_raw"] and box(l)[1] > db[3]]
    place_bottom = min(seam + .76 * ph, min(mrz) - .04 * ph if mrz else h)
    regions.append({"field_id": "birth_place", "crop": _rect(.44*w, db[3]+.006*ph, .91*w, place_bottom,w,h), "mode": "det_rec", "anchor_ids": [bd["id"]]})
    department = [l for l in upper if l["pass_id"] == ud["pass_id"] and DEPARTMENT.fullmatch(l["text_raw"].strip()) and abs(box(l)[1]-ub[1]) < .06*seam]
    if len(department) == 1:
        anchors["department_code"] = department[0]["id"]
        regions.append({"field_id": "issuer", "crop": _rect(.24*w, .13*seam, .90*w, ub[1]-.025*seam,w,h), "mode": "det_rec", "anchor_ids": [ud["id"], department[0]["id"]]})
    plan = {"status": "ready", "template_id": TEMPLATE_ID, "orientation": "upright_supported_by_two_dates_and_three_name_rows",
            "image": copy.deepcopy(image), "anchors": anchors, "regions": regions}
    if skipped_regions:
        plan["skipped_regions"] = skipped_regions
    return plan


def read_template(image_path: Path, observations: dict, lineage: dict, *, engine: Any,
                  provider_validator: Callable, reader: Callable = rapidocr_reader) -> tuple[dict, dict]:
    """Read each planned region once. No field values are selected in Python."""
    if sha(image_path) != observations["image"]["sha256"]:
        raise ValueError("template_raster_sha_mismatch")
    plan = plan_template(observations, lineage)
    evidence = {"schema_version": "passport-template-evidence/v1", "template_id": TEMPLATE_ID,
                "image": copy.deepcopy(observations["image"]), "anchors": plan.get("anchors", {}), "regions": []}
    if plan["status"] != "ready":
        return plan, evidence
    with Image.open(image_path) as opened:
        rgb = np.asarray(opened.convert("RGB"))
    if [rgb.shape[1], rgb.shape[0]] != [observations["image"]["width"], observations["image"]["height"]]:
        raise ValueError("template_raster_size_mismatch")
    for region in plan["regions"]:
        x0,y0,x1,y1 = region["crop"]
        result = reader(engine, rgb[y0:y1,x0:x1], mode=region["mode"])
        # Verify the actual sessions used immediately after every call.
        providers = provider_validator(engine, mode=region["mode"])
        records = []
        for output in result.get("outputs", []):
            raw = output.get("text_raw")
            score = output.get("raw_confidence")
            if not isinstance(raw, str) or not raw.strip() or not isinstance(score,(int,float)) or not math.isfinite(score) or not 0 <= score <= 1:
                continue
            b = output.get("box_source")
            if region["mode"] == "rec_only":
                if len(result.get("outputs", [])) != 1:
                    continue
                b = [0,0,x1-x0,y1-y0]
                scope = "entire_crop"
            else:
                scope = "detector_box"
            if not isinstance(b,list) or len(b)!=4 or not all(math.isfinite(v) for v in b) or not (0<=b[0]<b[2]<=x1-x0 and 0<=b[1]<b[3]<=y1-y0):
                continue
            records.append({"text_raw": raw, "score": score, "box": [b[0]+x0,b[1]+y0,b[2]+x0,b[3]+y0], "geometry_scope": scope, "reader_mode": region["mode"]})
        alternatives = []
        # Detector fragments may split one printed row into several words.
        # Re-read each geometrically observed row as a strip, retaining the
        # detector output as a separate hypothesis. No spelling/voting/gold.
        if region["mode"] == "det_rec" and records:
            rows: list[list[dict]] = []
            for record in sorted(records,key=lambda r: (r["box"][1]+r["box"][3])/2):
                b=record["box"]
                if rows:
                    a=rows[-1][0]["box"]
                    same_row=abs((a[1]+a[3]-b[1]-b[3])/2) < .5*min(a[3]-a[1],b[3]-b[1])
                else:same_row=False
                if same_row:rows[-1].append(record)
                else:rows.append([record])
            refined=[]
            if len(rows)<=4:
                for row in rows:
                    row.sort(key=lambda r:r['box'][0])
                    if any(row[i+1]['box'][0]-row[i]['box'][2]>2*max(row[i]['box'][3]-row[i]['box'][1],row[i+1]['box'][3]-row[i+1]['box'][1]) for i in range(len(row)-1)):
                        refined=[];break
                    left=min(r['box'][0] for r in row);right=max(r['box'][2] for r in row)
                    top=min(r['box'][1] for r in row);bottom=max(r['box'][3] for r in row)
                    pad=.25*(bottom-top)
                    # Keep the whole template lane: tight detector word boxes
                    # can omit a short prefix or the first/last character.
                    rb=_rect(x0,max(y0,top-pad),x1,min(y1,bottom+pad),rgb.shape[1],rgb.shape[0])
                    a,b,c,d=rb
                    reread=reader(engine,rgb[b:d,a:c],mode='rec_only')
                    provider_validator(engine,mode='rec_only')
                    plan.setdefault('row_readers',[]).append({'field_id':region['field_id'],'mode':'rec_only','status':reread['status']})
                    result_outputs=reread.get('outputs',[])
                    if reread['status']!='ok' or len(result_outputs)!=1:
                        refined=[];break
                    output=result_outputs[0];score=output.get('raw_confidence');raw=output.get('text_raw')
                    if not isinstance(raw,str) or not raw.strip() or not isinstance(score,(int,float)) or not math.isfinite(score) or not 0<=score<=1:
                        refined=[];break
                    refined.append({'text_raw':raw,'score':score,'box':rb,'geometry_scope':'row_crop','reader_mode':'rec_only'})
                if refined:alternatives.append(refined)
        evidence["regions"].append({**region, "reader": "rapidocr", "reader_mode": region["mode"],
                                    "reader_model_sha256": observations["runtime"]["model_sha256"]["recognizer"],
                                    "status": result["status"], "elapsed_seconds": result.get("elapsed_seconds",0),
                                    "records": records, "alternatives":alternatives})
        plan.setdefault("actual_readers", []).append({"field_id": region["field_id"], "mode":region["mode"], "status": result["status"], "records":len(records),"providers":providers})
    if sha(image_path) != observations["image"]["sha256"]:
        raise ValueError("template_raster_changed")
    return plan, evidence


def build_template_passport(image_path: Path, image_id: str, detector: Path, recognizer: Path,
                            *, private_output_dir: Path) -> dict:
    """Reusable complete route: physical registration -> pages -> field crops.

    The caller sends observations and optional template_evidence_path to the
    Go evaluator. A rejected registration never enables a template on source
    photo coordinates. No reference data is accepted by this function.
    """
    import json
    from ocr.experiments.registration_first_pipeline import build_registered_observations, _write_private_json
    from ocr.experiments.region_pipeline import create_cpu_engine
    engines = []
    def factory(det, rec):
        engine = create_cpu_engine(det, rec)
        engines.append(engine)
        return engine
    result = build_registered_observations(image_path,image_id,detector,recognizer,
        private_output_dir=private_output_dir,engine_factory=factory)
    if result['status']!='accepted':
        result['template_plan']={'status':'skipped','reason':'physical_registration_unverified','regions':[]}
        result['template_evidence_path']=None
        return result
    lineage=json.loads(Path(result['lineage_path']).read_text(encoding='utf-8'))
    plan,evidence=read_template(Path(result['canonical_image_path']),result['observations'],lineage,
        engine=engines[-1],provider_validator=verify_template_cpu)
    result['template_plan']=plan
    result['template_evidence_path']=None
    _write_private_json(private_output_dir/'template-plan.json',plan)
    if plan['status']=='ready':
        path=private_output_dir/'template-evidence.json'
        _write_private_json(path,evidence)
        result['template_evidence_path']=str(path)
    return result
