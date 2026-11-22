package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectorSchedule(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	now := time.Now().UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"current_units":{"temperature_2m":"°C","relative_humidity_2m":"%%"},"current":{"time":%d,"interval":900,"temperature_2m":22,"relative_humidity_2m":44}}`, now.Unix())
	collector := &Collector{Config: store.config, Store: store, Provider: providerFixture(testingContext, body, 200)}
	if _, err := collector.Step(context.Background(), now); err != nil {
		testingContext.Fatal(err)
	}
	if !collector.nextCurrent.Equal(now.Add(900 * time.Second)) {
		testingContext.Fatal(collector.nextCurrent)
	}
	var count int
	store.db.QueryRow("SELECT COUNT(*) FROM measurements WHERE source='current' AND observed_at=?", now).Scan(&count)
	if count != 1 {
		testingContext.Fatal(count)
	}
	if _, err := collector.Step(context.Background(), now.Add(time.Second)); err != nil {
		testingContext.Fatal(err)
	}
	if !collector.nextCurrent.Equal(now.Add(900 * time.Second)) {
		testingContext.Fatal("polled early")
	}
}
func TestCollectorThrottle(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	now := time.Now().UTC()
	collector := &Collector{Config: store.config, Store: store, Provider: providerFixture(testingContext, `{}`, 429)}
	if _, err := collector.Step(context.Background(), now); err == nil {
		testingContext.Fatal("throttle hidden")
	}
	if !collector.nextCurrent.After(now.Add(119 * time.Second)) {
		testingContext.Fatal("retry not deferred")
	}
	store.db.Exec("UPDATE collector_status SET provider_not_before=NULL WHERE location_id=?", store.config.Location.ID)
}
func TestCollectorDatabaseOutage(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	store.db.Close()
	collector := &Collector{Config: store.config, Store: store, Provider: providerFixture(testingContext, `{}`, 200)}
	if _, err := collector.Step(context.Background(), time.Now()); err == nil {
		testingContext.Fatal("database outage hidden")
	}
}
func TestCommand(testingContext *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"check", "--once"}, {"collect", "--bad"}, {"check", "--config", "/missing"}} {
		if err := command(context.Background(), args); err == nil {
			testingContext.Fatal(args)
		}
	}
	if err := command(context.Background(), []string{"--help"}); err != nil {
		testingContext.Fatal(err)
	}
	filename := os.Getenv("WEATHER_TEST_DATABASE_CONFIG")
	if filename != "" {
		if err := command(context.Background(), []string{"check", "--config", filename}); err != nil {
			testingContext.Fatal(err)
		}
	}
	root := testingContext.TempDir()
	os.Chmod(root, 0700)
	path := filepath.Join(root, "lock")
	release, err := collectorLock(path)
	if err != nil {
		testingContext.Fatal(err)
	}
	defer release()
	if second, err := collectorLock(path); err == nil {
		second()
		testingContext.Fatal("duplicate collector lock")
	}
}
