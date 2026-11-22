package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitCurrent(t *testing.T, gate *ProviderGate) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mutex.Lock()
		n := gate.waitingCurrent
		gate.mutex.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("current request never queued")
}
func TestProviderGatePriorityCapacityAndCancellation(t *testing.T) {
	gate := newProviderGate(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := gate.Acquire(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.Acquire(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 2)
	releaseCurrent := make(chan struct{})
	go func() {
		release, err := gate.Acquire(ctx, false)
		if err == nil {
			order <- "history"
			release()
		}
	}()
	go func() {
		release, err := gate.Acquire(ctx, true)
		if err == nil {
			order <- "current"
			<-releaseCurrent
			release()
		}
	}()
	waitCurrent(t, gate)
	select {
	case <-order:
		t.Fatal("more than two active requests")
	default:
	}
	first()
	select {
	case value := <-order:
		if value != "current" {
			t.Fatal("history started ahead of waiting current poll")
		}
	case <-time.After(time.Second):
		t.Fatal("slot not released")
	}
	cancel()
	close(releaseCurrent)
	second()
}
func TestProviderGateSpacingAndCooldown(t *testing.T) {
	gate := newProviderGate(25 * time.Millisecond)
	ctx := context.Background()
	release, err := gate.Acquire(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	release()
	release, err = gate.Acquire(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if time.Since(start) < 24*time.Millisecond {
		t.Fatal("shared start throttle bypassed")
	}
	future := time.Now().Add(time.Hour)
	gate.DeferUntil(future)
	if _, err := gate.Acquire(ctx, true); err == nil {
		t.Fatal("cooldown ignored")
	} else {
		var problem *ProviderError
		if !errors.As(err, &problem) || !problem.Retryable || !problem.RetryAt.Equal(future) {
			t.Fatal("cooldown retry metadata lost", err)
		}
	}
	if !gate.NotBefore().Equal(future) {
		t.Fatal("cooldown changed")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := gate.Acquire(canceled, true); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}
func TestLocationWorkersOnceAndFailureIsolation(t *testing.T) {
	store := databaseFixture(t)
	config, _ := secondLocation(t, store)
	var output bytes.Buffer
	logger := newLogger(&output, "debug", false)
	workers, err := prepareCollectors(context.Background(), config, store, logger)
	if err != nil {
		t.Fatal(err)
	}
	if workers[0].Provider.client != workers[1].Provider.client {
		t.Fatal("HTTP client is not shared")
	}
	if workers[0].Provider.gate != workers[1].Provider.gate {
		t.Fatal("provider limits are not shared")
	}
	workers[0].Provider.gate.spacing = 0
	var mu sync.Mutex
	attempts := map[string]int{}
	for i, worker := range workers {
		index := i
		id := worker.Config.Location.ID
		worker.Provider.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			attempts[id]++
			mu.Unlock()
			if index == 0 {
				return nil, fmt.Errorf("secret-canary password=do-not-log URL=%s", r.URL)
			}
			body := fmt.Sprintf(`{"current_units":{"temperature_2m":"°C","relative_humidity_2m":"%%"},"current":{"time":%d,"interval":900,"temperature_2m":31,"relative_humidity_2m":45}}`, time.Now().Unix())
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runCollectors(ctx, workers, true); err == nil {
		t.Fatal("failed location was hidden")
	}
	if attempts[config.Locations[0].ID] != 1 || attempts[config.Locations[1].ID] != 1 {
		t.Fatal(attempts)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM measurements WHERE location_id=?", config.Locations[1].ID).Scan(&count); err != nil || count == 0 {
		t.Fatal("healthy location did not persist", count, err)
	}
	for _, bad := range []string{"secret-canary", "do-not-log", "https://", "latitude=", "longitude="} {
		if strings.Contains(output.String(), bad) {
			t.Fatalf("unsafe diagnostic field %q", bad)
		}
	}
	for _, event := range []string{"current_poll_started", "current_poll_completed", "provider_response", "current_poll_failed"} {
		if !strings.Contains(output.String(), event) {
			t.Errorf("missing diagnostic event %s", event)
		}
	}
}
func TestProviderCooldownSurvivesWorkersAndRestart(t *testing.T) {
	store := databaseFixture(t)
	config, _ := secondLocation(t, store)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	t.Cleanup(func() { store.db.Exec("UPDATE collector_status SET provider_not_before=NULL") })
	if err := store.PersistProviderCooldown(ctx, future); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, Status{}); err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := store.db.QueryRow("SELECT COALESCE(CAST(provider_not_before AS CHAR),'') FROM collector_status WHERE location_id=?", store.config.Location.ID).Scan(&saved); err != nil || saved == "" {
		t.Fatal("ordinary status update erased shared cooldown", err)
	}
	if _, err := store.db.Exec("UPDATE collector_status SET provider_not_before=? WHERE location_id=?", time.Now().Add(-time.Hour), store.config.Location.ID); err != nil {
		t.Fatal(err)
	}
	workers, err := prepareCollectors(ctx, config, store, newLogger(io.Discard, "info", false))
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers {
		if !worker.Provider.gate.NotBefore().Equal(future) {
			t.Fatal("persisted global cooldown lost")
		}
	}
	once, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := runCollectors(once, workers, true); err != nil {
		t.Fatal(err)
	}
	if once.Err() != nil {
		t.Fatal("once waited for a deferred retry")
	}
}
func TestUnknownProviderErrorIsSanitized(t *testing.T) {
	if errorCode(&ProviderError{Code: "secret-canary"}) != "operation_failed" {
		t.Fatal("unrecognized provider error leaked")
	}
}

func TestSharedRetryAfterPersistsBeforeRestart(t *testing.T) {
	store := databaseFixture(t)
	config, _ := secondLocation(t, store)
	ctx := context.Background()
	t.Cleanup(func() { store.db.Exec("UPDATE collector_status SET provider_not_before=NULL") })
	workers, err := prepareCollectors(ctx, config, store, newLogger(io.Discard, "debug", false))
	if err != nil {
		t.Fatal(err)
	}
	workers[0].Provider.gate.spacing = 0
	workers[0].Provider.client = providerFixture(t, "{}", 429).client
	if _, err := workers[0].Provider.Current(ctx); err == nil {
		t.Fatal("429 hidden")
	}
	saved, err := store.ProviderCooldown(ctx)
	if err != nil || time.Until(saved) < 119*time.Second {
		t.Fatal("Retry-After not durable", saved, err)
	}
	called := false
	workers[1].Provider.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) { called = true; return nil, errors.New("must not start") })}
	if _, err := workers[1].Provider.Current(ctx); err == nil || called {
		t.Fatal("second location bypassed cooldown")
	}
	restarted, err := prepareCollectors(ctx, config, store, newLogger(io.Discard, "info", false))
	if err != nil || !restarted[0].Provider.gate.NotBefore().Equal(saved) {
		t.Fatal("restart lost shared Retry-After", err)
	}
}

func TestHistoryWorkerProcessingAndFailures(t *testing.T) {
	for _, scenario := range []string{"success", "dates", "cursor", "provider", "response"} {
		t.Run(scenario, func(t *testing.T) {
			store := databaseFixture(t)
			_, east := secondLocation(t, store)
			ctx := context.Background()
			now := time.Now().UTC()
			start, end := "2020-08-01", "2020-08-01"
			if scenario == "dates" {
				start = now.AddDate(0, 0, 3).Format(time.DateOnly)
				end = start
			}
			key := submitTestJob(t, east, start, end)
			if scenario == "cursor" {
				if _, err := east.db.Exec("UPDATE backfill_requests SET next_date='1940-01-01' WHERE request_id=?", key); err != nil {
					t.Fatal(err)
				}
			}
			now = time.Now().UTC()
			t.Cleanup(func() {
				east.db.Exec("UPDATE collector_status SET provider_not_before=NULL WHERE location_id=?", east.config.Location.ID)
			})
			chunk, _ := calendarRange(start, end, east.config.Location.Timezone)
			var stamps []int64
			var temps, hums []float64
			for at := chunk.StartUTC; at.Before(chunk.EndUTC); at = at.Add(time.Hour) {
				stamps = append(stamps, at.Unix())
				temps = append(temps, 20)
				hums = append(hums, 50)
			}
			raw, _ := json.Marshal(map[string]any{"hourly_units": map[string]string{"temperature_2m": "°C", "relative_humidity_2m": "%"}, "hourly": map[string]any{"time": stamps, "temperature_2m": temps, "relative_humidity_2m": hums}})
			status := 200
			if scenario == "provider" {
				status = 503
			}
			if scenario == "response" {
				raw = []byte("{}")
			}
			var logs bytes.Buffer
			logger := newLogger(&logs, "debug", false).With("location_id", east.config.Location.ID)
			provider := providerFixture(t, string(raw), status)
			provider.logger = logger
			east.logger = logger
			worker := &Collector{Config: east.config, Store: east, Provider: provider, Logger: logger, initialized: true, nextCurrent: now.Add(900 * time.Second), regularCurrent: now.Add(900 * time.Second)}
			_, err := worker.Step(ctx, now)
			var state string
			var hours int
			var code string
			if e := east.db.QueryRow("SELECT state,checked_hours,last_error_code FROM backfill_requests WHERE request_id=?", key).Scan(&state, &hours, &code); e != nil {
				t.Fatal(e)
			}
			if scenario == "success" {
				if err != nil || state != "completed" || hours != 24 || !strings.Contains(logs.String(), "backfill_persisted") {
					t.Fatal(state, hours, err, logs.String())
				}
			} else {
				if err == nil {
					t.Fatal("failed history hidden")
				}
				expected := "failed"
				if scenario == "provider" {
					expected = "pending"
				}
				if state != expected || code == "" || !strings.Contains(logs.String(), "backfill_failed") {
					t.Fatal(state, code, logs.String())
				}
			}
		})
	}
}
func TestLocationWorkersCancelAllRequests(t *testing.T) {
	store := databaseFixture(t)
	config, _ := secondLocation(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workers, err := prepareCollectors(ctx, config, store, newLogger(io.Discard, "info", false))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	for _, worker := range workers {
		worker.Provider.client = client
	}
	done := make(chan error, 1)
	go func() { done <- runCollectors(ctx, workers, false) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker shutdown blocked")
	}
}
