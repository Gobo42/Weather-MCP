-- Administrative maintenance migration. Stop collector and Weather MCP access,
-- take a recoverable backup, set @legacy_location_id to the existing config ID,
-- then SOURCE this file with --abort-source-on-error. Never run with --force.
-- DDL commits implicitly: on failure keep services stopped and restore the backup.
USE weatheragent;
SET SESSION time_zone='+00:00';
DELIMITER //
BEGIN NOT ATOMIC
 DECLARE location_count INT;
 DECLARE measurement_count BIGINT;
 DECLARE job_count BIGINT;
 DECLARE status_count INT;
 DECLARE EXIT HANDLER FOR SQLEXCEPTION
 BEGIN
  DO RELEASE_LOCK('weatheragent:schema');
  RESIGNAL;
 END;
 IF COALESCE(GET_LOCK('weatheragent:schema',10),0)<>1 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Cannot acquire schema lock';
 END IF;
 IF (SELECT COUNT(*) FROM schema_migrations)<>1 OR (SELECT MAX(version) FROM schema_migrations)<>1 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Migration requires untouched schema version 1';
 END IF;
 SELECT COUNT(*) INTO location_count FROM location;
 SELECT COUNT(*) INTO measurement_count FROM measurements;
 SELECT COUNT(*) INTO job_count FROM backfill_requests;
 SELECT COUNT(*) INTO status_count FROM collector_status;
 IF location_count>1 OR status_count<>1 OR NOT EXISTS(SELECT 1 FROM collector_status WHERE id=1) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Ambiguous legacy location/status mapping';
 END IF;
 IF location_count=1 AND (@legacy_location_id IS NULL OR NOT EXISTS(SELECT 1 FROM location WHERE BINARY id=BINARY @legacy_location_id)) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Supply the original configured location ID';
 END IF;
 IF location_count=0 AND (measurement_count<>0 OR job_count<>0 OR EXISTS(
  SELECT 1 FROM collector_status WHERE last_attempt_at IS NOT NULL OR last_success_at IS NOT NULL
   OR next_attempt_at IS NOT NULL OR provider_not_before IS NOT NULL OR failures<>0 OR last_error_code<>'')) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Cannot map orphan legacy data to a location';
 END IF;
 IF EXISTS(SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='collector_status_v2')
 OR EXISTS(SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='backfill_requests' AND COLUMN_NAME='location_id') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Partial migration detected; restore the maintenance backup';
 END IF;
 IF (SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=DATABASE() AND TABLE_NAME='location' AND CONSTRAINT_TYPE='CHECK')<>1 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Unexpected location constraints; inspect before migration';
 END IF;
 ALTER TABLE location DROP PRIMARY KEY, DROP COLUMN slot, ADD PRIMARY KEY(id);
 CREATE TABLE collector_status_v2(
  location_id VARCHAR(32) COLLATE utf8mb4_bin NOT NULL PRIMARY KEY,
  last_attempt_at DATETIME(6) NULL, last_success_at DATETIME(6) NULL,
  next_attempt_at DATETIME(6) NULL, provider_not_before DATETIME(6) NULL,
  failures INT NOT NULL DEFAULT 0, last_error_code VARCHAR(64) NOT NULL DEFAULT '',
  FOREIGN KEY(location_id) REFERENCES location(id)
 ) ENGINE=InnoDB;
 INSERT INTO collector_status_v2 SELECT location.id,old.last_attempt_at,old.last_success_at,
  old.next_attempt_at,old.provider_not_before,old.failures,old.last_error_code
 FROM collector_status AS old CROSS JOIN location;
 DROP TABLE collector_status;
 RENAME TABLE collector_status_v2 TO collector_status;
 ALTER TABLE backfill_requests ADD COLUMN location_id VARCHAR(32) COLLATE utf8mb4_bin NULL AFTER request_id;
 UPDATE backfill_requests SET location_id=@legacy_location_id;
 ALTER TABLE backfill_requests MODIFY location_id VARCHAR(32) COLLATE utf8mb4_bin NOT NULL,
  ADD CONSTRAINT backfill_location FOREIGN KEY(location_id) REFERENCES location(id),
  DROP INDEX pending_work, ADD KEY pending_work(location_id,state,available_at,created_at,request_id);
 ALTER TABLE measurements DROP INDEX history_time, ADD KEY history_time(location_id,source,observed_at,id);
 GRANT SELECT,INSERT,UPDATE ON weatheragent.collector_status TO 'weatheragent'@'localhost';
 IF (SELECT COUNT(*) FROM measurements)<>measurement_count OR (SELECT COUNT(*) FROM backfill_requests)<>job_count
 OR (SELECT COUNT(*) FROM location)<>location_count OR (SELECT COUNT(*) FROM collector_status)<>location_count
 OR EXISTS(SELECT 1 FROM backfill_requests WHERE BINARY location_id<>BINARY @legacy_location_id) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Post-migration validation failed; restore backup';
 END IF;
 INSERT INTO schema_migrations(version) VALUES(2);
 DO RELEASE_LOCK('weatheragent:schema');
END//
DELIMITER ;
