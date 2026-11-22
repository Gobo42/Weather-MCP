package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMCPBackfillFlow(testingContext *testing.T) {
	key := os.Getenv("WEATHER_TEST_REQUEST_ID")
	if key == "" {
		testingContext.Skip("MCP end-to-end runner supplies request UUID")
	}
	store := databaseFixture(testingContext)
	provider := newProvider(store.config)
	provider.client = &http.Client{Transport: testTransport(func(request *http.Request) (*http.Response, error) {
		start, err := time.Parse(time.DateOnly, request.URL.Query().Get("start_date"))
		if err != nil {
			testingContext.Fatal(err)
		}
		end, err := time.Parse(time.DateOnly, request.URL.Query().Get("end_date"))
		if err != nil {
			testingContext.Fatal(err)
		}
		var timestamps []int64
		var temperatures, humidities []float64
		for observed := start; observed.Before(end.AddDate(0, 0, 1)); observed = observed.Add(time.Hour) {
			timestamps = append(timestamps, observed.Unix())
			temperatures = append(temperatures, 20)
			humidities = append(humidities, 50)
		}
		body, _ := json.Marshal(map[string]any{"hourly_units": map[string]string{"temperature_2m": "°C", "relative_humidity_2m": "%"}, "hourly": map[string]any{"time": timestamps, "temperature_2m": temperatures, "relative_humidity_2m": humidities}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	now := time.Now().UTC()
	collector := &Collector{Config: store.config, Store: store, Provider: provider, initialized: true, nextCurrent: now.Add(900 * time.Second), regularCurrent: now.Add(900 * time.Second)}
	for attempt := 0; attempt < 25; attempt++ {
		if _, err := collector.Step(context.Background(), time.Now().UTC()); err != nil {
			testingContext.Fatal(err)
		}
		var state string
		var count int
		store.db.QueryRow("SELECT state,checked_hours FROM backfill_requests WHERE request_id=?", key).Scan(&state, &count)
		if state == "completed" {
			if count != 24 {
				testingContext.Fatal(count)
			}
			return
		}
		if state == "failed" {
			testingContext.Fatal("job failed")
		}
	}
	testingContext.Fatal("MCP-created request never completed")
}

func TestQueueCrashRecovery(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	key := submitTestJob(testingContext, store, "2020-06-01", "2020-06-08")
	now := time.Now().UTC()
	job, err := store.Claim(ctx, now)
	if err != nil || job == nil || job.ID != key {
		testingContext.Fatal(job, err)
	}
	request, _ := parseDateRange(job.StartDate, job.EndDate, now)
	chunk, _ := nextChunk(request, job.NextDate)
	readings := []Reading{}
	for observed := chunk.StartUTC; observed.Before(chunk.EndUTC); observed = observed.Add(time.Hour) {
		reading := exampleReading()
		reading.Source = "history"
		reading.IntervalSeconds = 3600
		reading.ObservedAt = observed
		readings = append(readings, reading)
	}
	if err = store.CommitChunk(ctx, *job, chunk, HistoryResult{readings, true}, now); err != nil {
		testingContext.Fatal(err)
	}
	config := store.config
	store.Close()
	store, err = openStore(config)
	if err != nil {
		testingContext.Fatal(err)
	}
	defer store.Close()
	resumed, err := store.Claim(ctx, now.Add(time.Second))
	if err != nil || resumed == nil || resumed.NextDate != "2020-06-08" || resumed.CheckedHours != 168 || resumed.Attempts != 1 {
		testingContext.Fatal(resumed, err)
	}
	if err = store.CommitChunk(ctx, *job, chunk, HistoryResult{readings, true}, now); err == nil {
		testingContext.Fatal("abandoned token committed")
	}
	finalChunk, _ := nextChunk(request, resumed.NextDate)
	readings = nil
	for observed := finalChunk.StartUTC; observed.Before(finalChunk.EndUTC); observed = observed.Add(time.Hour) {
		reading := exampleReading()
		reading.Source = "history"
		reading.IntervalSeconds = 3600
		reading.ObservedAt = observed
		readings = append(readings, reading)
	}
	if err = store.CommitChunk(ctx, *resumed, finalChunk, HistoryResult{readings, true}, now.Add(time.Second)); err != nil {
		testingContext.Fatal(err)
	}
	var state string
	var count int
	store.db.QueryRow("SELECT state,checked_hours FROM backfill_requests WHERE request_id=?", key).Scan(&state, &count)
	if state != "completed" || count != 192 {
		testingContext.Fatal(state, count)
	}
}

func TestProviderRetryAfterLarge(testingContext *testing.T) {
	provider := providerFixture(testingContext, `{}`, 429)
	provider.client.Transport = testTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{fmt.Sprint(int64(10_000_000_000))}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_, err := provider.Current(context.Background())
	problem, ok := err.(*ProviderError)
	if !ok || problem.RetryAt.Year() < 2300 {
		testingContext.Fatal("large valid retry wrapped or ignored", err)
	}
}

func TestMCPSeedLocations(t *testing.T) {
	store := databaseFixture(t)
	config := store.config
	latitude, longitude := 41.0, -74.0
	east := Location{ID: "east", Name: "Synthetic east", Latitude: &latitude, Longitude: &longitude, Timezone: "America/New_York"}
	config.Version = 2
	config.Locations = []Location{config.Location, east}
	config.Location = Location{}
	if err := store.RegisterLocations(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	for i, location := range config.Locations {
		reading := exampleReading()
		reading.ObservedAt = time.Date(1940, 1, 1, 0, 0, 0, 0, time.UTC)
		reading.TemperatureC = float64(20 + i*10)
		if err := store.ForLocation(location).PersistCurrent(context.Background(), reading, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}
