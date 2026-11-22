import json
from pathlib import Path
import unittest

class ProfileTests(unittest.TestCase):
 def test_location_contract(self):
  app=json.loads((Path(__file__).resolve().parents[1]/'examples/mcp/app.example.json').read_text())
  self.assertEqual(app['schema_version']['expected'],2)
  tools={tool['name']:tool for tool in app['tools']}
  self.assertEqual(set(tools),{'list_locations','get_latest','get_history','get_status','request_backfill','get_backfill'})
  for name in ['get_latest','get_history','get_status','request_backfill']:
   self.assertIn('location_id',tools[name]['input_schema']['required'])
   self.assertTrue(any(b['name']=='location_id' and b['source']=='argument' for b in tools[name]['bindings']))
  for name in ['get_latest','get_history','get_status','get_backfill','list_locations']:
   self.assertTrue(any(f['name']=='location_id' for f in tools[name]['fields']))
  self.assertIn('location_id',tools['request_backfill']['sql'])
if __name__=='__main__':unittest.main()
