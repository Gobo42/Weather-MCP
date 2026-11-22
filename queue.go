package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type Job struct {
	ID, LocationID, StartDate, EndDate, NextDate, Token string
	Attempts                                            int
	LeaseUntil                                          time.Time
	CheckedHours, InsertedRows                          int64
}

func newUUID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		panic("random_unavailable")
	}
	data[6] = (data[6] & 15) | 64
	data[8] = (data[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[:4], data[4:6], data[6:8], data[8:10], data[10:])
}
func (store *Store) Claim(ctx context.Context, now time.Time) (*Job, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	now = now.UTC()
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, failure("database_unavailable")
	}
	defer transaction.Rollback()
	if _, err = transaction.ExecContext(ctx, "UPDATE backfill_requests SET state='failed',completed_at=?,last_error_code='retry_exhausted',claim_token=NULL,lease_until=NULL WHERE location_id=? AND state='running' AND lease_until<=? AND attempts>=5", now, store.config.Location.ID, now); err != nil {
		return nil, err
	}
	job := &Job{}
	err = transaction.QueryRowContext(ctx, "SELECT request_id,location_id,DATE_FORMAT(start_date,'%Y-%m-%d'),DATE_FORMAT(end_date,'%Y-%m-%d'),DATE_FORMAT(COALESCE(next_date,start_date),'%Y-%m-%d'),attempts,checked_hours,inserted_rows FROM backfill_requests WHERE location_id=? AND ((state='pending' AND available_at<=?) OR (state='running' AND lease_until<=? AND attempts<5)) ORDER BY created_at,request_id LIMIT 1 FOR UPDATE SKIP LOCKED", store.config.Location.ID, now, now).Scan(&job.ID, &job.LocationID, &job.StartDate, &job.EndDate, &job.NextDate, &job.Attempts, &job.CheckedHours, &job.InsertedRows)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, commit(transaction)
	}
	if err != nil {
		return nil, err
	}
	job.Token = newUUID()
	job.LeaseUntil = now.Add(5 * time.Minute)
	job.Attempts++
	_, err = transaction.ExecContext(ctx, "UPDATE backfill_requests SET state='running',started_at=COALESCE(started_at,?),next_date=COALESCE(next_date,start_date),claim_token=?,lease_until=?,attempts=? WHERE request_id=?", now, job.Token, job.LeaseUntil, job.Attempts, job.ID)
	if err != nil {
		return nil, err
	}
	if err = commit(transaction); err != nil {
		return nil, err
	}
	return job, nil
}
func lockClaim(ctx context.Context, transaction *sql.Tx, job Job, now time.Time) error {
	var token string
	var expiry time.Time
	if err := transaction.QueryRowContext(ctx, "SELECT claim_token,lease_until FROM backfill_requests WHERE request_id=? AND location_id=? AND state='running' FOR UPDATE", job.ID, job.LocationID).Scan(&token, &expiry); err != nil || token != job.Token || !expiry.After(now) {
		return failure("lease_lost")
	}
	return nil
}
func (store *Store) CommitChunk(ctx context.Context, job Job, chunk DateRange, result HistoryResult, now time.Time) error {
	if job.LocationID != store.config.Location.ID {
		return failure("lease_lost")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return failure("database_unavailable")
	}
	defer transaction.Rollback()
	if err = lockClaim(ctx, transaction, job, now.UTC()); err != nil {
		return err
	}
	request, err := parseDateRange(job.StartDate, job.EndDate, now, store.config.Location.Timezone)
	if err != nil {
		return err
	}
	expected, err := nextChunk(request, job.NextDate)
	if err != nil || chunk != expected {
		return failure("invalid_cursor")
	}
	if result.Complete && len(result.Readings) != hourlyCount(chunk) {
		return failure("invalid_response")
	}
	if err = store.syncLocation(ctx, transaction); err != nil {
		return err
	}
	var inserted int64
	var previous time.Time
	for _, reading := range result.Readings {
		if reading.Source != "history" || reading.ObservedAt.Before(chunk.StartUTC) || !reading.ObservedAt.Before(chunk.EndUTC) || !previous.IsZero() && !reading.ObservedAt.After(previous) || reading.ObservedAt.Unix()%3600 != 0 {
			return failure("invalid_response")
		}
		count, err := store.insertReading(ctx, transaction, reading)
		if err != nil {
			return err
		}
		inserted += count
		previous = reading.ObservedAt
	}
	state, code := "pending", ""
	var completed any
	zone, _ := time.LoadLocation(store.config.Location.Timezone)
	cursor := chunk.EndUTC.In(zone).Format(time.DateOnly)
	attempts := 0
	if !result.Complete {
		state = "failed"
		code = "data_unavailable"
		completed = now.UTC()
		cursor = job.NextDate
		attempts = job.Attempts
	} else if chunk.EndDate == job.EndDate {
		state = "completed"
		completed = now.UTC()
	}
	outcome, err := transaction.ExecContext(ctx, "UPDATE backfill_requests SET state=?,completed_at=?,next_date=?,available_at=?,attempts=?,claim_token=NULL,lease_until=NULL,checked_hours=checked_hours+?,inserted_rows=inserted_rows+?,last_error_code=? WHERE request_id=? AND claim_token=? AND lease_until>?", state, completed, cursor, now.UTC(), attempts, len(result.Readings), inserted, code, job.ID, job.Token, now.UTC().Add(time.Since(started)))
	if err != nil {
		return err
	}
	affected, err := outcome.RowsAffected()
	if err != nil || affected != 1 {
		return failure("lease_lost")
	}
	if err = commit(transaction); err != nil {
		return err
	}
	logger := store.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("backfill_persisted", "request_id", job.ID, "duration_ms", time.Since(started).Milliseconds(), "inserted_rows", inserted, "replayed_rows", int64(len(result.Readings))-inserted, "state", state)
	return nil
}
func (store *Store) FailOrRetry(ctx context.Context, job Job, cause error, now time.Time) error {
	if job.LocationID != store.config.Location.ID {
		return failure("lease_lost")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return failure("database_unavailable")
	}
	defer transaction.Rollback()
	if err = lockClaim(ctx, transaction, job, now.UTC()); err != nil {
		return err
	}
	state := "failed"
	var completed any = now.UTC()
	available := now.UTC()
	var problem *ProviderError
	providerRetry := errors.As(cause, &problem) && problem.Retryable
	if (providerRetry || retryableDatabase(cause)) && job.Attempts < 5 {
		state = "pending"
		completed = nil
		available = now.Add(time.Duration(60*(1<<(job.Attempts-1))) * time.Second)
		if problem != nil && problem.RetryAt.After(available) {
			available = problem.RetryAt
		}
	}
	outcome, err := transaction.ExecContext(ctx, "UPDATE backfill_requests SET state=?,completed_at=?,available_at=?,claim_token=NULL,lease_until=NULL,last_error_code=? WHERE request_id=? AND claim_token=? AND lease_until>?", state, completed, available.UTC(), errorCode(cause), job.ID, job.Token, now.UTC().Add(time.Since(started)))
	if err != nil {
		return err
	}
	affected, err := outcome.RowsAffected()
	if err != nil || affected != 1 {
		return failure("lease_lost")
	}
	if err = commit(transaction); err != nil {
		return err
	}
	logger := store.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("backfill_retry_outcome", "request_id", job.ID, "attempt", job.Attempts, "state", state, "next_attempt_at", available.UTC(), "error_code", errorCode(cause))
	return nil
}
