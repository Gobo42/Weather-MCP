package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func databaseFixture(testingContext *testing.T) *Store {
	testingContext.Helper()
	filename := os.Getenv("WEATHER_TEST_DATABASE_CONFIG")
	if filename == "" {
		testingContext.Skip("isolated MariaDB fixture not selected; mandatory acceptance runner enforces it")
	}
	config, err := loadConfig(filename)
	if err != nil {
		testingContext.Fatal(err)
	}
	store, err := openStore(config)
	if err != nil {
		testingContext.Fatal(err)
	}
	if err := store.RegisterLocations(context.Background(), config); err != nil {
		testingContext.Fatal(err)
	}
	testingContext.Cleanup(func() { store.Close() })
	return store
}
func exampleReading() Reading {
	return Reading{Source: "current", Selection: "best_match", ObservedAt: time.Now().UTC().Truncate(time.Microsecond), RetrievedAt: time.Now().UTC().Truncate(time.Microsecond), IntervalSeconds: 900, TemperatureC: 20, HumidityPct: 50}
}
func TestStoreSchema(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	if err := store.Check(context.Background(), store.config); err != nil {
		testingContext.Fatal(err)
	}
}
func TestStoreLocation(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	if err := store.PersistCurrent(ctx, exampleReading(), time.Now()); err != nil {
		testingContext.Fatal(err)
	}
	config := store.config
	changed := 41.0
	config.Location.Latitude = &changed
	if err := store.Check(ctx, config); err == nil {
		testingContext.Fatal("location relabeled")
	}
}
func TestStoreObservation(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	reading := exampleReading()
	reading.ObservedAt = time.Date(1940, 1, 1, 0, 0, int(time.Now().UnixNano()%50), int(time.Now().UnixNano()%1000000)*1000, time.UTC)
	if err := store.PersistCurrent(ctx, reading, time.Now()); err != nil {
		testingContext.Fatal(err)
	}
	reading.RetrievedAt = reading.RetrievedAt.Add(time.Second)
	if err := store.PersistCurrent(ctx, reading, time.Now()); err != nil {
		testingContext.Fatal("repeat failed", err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM measurements WHERE observed_at=? AND source='current'", reading.ObservedAt).Scan(&count); err != nil || count != 1 {
		testingContext.Fatal(count, err)
	}
	reading.TemperatureC = 21
	if err := store.PersistCurrent(ctx, reading, time.Now()); err == nil || err.Error() != "observation_conflict" {
		testingContext.Fatal("overwrote observation", err)
	}
	var temperature float64
	store.db.QueryRow("SELECT temperature_c FROM measurements WHERE observed_at=? AND source='current'", reading.ObservedAt).Scan(&temperature)
	if temperature != 20 {
		testingContext.Fatal(temperature)
	}
}
func TestStoreGrants(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	if _, err := store.db.Exec("DELETE FROM measurements"); err == nil {
		testingContext.Fatal("writer can delete")
	}
	if _, err := store.db.Exec("CREATE TABLE should_not_exist(id INT)"); err == nil {
		testingContext.Fatal("writer can modify schema")
	}
	if _, err := store.db.Exec("INSERT INTO backfill_requests(request_id,start_date,end_date) VALUES(UUID(),'2020-01-01','2020-01-01')"); err == nil {
		testingContext.Fatal("worker can submit commands")
	}
}
func TestStoreCurrentRollback(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.PersistCurrent(ctx, exampleReading(), time.Now()); err == nil {
		testingContext.Fatal("canceled persistence succeeded")
	}
}
