from __future__ import annotations
import copy
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import numpy as np
from PIL import Image
from ocr.experiments import template_field_pipeline as template


def fixture():
    def line(id,text,x,y,w=160,h=28,upper=False):
        return {"id":id,"pass_id":"upper" if upper else "lower","text_raw":text,"score":.9,"quad":[[x,y],[x+w,y],[x+w,y+h],[x,y+h]]}
    image={"id":"derived","sha256":"a"*64,"width":1000,"height":1400,"coordinate_frame":"exif_normalized_pixels"}
    obs={"image":image,"document":{"seam_y":700},"passes":[{"id":p,"kind":p+"_page","status":"ok","rotation_degrees":0} for p in ("upper","lower")],
         "lines":[line("issue","02.03.2015",250,280,upper=True),line("dept","111-222",750,280,upper=True),line("family","АЛЬФОВ",620,825),line("name","ТЕСТ",620,910),line("patronymic","АЛЬФОВИЧ",620,965),line("birth","01.02.1990",650,1040),line("mrz","PNRUS<<TEST",100,1250,700)],
         "runtime":{"registered_spread":{"coordinate_space_role":"derived_registered_spread","selected_candidate_source":"contour"},"model_sha256":{"recognizer":"b"*64}}}
    lineage={"canonical_image":copy.deepcopy(image),"registration":{"status":"accepted"}}
    return obs,lineage


