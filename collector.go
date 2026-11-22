package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

type Collector struct {
	Config                                Config
	Store                                 *Store
	Provider                              *Provider
	Logger                                *slog.Logger
	nextCurrent, regularCurrent, cooldown time.Time
	currentAttempts                       int
	status                                Status
	initialized                           bool
}

func (collector *Collector) initialize(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var lastAttempt, lastSuccess, cooldown sql.NullTime
	err := collector.Store.db.QueryRowContext(ctx, "SELECT last_attempt_at,last_success_at,provider_not_before FROM collector_status WHERE location_id=?", collector.Store.config.Location.ID).Scan(&lastAttempt, &lastSuccess, &cooldown)
	if err != nil {
		return failure("database_unavailable")
	}
	if lastAttempt.Valid {
		collector.status.LastAttemptAt = &lastAttempt.Time
	}
	if lastSuccess.Valid {
		collector.status.LastSuccessAt = &lastSuccess.Time
	}
	if cooldown.Valid {
		collector.cooldown = cooldown.Time
	}
	collector.nextCurrent = now
	collector.regularCurrent = now
	collector.initialized = true
	logger := collector.Logger
	if logger == nil {
		logger = slog.Default().With("location_id", collector.Store.config.Location.ID)
	}
	logger.Info("location_recovered", "last_success_at", collector.status.LastSuccessAt, "next_attempt_at", collector.nextCurrent)
	return nil
}
func (collector *Collector) persistStatus(ctx context.Context, now time.Time) error {
	collector.status.NextAttemptAt = &collector.nextCurrent
	collector.status.ProviderNotBefore = nil
	if collector.cooldown.After(now) {
		collector.status.ProviderNotBefore = &collector.cooldown
	}
	return collector.Store.UpdateStatus(ctx, collector.status)
}
func (collector *Collector) logger() *slog.Logger {
	if collector.Logger != nil {
		return collector.Logger
	}
	return slog.Default().With("location_id", collector.Store.config.Location.ID)
}
func (collector *Collector) Step(ctx context.Context, now time.Time) (time.Time, error) {
	now = now.UTC()
	if !collector.initialized {
		if err := collector.initialize(ctx, now); err != nil {
			return now.Add(5 * time.Second), err
		}
	}
	if deadline := collector.Provider.gate.NotBefore(); deadline.After(collector.cooldown) {
		collector.cooldown = deadline
	}
	if collector.cooldown.After(now) {
		return collector.cooldown, nil
	}
	if !collector.nextCurrent.After(now) {
		return collector.pollCurrent(ctx, now)
	}
	return collector.pollHistory(ctx, now)
}
func (collector *Collector) scheduleCurrent(now time.Time, err error) {
	collector.status.LastErrorCode = errorCode(err)
	collector.status.Failures = collector.currentAttempts
	var problem *ProviderError
	if errors.As(err, &problem) && problem.Retryable && collector.currentAttempts < 5 {
		collector.nextCurrent = now.Add(time.Duration(60*(1<<(collector.currentAttempts-1))) * time.Second)
		if problem.RetryAt.After(now) {
			collector.cooldown = problem.RetryAt
		}
		if collector.cooldown.After(collector.nextCurrent) {
			collector.nextCurrent = collector.cooldown
		}
		return
	}
	period := 900 * time.Second
	collector.regularCurrent = collector.regularCurrent.Add((now.Sub(collector.regularCurrent)/period + 1) * period)
	collector.nextCurrent = collector.regularCurrent
	collector.currentAttempts = 0
	if err == nil {
		collector.status.Failures = 0
	}
	if errors.As(err, &problem) && problem.RetryAt.After(now) {
		collector.cooldown = problem.RetryAt
	}
}
func (collector *Collector) pollCurrent(ctx context.Context, now time.Time) (time.Time, error) {
	logger := collector.logger()
	logger.Debug("current_poll_scheduled", "operation", "current", "scheduled_at", collector.nextCurrent)
	logger.Debug("current_poll_started", "operation", "current")
	started := time.Now()
	reading, err := collector.Provider.Current(ctx)
	collector.currentAttempts++
	collector.status.LastAttemptAt = &now
	if err == nil {
		err = collector.Store.PersistCurrent(ctx, reading, now)
		if err == nil {
			collector.status.LastSuccessAt = &now
		}
	}
	collector.scheduleCurrent(now, err)
	if err == nil {
		logger.Debug("current_poll_completed", "operation", "current", "duration_ms", time.Since(started).Milliseconds(), "next_attempt_at", collector.nextCurrent)
	} else {
		logger.Warn("current_poll_failed", "operation", "current", "error_code", errorCode(err), "duration_ms", time.Since(started).Milliseconds(), "next_attempt_at", collector.nextCurrent)
	}
	statusErr := collector.persistStatus(ctx, now)
	if err != nil {
		return now, err
	}
	return now, statusErr
}
func (collector *Collector) nextCheck(now time.Time) time.Time {
	next := now.Add(5 * time.Second)
	if collector.nextCurrent.Before(next) {
		next = collector.nextCurrent
	}
	return next
}
func (collector *Collector) processHistory(ctx context.Context, job Job, now time.Time) (int, error) {
	request, err := parseDateRange(job.StartDate, job.EndDate, now, collector.Store.config.Location.Timezone)
	if err != nil {
		return 0, err
	}
	chunk, err := nextChunk(request, job.NextDate)
	if err != nil {
		return 0, err
	}
	result, err := collector.Provider.History(ctx, chunk)
	if err != nil {
		return 0, err
	}
	return len(result.Readings), collector.Store.CommitChunk(ctx, job, chunk, result, time.Now().UTC())
}
func (collector *Collector) historyFailure(ctx context.Context, job Job, cause error, now time.Time) error {
	var problem *ProviderError
	if errors.As(cause, &problem) && problem.RetryAt.After(now) {
		collector.cooldown = problem.RetryAt
		if err := collector.persistStatus(ctx, now); err != nil {
			return err
		}
	}
	if err := collector.Store.FailOrRetry(ctx, job, cause, time.Now().UTC()); err != nil && errorCode(err) != "lease_lost" {
		return err
	}
	return cause
}
func (collector *Collector) pollHistory(ctx context.Context, now time.Time) (time.Time, error) {
	chunkContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	job, err := collector.Store.Claim(chunkContext, now)
	if err != nil {
		return now.Add(5 * time.Second), err
	}
	if job == nil {
		return collector.nextCheck(now), nil
	}
	logger := collector.logger()
	logger.Debug("backfill_claimed", "operation", "history", "request_id", job.ID, "attempt", job.Attempts)
	started := time.Now()
	rows, err := collector.processHistory(chunkContext, *job, now)
	if err != nil {
		err = collector.historyFailure(ctx, *job, err, now)
		logger.Warn("backfill_failed", "request_id", job.ID, "error_code", errorCode(err), "duration_ms", time.Since(started).Milliseconds())
		return now.Add(5 * time.Second), err
	}
	logger.Debug("backfill_chunk_completed", "request_id", job.ID, "rows", rows, "duration_ms", time.Since(started).Milliseconds())
	return collector.nextCheck(now), nil
}
func (collector *Collector) Run(ctx context.Context, once bool) error {
	if once {
		_, first := collector.Step(ctx, time.Now().UTC())
		_, second := collector.Step(ctx, time.Now().UTC())
		if first != nil {
			return first
		}
		return second
	}
	for ctx.Err() == nil {
		next, err := collector.Step(ctx, time.Now().UTC())
		if err != nil {
			logger := collector.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Error("collector_failed", "error_code", errorCode(err))
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
