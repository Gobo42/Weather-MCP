package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"github.com/go-sql-driver/mysql"
	"log/slog"
	"math"
	"net"
	"reflect"
	"strings"
	"time"
)

type Store struct {
	db     *sql.DB
	config Config
	logger *slog.Logger
}
type Status struct {
	LastAttemptAt, LastSuccessAt, NextAttemptAt, ProviderNotBefore *time.Time
	Failures                                                       int
	LastErrorCode                                                  string
}

func openStore(config Config) (*Store, error) {
	settings := mysql.NewConfig()
	settings.Net = "unix"
	settings.Addr = config.Database.Socket
	settings.DBName = config.Database.Database
	settings.User = config.Database.Username
	settings.Passwd = config.Database.Password
	settings.ParseTime = true
	settings.Loc = time.UTC
	settings.Timeout = 5 * time.Second
	settings.ReadTimeout = 5 * time.Second
	settings.WriteTimeout = 5 * time.Second
	settings.Params = map[string]string{"time_zone": "'+00:00'", "sql_mode": "'STRICT_ALL_TABLES,NO_ZERO_DATE,NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"}
	database, err := sql.Open("mysql", settings.FormatDSN())
	if err != nil {
		return nil, failure("database_unavailable")
	}
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(2)
	if config.Location.ID == "" {
		config.Location = config.locations()[0]
	}
	return &Store{db: database, config: config, logger: slog.Default()}, nil
}
func (store *Store) Close() error { return store.db.Close() }
func (store *Store) Check(ctx context.Context, config Config) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var account string
	var role sql.NullString
	if err := store.db.QueryRowContext(ctx, "SELECT CURRENT_USER(),CURRENT_ROLE()").Scan(&account, &role); err != nil || account != config.Database.Username+"@localhost" || role.Valid && role.String != "NONE" {
		return failure("database_identity_invalid")
	}
	expected := map[string]bool{}
	for table, privileges := range map[string][]string{"schema_migrations": {"SELECT"}, "location": {"SELECT", "INSERT"}, "measurements": {"SELECT", "INSERT"}, "collector_status": {"SELECT", "INSERT", "UPDATE"}, "backfill_requests": {"SELECT", "UPDATE"}} {
		for _, privilege := range privileges {
			expected[table+":"+privilege] = true
		}
	}
	rows, err := store.db.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return failure("grants_invalid")
	}
	actual := map[string]bool{}
	for rows.Next() {
		var grant string
		if rows.Scan(&grant) != nil {
			rows.Close()
			return failure("grants_invalid")
		}
		parts := strings.SplitN(strings.TrimPrefix(grant, "GRANT "), " ON ", 2)
		if len(parts) != 2 || strings.Contains(grant, "WITH GRANT OPTION") {
			rows.Close()
			return failure("grants_invalid")
		}
		target := strings.SplitN(parts[1], " TO ", 2)[0]
		if parts[0] == "USAGE" && target == "*.*" {
			continue
		}
		prefix := "`" + config.Database.Database + "`.`"
		if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "`") {
			rows.Close()
			return failure("grants_invalid")
		}
		table := strings.TrimSuffix(strings.TrimPrefix(target, prefix), "`")
		for _, privilege := range strings.Split(parts[0], ", ") {
			actual[table+":"+privilege] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !reflect.DeepEqual(actual, expected) {
		return failure("grants_invalid")
	}
	var version, count int
	if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations").Scan(&count, &version); err != nil || count != 2 || version != 2 {
		return failure("schema_invalid")
	}
	if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME IN('schema_migrations','location','measurements','collector_status','backfill_requests') AND ENGINE='InnoDB'").Scan(&count); err != nil || count != 5 {
		return failure("schema_invalid")
	}
	for table, columns := range map[string]string{"location": "id,name,latitude,longitude,timezone,attribution", "measurements": "id,location_id,source,selection,observed_at,retrieved_at,interval_seconds,temperature_c,humidity_pct,grid_latitude,grid_longitude,grid_elevation", "collector_status": "location_id,last_attempt_at,last_success_at,next_attempt_at,provider_not_before,failures,last_error_code", "backfill_requests": "request_id,location_id,start_date,end_date,state,created_at,started_at,completed_at,available_at,next_date,attempts,claim_token,lease_until,checked_hours,inserted_rows,last_error_code"} {
		result, err := store.db.QueryContext(ctx, "SELECT "+columns+" FROM "+table+" LIMIT 0")
		if err != nil {
			return failure("schema_invalid")
		}
		result.Close()
	}
	return checkLocation(ctx, store.db, config)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkLocation(ctx context.Context, query rowQuerier, config Config) error {
	for _, location := range config.locations() {
		var latitude, longitude float64
		var zone string
		err := query.QueryRowContext(ctx, "SELECT latitude,longitude,timezone FROM location WHERE id=?", location.ID).Scan(&latitude, &longitude, &zone)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return failure("database_unavailable")
		}
		if latitude != *location.Latitude || longitude != *location.Longitude || zone != location.Timezone {
			return failure("location_mismatch")
		}
	}
	return nil
}
func (store *Store) ForLocation(location Location) *Store {
	config := store.config
	config.Version = 1
	config.Location = location
	config.Locations = nil
	return &Store{db: store.db, config: config, logger: slog.Default().With("location_id", location.ID)}
}
func (store *Store) syncLocation(ctx context.Context, transaction *sql.Tx) error {
	location := store.config.Location
	_, err := transaction.ExecContext(ctx, "INSERT INTO location(id,name,latitude,longitude,timezone) SELECT ?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM location WHERE id=?)", location.ID, location.Name, *location.Latitude, *location.Longitude, location.Timezone, location.ID)
	if err != nil {
		return failure("database_unavailable")
	}
	if err = checkLocation(ctx, transaction, store.config); err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, "INSERT INTO collector_status(location_id) SELECT ? WHERE NOT EXISTS(SELECT 1 FROM collector_status WHERE location_id=?)", location.ID, location.ID)
	if err != nil {
		return failure("database_unavailable")
	}
	return nil
}
func (store *Store) RegisterLocations(ctx context.Context, config Config) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return failure("database_unavailable")
	}
	defer tx.Rollback()
	for _, location := range config.locations() {
		if err = store.ForLocation(location).syncLocation(ctx, tx); err != nil {
			return err
		}
	}
	return commit(tx)
}
func validReading(reading Reading) bool {
	return (reading.Source == "current" || reading.Source == "history") && reading.Selection == "best_match" && reading.IntervalSeconds > 0 && (reading.Source != "history" || reading.IntervalSeconds == 3600) && !reading.ObservedAt.IsZero() && reading.ObservedAt.Year() >= 1940 && reading.ObservedAt.Year() < 9999 && !reading.RetrievedAt.IsZero() && !math.IsNaN(reading.TemperatureC) && !math.IsInf(reading.TemperatureC, 0) && !math.IsNaN(reading.HumidityPct) && !math.IsInf(reading.HumidityPct, 0) && reading.HumidityPct >= 0 && reading.HumidityPct <= 100
}
func equalOptional(value *float64, stored sql.NullFloat64) bool {
	return value == nil && !stored.Valid || value != nil && stored.Valid && *value == stored.Float64
}
func (store *Store) insertReading(ctx context.Context, transaction *sql.Tx, reading Reading) (int64, error) {
	if !validReading(reading) {
		return 0, failure("invalid_reading")
	}
	var temperature, humidity float64
	var latitude, longitude, elevation sql.NullFloat64
	err := transaction.QueryRowContext(ctx, "SELECT temperature_c,humidity_pct,grid_latitude,grid_longitude,grid_elevation FROM measurements WHERE location_id=? AND source=? AND selection=? AND observed_at=? AND interval_seconds=? FOR UPDATE", store.config.Location.ID, reading.Source, reading.Selection, reading.ObservedAt, reading.IntervalSeconds).Scan(&temperature, &humidity, &latitude, &longitude, &elevation)
	if err == nil {
		if temperature != reading.TemperatureC || humidity != reading.HumidityPct || !equalOptional(reading.GridLatitude, latitude) || !equalOptional(reading.GridLongitude, longitude) || !equalOptional(reading.GridElevation, elevation) {
			return 0, failure("observation_conflict")
		}
		return 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	_, err = transaction.ExecContext(ctx, "INSERT INTO measurements(location_id,source,selection,observed_at,retrieved_at,interval_seconds,temperature_c,humidity_pct,grid_latitude,grid_longitude,grid_elevation) VALUES(?,?,?,?,?,?,?,?,?,?,?)", store.config.Location.ID, reading.Source, reading.Selection, reading.ObservedAt.UTC(), reading.RetrievedAt.UTC(), reading.IntervalSeconds, reading.TemperatureC, reading.HumidityPct, reading.GridLatitude, reading.GridLongitude, reading.GridElevation)
	return 1, err
}
func commit(transaction *sql.Tx) error {
	if transaction.Commit() != nil {
		return failure("write_outcome_unknown")
	}
	return nil
}
func (store *Store) PersistCurrent(ctx context.Context, reading Reading, now time.Time) error {
	started := time.Now()
	if reading.Source != "current" {
		return failure("invalid_reading")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return failure("database_unavailable")
	}
	defer transaction.Rollback()
	if err = store.syncLocation(ctx, transaction); err != nil {
		return err
	}
	inserted, err := store.insertReading(ctx, transaction, reading)
	if err != nil {
		return err
	}
	if _, err = transaction.ExecContext(ctx, "UPDATE collector_status SET last_attempt_at=?,last_success_at=?,failures=0,last_error_code='' WHERE location_id=?", now.UTC(), now.UTC(), store.config.Location.ID); err != nil {
		return err
	}
	if err = commit(transaction); err != nil {
		return err
	}
	logger := store.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("current_persisted", "duration_ms", time.Since(started).Milliseconds(), "inserted_rows", inserted, "replayed_rows", 1-inserted)
	return nil
}
func (store *Store) UpdateStatus(ctx context.Context, status Status) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := store.db.ExecContext(ctx, "UPDATE collector_status SET last_attempt_at=COALESCE(?,last_attempt_at),last_success_at=COALESCE(?,last_success_at),next_attempt_at=?,provider_not_before=GREATEST(COALESCE(provider_not_before,?),COALESCE(?,provider_not_before)),failures=?,last_error_code=? WHERE location_id=?", status.LastAttemptAt, status.LastSuccessAt, status.NextAttemptAt, status.ProviderNotBefore, status.ProviderNotBefore, status.Failures, status.LastErrorCode, store.config.Location.ID)
	if err != nil {
		return failure("database_unavailable")
	}
	return nil
}
func retryableDatabase(err error) bool {
	var databaseError *mysql.MySQLError
	var networkError net.Error
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) || errors.As(err, &networkError) || errors.As(err, &databaseError) && (databaseError.Number == 1205 || databaseError.Number == 1213) || err != nil && (err.Error() == "database_unavailable" || err.Error() == "write_outcome_unknown")
}

func (store *Store) PersistProviderCooldown(ctx context.Context, deadline time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := store.db.ExecContext(ctx, "UPDATE collector_status SET provider_not_before=GREATEST(COALESCE(provider_not_before,?),?)", deadline.UTC(), deadline.UTC())
	if err != nil {
		return failure("database_unavailable")
	}
	return nil
}
func (store *Store) ProviderCooldown(ctx context.Context) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var deadline sql.NullTime
	if err := store.db.QueryRowContext(ctx, "SELECT MAX(provider_not_before) FROM collector_status").Scan(&deadline); err != nil {
		return time.Time{}, failure("database_unavailable")
	}
	return deadline.Time, nil
}
