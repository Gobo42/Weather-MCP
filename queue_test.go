package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"github.com/go-sql-driver/mysql"
	"testing"
	"time"
)

func submitTestJob(testingContext *testing.T, store *Store, start, end string) string {
	testingContext.Helper()
	settings := mysql.NewConfig()
	settings.Net = "unix"
	settings.Addr = store.config.Database.Socket
	settings.DBName = store.config.Database.Database
	settings.User = "weather_mcp"
	settings.Passwd = "weather-fixture-mcp-password-00000000"
	database, err := sql.Open("mysql", settings.FormatDSN())
	if err != nil {
		testingContext.Fatal(err)
	}
	defer database.Close()
	key := newUUID()
	if _, err = database.Exec("INSERT INTO backfill_requests(request_id,location_id,start_date,end_date) VALUES(?,?,?,?)", key, store.config.Location.ID, start, end); err != nil {
		testingContext.Fatal(err)
	}
	return key
}
func TestQueueClaim(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	submitTestJob(testingContext, store, "2020-01-01", "2020-01-01")
	job, err := store.Claim(ctx, time.Now())
	if err != nil || job == nil || job.Attempts != 1 {
		testingContext.Fatal(job, err)
	}
	if err = store.FailOrRetry(ctx, *job, providerFailure("invalid_response", false), time.Now()); err != nil {
		testingContext.Fatal(err)
	}
	var state string
	store.db.QueryRow("SELECT state FROM backfill_requests WHERE request_id=?", job.ID).Scan(&state)
	if state != "failed" {
		testingContext.Fatal(state)
	}
}
func TestQueueLeaseFence(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	submitTestJob(testingContext, store, "2020-02-01", "2020-02-01")
	now := time.Now().UTC()
	job, err := store.Claim(ctx, now)
	if err != nil || job == nil {
		testingContext.Fatal(job, err)
	}
	successor, err := store.Claim(ctx, now.Add(6*time.Minute))
	if err != nil || successor == nil || successor.ID != job.ID || successor.Token == job.Token || successor.Attempts != 2 {
		testingContext.Fatal(successor, err)
	}
	if err = store.FailOrRetry(ctx, *job, providerFailure("invalid_response", false), now.Add(6*time.Minute)); err == nil {
		testingContext.Fatal("stale worker changed state")
	}
	store.FailOrRetry(ctx, *successor, providerFailure("invalid_response", false), now.Add(6*time.Minute))
}
func TestQueuePartialHistory(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	submitTestJob(testingContext, store, "2020-03-01", "2020-03-01")
	job, err := store.Claim(ctx, time.Now())
	if err != nil || job == nil {
		testingContext.Fatal(job, err)
	}
	request, _ := parseDateRange(job.StartDate, job.EndDate, time.Now())
	chunk, _ := nextChunk(request, job.NextDate)
	reading := exampleReading()
	reading.Source = "history"
	reading.IntervalSeconds = 3600
	reading.ObservedAt = chunk.StartUTC
	if err = store.CommitChunk(ctx, *job, chunk, HistoryResult{Readings: []Reading{reading}, Complete: false}, time.Now()); err != nil {
		testingContext.Fatal(err)
	}
	var state, code string
	store.db.QueryRow("SELECT state,last_error_code FROM backfill_requests WHERE request_id=?", job.ID).Scan(&state, &code)
	if state != "failed" || code != "data_unavailable" {
		testingContext.Fatal(state, code)
	}
}
func TestQueueRetryBudget(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	submitTestJob(testingContext, store, "2020-04-01", "2020-04-01")
	now := time.Now().UTC()
	var key string
	for attempt := 1; attempt <= 5; attempt++ {
		job, err := store.Claim(ctx, now)
		if err != nil || job == nil || job.Attempts != attempt {
			testingContext.Fatal(attempt, job, err)
		}
		key = job.ID
		if err = store.FailOrRetry(ctx, *job, providerFailure("provider_transport", true), now); err != nil {
			testingContext.Fatal(err)
		}
		if attempt < 5 {
			if early, err := store.Claim(ctx, now); err != nil || early != nil {
				testingContext.Fatal("retried early", early, err)
			}
		}
		now = now.Add(time.Duration(60*(1<<(attempt-1))) * time.Second)
	}
	var state string
	store.db.QueryRow("SELECT state FROM backfill_requests WHERE request_id=?", key).Scan(&state)
	if state != "failed" {
		testingContext.Fatal(state)
	}
}

func TestQueueDatabaseRetry(testingContext *testing.T) {
	store := databaseFixture(testingContext)
	ctx := context.Background()
	key := submitTestJob(testingContext, store, "2020-07-01", "2020-07-01")
	now := time.Now().UTC()
	job, err := store.Claim(ctx, now)
	if err != nil || job == nil || job.ID != key {
		testingContext.Fatal(job, err)
	}
	if err = store.FailOrRetry(ctx, *job, driver.ErrBadConn, now); err != nil {
		testingContext.Fatal(err)
	}
	var state string
	store.db.QueryRow("SELECT state FROM backfill_requests WHERE request_id=?", key).Scan(&state)
	if state != "pending" {
		testingContext.Fatal("transient DB disconnect was not retryable", state)
	}
	resumed, err := store.Claim(ctx, now.Add(time.Minute))
	if err != nil || resumed == nil {
		testingContext.Fatal(err)
	}
	store.FailOrRetry(ctx, *resumed, providerFailure("invalid_response", false), now.Add(time.Minute))
}