class TemplateTests(unittest.TestCase):
    def assert_go_name_crop_contract(self, observations, plan):
        image = observations['image']; seam = observations['document']['seam_y']
        lines = {line['id']: line for line in observations['lines']}
        anchors = plan['anchors']
        birth = lines[anchors['birth_date']]
        birth_y = min(point[1] for point in birth['quad'])
        name_boxes = {}
        for field in ('surname', 'given_name', 'patronymic'):
            line = lines[anchors[field]]
            q = line['quad']
            name_boxes[field] = [min(p[0] for p in q), min(p[1] for p in q), max(p[0] for p in q), max(p[1] for p in q)]
        for region in plan['regions']:
            if region['field_id'] not in name_boxes:
                continue
            x0, y0, x1, y1 = region['crop']; own = name_boxes[region['field_id']]
            self.assertTrue(all(isinstance(value, int) for value in region['crop']))
            self.assertGreaterEqual(x0, .40 * image['width'])
            self.assertLessEqual(x1, .92 * image['width'])
            self.assertGreater(y0, seam)
            self.assertLess(y1, birth_y)
            self.assertLessEqual(x0, own[0]); self.assertLessEqual(y0, own[1])
            self.assertGreaterEqual(x1, own[2]); self.assertGreaterEqual(y1, own[3])
            for other_field, other in name_boxes.items():
                if other_field == region['field_id']:
                    continue
                self.assertFalse(x0 < other[2] and other[0] < x1 and y0 < other[3] and other[1] < y1)

    def test_page_detection_is_restored_after_recognition_only_call(self):
        from types import SimpleNamespace
        from ocr.experiments.region_pipeline import _run_pass
        class StickyEngine:
            use_det=True
            def __call__(self,image,**kw):
                self.use_det=kw.get('use_det',self.use_det)
                if not self.use_det:return SimpleNamespace(txts=['SYNTHETIC'],scores=[.9],boxes=None)
                return SimpleNamespace(txts=['SYNTHETIC'],scores=[.9],boxes=np.array([[[1,1],[20,1],[20,15],[1,15]]]))
        engine=StickyEngine();pixels=np.zeros((30,50,3),dtype=np.uint8)
        engine(pixels,use_det=False)
        record,lines=_run_pass(engine,pixels,'whole','original',(0,0,50,30),0)
        self.assertEqual(record['status'],'ok');self.assertEqual(len(lines),1)
        self.assertTrue(engine.use_det)
    def test_lazy_recognition_guard_requires_only_used_stage(self):
        target='ocr.experiments.region_pipeline.cpu_provider_report'
        report={'sessions':{'rec':['CPUExecutionProvider'],'det':None,'nested_sessions':{'rec':['CPUExecutionProvider']}},'available':['CPUExecutionProvider','CoreMLExecutionProvider']}
        with mock.patch(target,return_value=report):
            template.verify_template_cpu(object(),mode='rec_only')
            with self.assertRaises(RuntimeError):template.verify_template_cpu(object(),mode='det_rec')
        report['sessions']['det']=['CPUExecutionProvider']
        with mock.patch(target,return_value=report):template.verify_template_cpu(object(),mode='det_rec')
        report['sessions']['rec']=['CoreMLExecutionProvider']
        with mock.patch(target,return_value=report):
            with self.assertRaises(RuntimeError):template.verify_template_cpu(object(),mode='rec_only')
    def test_plan_has_six_bounded_anchor_refined_fields(self):
        o,l=fixture();p=template.plan_template(o,l)
        self.assertEqual(p['status'],'ready');self.assertEqual(len(p['regions']),6)
        for r in p['regions']:
            self.assertTrue(0<=r['crop'][0]<r['crop'][2]<=1000)
            self.assertTrue(0<=r['crop'][1]<r['crop'][3]<=1400)
        place=next(r for r in p['regions'] if r['field_id']=='birth_place')
        self.assertGreater(place['crop'][1],1068);self.assertLess(place['crop'][3],1250)
        names={r['field_id']:r['crop'] for r in p['regions'] if r['field_id'] in ('surname','given_name','patronymic')}
        self.assertEqual(names, {'surname':[500,816,910,862], 'given_name':[500,901,910,947], 'patronymic':[500,956,910,1002]})
        self.assert_go_name_crop_contract(o,p)

    def test_name_padding_is_limited_by_close_rows_and_birth_date(self):
        o,l=fixture()
        coords={'family':(825,853),'name':(861,889),'patronymic':(895,923),'birth':(930,958)}
        for line in o['lines']:
            if line['id'] in coords:
                top,bottom=coords[line['id']]
                line['quad']=[[620,top],[780,top],[780,bottom],[620,bottom]]
        p=template.plan_template(o,l)
        self.assertEqual(p['status'],'ready')
        self.assert_go_name_crop_contract(o,p)

    def test_subpixel_name_anchors_are_contained_after_integer_rounding(self):
        o,l=fixture()
        shifts={'family':(.25,.25),'name':(.5,.5),'patronymic':(.75,.25),'birth':(.5,.5)}
        for line in o['lines']:
            if line['id'] in shifts:
                dx,dy=shifts[line['id']]
                line['quad']=[[x+dx,y+dy] for x,y in line['quad']]
        p=template.plan_template(o,l)
        self.assertEqual(p['status'],'ready')
        self.assert_go_name_crop_contract(o,p)

    def test_subpixel_tight_name_gap_skips_only_unrepresentable_regions(self):
        o,l=fixture()
        coordinates={'family':(825.25,853.7),'name':(853.8,881.8),'patronymic':(900.25,928.7)}
        for line in o['lines']:
            if line['id'] in coordinates:
                top,bottom=coordinates[line['id']]
                line['quad']=[[620.25,top],[780.25,top],[780.25,bottom],[620.25,bottom]]
        p=template.plan_template(o,l)
        self.assertEqual(p['status'],'ready')
        self.assertEqual([item['field_id'] for item in p['skipped_regions']],['surname','given_name'])
        self.assertEqual([item['reason'] for item in p['skipped_regions']],
                         ['no_integer_crop_between_anchor_constraints'] * 2)
        self.assertIn('patronymic',[r['field_id'] for r in p['regions']])
        self.assert_go_name_crop_contract(o,p)

    def test_uncontainable_name_anchor_skips_only_its_region(self):
        o,l=fixture()
        o['lines'][2]['quad'][1][0]=925
        o['lines'][2]['quad'][2][0]=925
        p=template.plan_template(o,l)
        self.assertEqual(p['status'],'ready')
        self.assertNotIn('surname',[r['field_id'] for r in p['regions']])
        self.assertEqual(p['skipped_regions'],[{'field_id':'surname','reason':'name_anchor_outside_integer_crop_limits'}])
        self.assertEqual(len(p['regions']),5)

    def test_name_anchor_crossing_birth_date_disables_plan(self):
        o,l=fixture()
        patronymic=next(line for line in o['lines'] if line['id']=='patronymic')
        patronymic['quad']=[[620,1000],[780,1000],[780,1045],[620,1045]]
        p=template.plan_template(o,l)
        self.assertEqual(p['status'],'skipped')
        self.assertEqual(p['reason'],'upright_three_name_rows_missing_or_ambiguous')

    def test_missing_rotated_duplicate_anchor_disables_template(self):
        for mutation in ('date','duplicate','rotation','sha','source'):
            o,l=fixture()
            if mutation=='date':o['lines']=[x for x in o['lines'] if x['id']!='birth']
            if mutation=='duplicate':o['lines'].append({**o['lines'][5],'id':'birth2'})
            if mutation=='rotation':o['passes'][1]['rotation_degrees']=180
            if mutation=='sha':l['canonical_image']['sha256']='c'*64
            if mutation=='source':o['runtime']['registered_spread']['selected_candidate_source']='line'
            self.assertEqual(template.plan_template(o,l)['status'],'skipped',mutation)

    def test_no_reference_or_selected_extraction_parameter(self):
        import inspect
        self.assertEqual(list(inspect.signature(template.plan_template).parameters),['observations','lineage'])
        o,l=fixture(); before=copy.deepcopy((o,l));template.plan_template(o,l)
        self.assertEqual((o,l),before)

    def test_reader_records_actual_mode_mapping_and_provider_each_call(self):
        o,l=fixture();calls=[];guards=[]
        def reader(engine,rgb,*,mode):
            calls.append(mode)
            # Detector box is crop-local; rec-only geometry covers full crop.
            return {'status':'ok','outputs':[{'text_raw':'ТЕСТ','raw_confidence':.9,'box_source':[2,3,22,23]}],'elapsed_seconds':.01}
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'test.png';Image.fromarray(np.zeros((1400,1000,3),dtype=np.uint8)).save(p)
            o['image']['sha256']=template.sha(p);l['canonical_image']['sha256']=o['image']['sha256']
            plan,e=template.read_template(p,o,l,engine=object(),reader=reader,provider_validator=lambda _,**kw:(guards.append(1) or {'cpu':True}))
            self.assertEqual(len(calls),8);self.assertEqual(len(guards),8)
            self.assertEqual(len(plan['row_readers']),2)
            for r in e['regions']:
                record=r['records'][0]
                self.assertEqual(r['mode'],r['reader_mode'])
                if r['mode']=='rec_only':self.assertEqual(record['box'],r['crop'])
                else:self.assertEqual(record['box'][0],r['crop'][0]+2)

    def test_wrong_raster_rejected_before_reader(self):
        o,l=fixture()
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'test.png';Image.fromarray(np.zeros((20,20,3),dtype=np.uint8)).save(p)
            reader=mock.Mock()
            with self.assertRaisesRegex(ValueError,'sha_mismatch'):template.read_template(p,o,l,engine=None,reader=reader,provider_validator=lambda _:None)
            reader.assert_not_called()

    def test_provider_failure_stops_before_next_crop(self):
        o,l=fixture();reader=mock.Mock(return_value={'status':'empty','outputs':[]})
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'test.png';Image.fromarray(np.zeros((1400,1000,3),dtype=np.uint8)).save(p)
            o['image']['sha256']=template.sha(p);l['canonical_image']['sha256']=o['image']['sha256']
            with self.assertRaises(RuntimeError):template.read_template(p,o,l,engine=None,reader=reader,provider_validator=mock.Mock(side_effect=RuntimeError('cpu')))
            self.assertEqual(reader.call_count,1)

if __name__=='__main__':unittest.main()
