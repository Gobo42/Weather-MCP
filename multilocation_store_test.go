package main

import (
	"context"
	"testing"
	"time"
)

func secondLocation(t *testing.T, store *Store) (Config, *Store) {
	t.Helper()
	config := store.config
	config.Version = 2
	latitude, longitude := 41.0, -74.0
	second := Location{ID: "east_" + newUUID()[:8], Name: "Synthetic east", Latitude: &latitude, Longitude: &longitude, Timezone: "America/New_York"}
	config.Locations = []Location{config.Location, second}
	config.Location = Location{}
	if err := store.RegisterLocations(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	return config, store.ForLocation(second)
}
func TestMultipleLocationStoreIsolation(t *testing.T) {
	store := databaseFixture(t)
	ctx := context.Background()
	config, east := secondLocation(t, store)
	reading := exampleReading()
	reading.TemperatureC = 17
	if err := store.PersistCurrent(ctx, reading, time.Now()); err != nil {
		t.Fatal(err)
	}
	reading.TemperatureC = 29
	if err := east.PersistCurrent(ctx, reading, time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM measurements WHERE observed_at=? AND location_id IN(?,?)", reading.ObservedAt, store.config.Location.ID, east.config.Location.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if err := east.UpdateStatus(ctx, Status{Failures: 3, LastErrorCode: "provider_http"}); err != nil {
		t.Fatal(err)
	}
	var failures int
	if err := store.db.QueryRow("SELECT failures FROM collector_status WHERE location_id=?", east.config.Location.ID).Scan(&failures); err != nil || failures != 3 {
		t.Fatal(failures, err)
	}
	copyConfig := config
	copyConfig.Locations = append([]Location{}, config.Locations...)
	changed := 42.0
	copyConfig.Locations[1].Latitude = &changed
	if err := store.RegisterLocations(ctx, copyConfig); err == nil {
		t.Fatal("relocated persisted location")
	}
	if err := store.Check(ctx, copyConfig); err == nil {
		t.Fatal("metadata check missed changed identity")
	}
}
func TestMultipleLocationQueueFence(t *testing.T) {
	store := databaseFixture(t)
	ctx := context.Background()
	_, east := secondLocation(t, store)
	key := submitTestJob(t, east, "2020-01-01", "2020-01-01")
	now := time.Now().UTC()
	job, err := east.Claim(ctx, now)
	if err != nil || job == nil || job.ID != key || job.LocationID != east.config.Location.ID {
		t.Fatal(job, err)
	}
	if foreign, err := store.Claim(ctx, now); err != nil || foreign != nil {
		t.Fatal("claimed another location", foreign, err)
	}
	if err := store.CommitChunk(ctx, *job, DateRange{}, HistoryResult{}, now); err == nil {
		t.Fatal("cross-location commit accepted")
	}
	if err := store.FailOrRetry(ctx, *job, providerFailure("invalid_response", false), now); err == nil {
		t.Fatal("cross-location claim changed state")
	}
	changed := *job
	changed.LocationID = store.config.Location.ID
	if err := store.FailOrRetry(ctx, changed, providerFailure("invalid_response", false), now); err == nil {
		t.Fatal("changed claim identity accepted")
	}
	if err := east.FailOrRetry(ctx, *job, providerFailure("invalid_response", false), now); err != nil {
		t.Fatal(err)
	}
	// An unconfigured location keeps its pending work; the home worker cannot consume it.
	key = submitTestJob(t, east, "2020-02-01", "2020-02-01")
	if job, err := store.Claim(ctx, now); err != nil || job != nil {
		t.Fatal("consumed removed-location job", err)
	}
	var state string
	if err := store.db.QueryRow("SELECT state FROM backfill_requests WHERE request_id=?", key).Scan(&state); err != nil || state != "pending" {
		t.Fatal(state, err)
	}
}
func TestRegistrationCheckIsReadOnly(t *testing.T) {
	store := databaseFixture(t)
	config := store.config
	config.Location.ID = "notregistered_" + newUUID()[:8]
	if err := store.Check(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	var count int
	store.db.QueryRow("SELECT COUNT(*) FROM location WHERE id=?", config.Location.ID).Scan(&count)
	if count != 0 {
		t.Fatal("check wrote a location")
	}
}
