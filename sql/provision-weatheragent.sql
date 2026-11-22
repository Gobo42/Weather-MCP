SET SESSION time_zone='+00:00', old_passwords=0;
DELIMITER //
BEGIN NOT ATOMIC
 IF COALESCE(GET_LOCK('weatheragent:schema',10),0)<>1 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Cannot acquire schema lock';
 END IF;
 IF @weatheragent_password IS NULL OR CHAR_LENGTH(@weatheragent_password)<32
 OR @weather_mcp_password IS NULL OR CHAR_LENGTH(@weather_mcp_password)<32
 OR BINARY @weatheragent_password=BINARY @weather_mcp_password THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Supply distinct passwords of at least 32 characters';
 END IF;
 IF EXISTS(SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='weatheragent')
 OR EXISTS(SELECT 1 FROM mysql.user WHERE User IN('weatheragent','weather_mcp')) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='Existing database or account; refusing replacement';
 END IF;
END//
DELIMITER ;
CREATE DATABASE weatheragent CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
SET @statement=CONCAT('CREATE USER ''weatheragent''@''localhost'' IDENTIFIED VIA mysql_native_password USING ',QUOTE(PASSWORD(@weatheragent_password)));
PREPARE weather_create FROM @statement;
EXECUTE weather_create;
DEALLOCATE PREPARE weather_create;
SET @statement=CONCAT('CREATE USER ''weather_mcp''@''localhost'' IDENTIFIED VIA mysql_native_password USING ',QUOTE(PASSWORD(@weather_mcp_password)));
PREPARE weather_create FROM @statement;
EXECUTE weather_create;
DEALLOCATE PREPARE weather_create;
SET @statement=NULL,@weatheragent_password=NULL,@weather_mcp_password=NULL;
SOURCE migrations/001_initial.sql;
SOURCE migrations/002_multiple_locations.sql;
DO RELEASE_LOCK('weatheragent:schema');
