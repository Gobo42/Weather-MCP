"""Administrative schema2 migration acceptance on an explicit private fixture."""
import os
from pathlib import Path
import subprocess
import unittest

ROOT=Path(__file__).resolve().parents[1]
BASE=Path('/build/weather-agent/runtime/extracted/usr')
class MigrationTests(unittest.TestCase):
 def setUp(self):
  self.socket=Path(os.environ['WEATHER_TEST_SOCKET']).resolve()
  assert self.socket.is_relative_to('/build/weather-agent') and self.socket.is_socket()
  self.sql("DROP DATABASE IF EXISTS weatheragent; CREATE DATABASE weatheragent CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; CREATE USER IF NOT EXISTS 'weatheragent'@'localhost' IDENTIFIED BY 'weather-fixture-collector-password-000000'; CREATE USER IF NOT EXISTS 'weather_mcp'@'localhost' IDENTIFIED BY 'weather-fixture-mcp-password-00000000';")
  self.sql((ROOT/'sql/migrations/001_initial.sql').read_text())
 def sql(self,source,success=True):
  result=subprocess.run([str(BASE/'bin/mariadb'),'--no-defaults','--socket='+str(self.socket),'-uroot','--batch','--skip-column-names','--abort-source-on-error'],input=source,text=True,capture_output=True,env=dict(os.environ,LD_LIBRARY_PATH=str(BASE/'lib/x86_64-linux-gnu')))
  self.assertEqual(result.returncode==0,success,result.stderr)
  return result.stdout.strip()
 def migrate(self,location='home',success=True):
  text=(ROOT/'sql/migrations/002_multiple_locations.sql').read_text()
  return self.sql("SET @legacy_location_id='"+location+"';\n"+text,success)
 def populate(self):
  self.sql("""USE weatheragent;
INSERT INTO location(id,name,latitude,longitude,timezone) VALUES('home','Synthetic',40,-105,'America/Los_Angeles');
INSERT INTO measurements(location_id,source,selection,observed_at,retrieved_at,interval_seconds,temperature_c,humidity_pct) VALUES('home','current','best_match','2020-01-01','2020-01-01',900,20,50);
INSERT INTO backfill_requests(request_id,start_date,end_date) VALUES('11111111-1111-4111-8111-111111111111','2020-01-01','2020-01-01');
UPDATE collector_status SET last_success_at='2020-01-01',failures=2;""")
 def test_empty_prepared_schema(self):
  self.migrate()
  self.assertEqual(self.sql('USE weatheragent; SELECT MAX(version) FROM schema_migrations; SELECT COUNT(*) FROM location; SELECT COUNT(*) FROM collector_status;'),'2\n0\n0')
  self.assertIn('INSERT',self.sql("SHOW GRANTS FOR 'weatheragent'@'localhost';"))
  index=self.sql("SELECT TABLE_NAME,GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='weatheragent' AND INDEX_NAME IN('history_time','pending_work') GROUP BY TABLE_NAME ORDER BY TABLE_NAME;")
  self.assertEqual(index,'backfill_requests\tlocation_id,state,available_at,created_at,request_id\nmeasurements\tlocation_id,source,observed_at,id')
 def test_populated_schema_preserves_identity_and_jobs(self):
  self.populate();self.migrate()
  self.assertEqual(self.sql("USE weatheragent; SELECT location_id,temperature_c FROM measurements; SELECT location_id,failures FROM collector_status; SELECT location_id,request_id FROM backfill_requests;"),'home\t20\nhome\t2\nhome\t11111111-1111-4111-8111-111111111111')
  self.assertEqual(self.sql("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='weatheragent' AND TABLE_NAME='location' AND COLUMN_NAME='slot';"),'0')
  self.migrate(success=False)
 def test_mapping_mismatch_refused_before_ddl(self):
  self.populate();self.migrate('wrong',False)
  self.assertEqual(self.sql('USE weatheragent; SELECT MAX(version) FROM schema_migrations; SELECT COUNT(*) FROM location;'),'1\n1')
  self.assertEqual(self.sql("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='weatheragent' AND TABLE_NAME='location' AND COLUMN_NAME='slot';"),'1')
 def test_orphan_status_refused_before_ddl(self):
  self.sql("USE weatheragent; UPDATE collector_status SET last_error_code='orphan';")
  self.migrate(success=False)
  self.assertEqual(self.sql('USE weatheragent; SELECT MAX(version) FROM schema_migrations;'),'1')
 def test_partial_migration_refused(self):
  self.sql("USE weatheragent; CREATE TABLE collector_status_v2(id INT);")
  self.migrate(success=False)
  self.assertEqual(self.sql('USE weatheragent; SELECT MAX(version) FROM schema_migrations;'),'1')
  self.assertEqual(self.sql("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='weatheragent' AND TABLE_NAME='location' AND COLUMN_NAME='slot';"),'1')
if __name__=='__main__':unittest.main()
