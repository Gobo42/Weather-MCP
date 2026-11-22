USE weatheragent;
SET SESSION time_zone = '+00:00';
CREATE TABLE schema_migrations(version BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB;
INSERT INTO schema_migrations VALUES(1);
CREATE TABLE location(
 slot TINYINT PRIMARY KEY DEFAULT 1 CHECK(slot=1),
 id VARCHAR(32) COLLATE utf8mb4_bin NOT NULL UNIQUE,
 name VARCHAR(128) NOT NULL,
 latitude DOUBLE NOT NULL, longitude DOUBLE NOT NULL,
 timezone VARCHAR(64) NOT NULL,
 attribution VARCHAR(255) NOT NULL DEFAULT 'Weather data by Open-Meteo.com, CC BY 4.0'
) ENGINE=InnoDB;
CREATE TABLE measurements(
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 location_id VARCHAR(32) COLLATE utf8mb4_bin NOT NULL,
 source ENUM('current','history') NOT NULL,
 selection VARCHAR(32) COLLATE utf8mb4_bin NOT NULL,
 observed_at DATETIME(6) NOT NULL, retrieved_at DATETIME(6) NOT NULL,
 interval_seconds INT NOT NULL CHECK(interval_seconds>0),
 temperature_c DOUBLE NOT NULL, humidity_pct DOUBLE NOT NULL CHECK(humidity_pct BETWEEN 0 AND 100),
 grid_latitude DOUBLE NULL, grid_longitude DOUBLE NULL, grid_elevation DOUBLE NULL,
 UNIQUE KEY observation(location_id,source,selection,observed_at,interval_seconds),
 KEY history_time(source,observed_at,id),
 FOREIGN KEY(location_id) REFERENCES location(id)
) ENGINE=InnoDB;
CREATE TABLE collector_status(
 id TINYINT PRIMARY KEY CHECK(id=1),
 last_attempt_at DATETIME(6) NULL, last_success_at DATETIME(6) NULL,
 next_attempt_at DATETIME(6) NULL, provider_not_before DATETIME(6) NULL,
 failures INT NOT NULL DEFAULT 0,
 last_error_code VARCHAR(64) NOT NULL DEFAULT ''
) ENGINE=InnoDB;
INSERT INTO collector_status(id) VALUES(1);
CREATE TABLE backfill_requests(
 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY
 CHECK(request_id REGEXP '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
 start_date DATE NOT NULL CHECK(start_date>='1940-01-01'),
 end_date DATE NOT NULL,
 state ENUM('pending','running','completed','failed') NOT NULL DEFAULT 'pending',
 created_at DATETIME(6) NOT NULL DEFAULT UTC_TIMESTAMP(6),
 started_at DATETIME(6) NULL, completed_at DATETIME(6) NULL,
 available_at DATETIME(6) NOT NULL DEFAULT UTC_TIMESTAMP(6),
 next_date DATE NULL, attempts INT NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),
 claim_token CHAR(36) CHARACTER SET ascii NULL, lease_until DATETIME(6) NULL,
 checked_hours BIGINT UNSIGNED NOT NULL DEFAULT 0,
 inserted_rows BIGINT UNSIGNED NOT NULL DEFAULT 0,
 last_error_code VARCHAR(64) NOT NULL DEFAULT '',
 CHECK(end_date>=start_date AND DATEDIFF(end_date,start_date)<=365),
 KEY pending_work(state,available_at,created_at,request_id)
) ENGINE=InnoDB;
GRANT SELECT ON weatheragent.schema_migrations TO 'weatheragent'@'localhost';
GRANT SELECT,INSERT ON weatheragent.location TO 'weatheragent'@'localhost';
GRANT SELECT,INSERT ON weatheragent.measurements TO 'weatheragent'@'localhost';
GRANT SELECT,UPDATE ON weatheragent.collector_status TO 'weatheragent'@'localhost';
GRANT SELECT,UPDATE ON weatheragent.backfill_requests TO 'weatheragent'@'localhost';
GRANT SELECT ON weatheragent.schema_migrations TO 'weather_mcp'@'localhost';
GRANT SELECT ON weatheragent.location TO 'weather_mcp'@'localhost';
GRANT SELECT ON weatheragent.measurements TO 'weather_mcp'@'localhost';
GRANT SELECT ON weatheragent.collector_status TO 'weather_mcp'@'localhost';
GRANT SELECT,INSERT ON weatheragent.backfill_requests TO 'weather_mcp'@'localhost';
