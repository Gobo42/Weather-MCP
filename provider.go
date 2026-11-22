package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const currentEndpoint = "https://api.open-meteo.com/v1/forecast"
const historyEndpoint = "https://archive-api.open-meteo.com/v1/archive"

type Reading struct {
	Source                                     string
	Selection                                  string
	ObservedAt, RetrievedAt                    time.Time
	IntervalSeconds                            int
	TemperatureC, HumidityPct                  float64
	GridLatitude, GridLongitude, GridElevation *float64
}
type HistoryResult struct {
	Readings []Reading
	Complete bool
}
type ProviderError struct {
	Code      string
	Retryable bool
	RetryAt   time.Time
}

func (providerError *ProviderError) Error() string { return providerError.Code }
func providerFailure(code string, retryable bool) error {
	return &ProviderError{Code: code, Retryable: retryable}
}

type Provider struct {
	client              *http.Client
	latitude, longitude float64
	gate                *ProviderGate
	logger              *slog.Logger
}

func newProvider(config Config) *Provider {
	return &Provider{client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, latitude: *config.Location.Latitude, longitude: *config.Location.Longitude, gate: newProviderGate(time.Second)}
}

type weatherResponse struct {
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
	Elevation    *float64 `json:"elevation"`
	CurrentUnits struct {
		Temperature string `json:"temperature_2m"`
		Humidity    string `json:"relative_humidity_2m"`
	} `json:"current_units"`
	Current struct {
		Time        *int64   `json:"time"`
		Interval    int      `json:"interval"`
		Temperature *float64 `json:"temperature_2m"`
		Humidity    *float64 `json:"relative_humidity_2m"`
	} `json:"current"`
	HourlyUnits struct {
		Temperature string `json:"temperature_2m"`
		Humidity    string `json:"relative_humidity_2m"`
	} `json:"hourly_units"`
	Hourly struct {
		Time        []int64    `json:"time"`
		Temperature []*float64 `json:"temperature_2m"`
		Humidity    []*float64 `json:"relative_humidity_2m"`
	} `json:"hourly"`
}

func validNumber(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}
func (provider *Provider) fetch(ctx context.Context, endpoint string, parameters url.Values) (weatherResponse, error) {
	var result weatherResponse
	release, err := provider.gate.Acquire(ctx, endpoint == currentEndpoint)
	if err != nil {
		return result, err
	}
	defer release()
	parameters.Set("latitude", strconv.FormatFloat(provider.latitude, 'f', -1, 64))
	parameters.Set("longitude", strconv.FormatFloat(provider.longitude, 'f', -1, 64))
	parameters.Set("temperature_unit", "celsius")
	parameters.Set("timezone", "GMT")
	parameters.Set("timeformat", "unixtime")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+parameters.Encode(), nil)
	if err != nil {
		return result, providerFailure("invalid_request", false)
	}
	started := time.Now()
	response, err := provider.client.Do(request)
	logger := provider.logger
	if logger == nil {
		logger = slog.Default()
	}
	if err == nil {
		logger.Debug("provider_response", "operation", map[bool]string{true: "current", false: "history"}[endpoint == currentEndpoint], "http_status", response.StatusCode, "duration_ms", time.Since(started).Milliseconds())
	}
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, providerFailure("provider_transport", true)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		problem := &ProviderError{Code: "provider_http", Retryable: response.StatusCode == 429 || response.StatusCode >= 500 && response.StatusCode <= 599}
		if problem.Retryable {
			raw := response.Header.Get("Retry-After")
			if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 && seconds <= time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC).Unix()-time.Now().Unix() {
				problem.RetryAt = time.Unix(time.Now().Unix()+seconds, 0).UTC()
			} else if retry, err := http.ParseTime(raw); err == nil && retry.After(time.Now()) && retry.Year() < 9999 {
				problem.RetryAt = retry
			}
		}
		if problem.RetryAt.After(time.Now()) {
			provider.gate.DeferUntil(problem.RetryAt)
			logger.Warn("provider_throttled", "error_code", problem.Code, "next_attempt_at", problem.RetryAt.UTC())
			if provider.gate.persist != nil {
				if err := provider.gate.persist(ctx, problem.RetryAt); err != nil {
					return result, err
				}
			}
		}
		return result, problem
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return result, providerFailure("provider_transport", true)
	}
	if len(body) > 1<<20 || json.Unmarshal(body, &result) != nil {
		return result, providerFailure("invalid_response", false)
	}
	for _, value := range []*float64{result.Latitude, result.Longitude, result.Elevation} {
		if value != nil && !validNumber(value) {
			return result, providerFailure("invalid_response", false)
		}
	}
	if result.Latitude != nil && math.Abs(*result.Latitude) > 90 || result.Longitude != nil && math.Abs(*result.Longitude) > 180 {
		return result, providerFailure("invalid_response", false)
	}
	return result, nil
}
func (response weatherResponse) reading(source string, observed int64, interval int, temperature, humidity *float64) (Reading, error) {
	if !validNumber(temperature) || !validNumber(humidity) || *humidity < 0 || *humidity > 100 || interval <= 0 {
		return Reading{}, providerFailure("invalid_response", false)
	}
	at := time.Unix(observed, 0).UTC()
	if at.Year() < 1940 || at.Year() > 9998 || at.After(time.Now().Add(time.Minute)) {
		return Reading{}, providerFailure("clock_anomaly", false)
	}
	return Reading{Source: source, Selection: "best_match", ObservedAt: at, RetrievedAt: time.Now().UTC().Truncate(time.Microsecond), IntervalSeconds: interval, TemperatureC: *temperature, HumidityPct: *humidity, GridLatitude: response.Latitude, GridLongitude: response.Longitude, GridElevation: response.Elevation}, nil
}
func (provider *Provider) Current(ctx context.Context) (Reading, error) {
	response, err := provider.fetch(ctx, currentEndpoint, url.Values{"current": {"temperature_2m,relative_humidity_2m"}})
	if err != nil {
		return Reading{}, err
	}
	if response.CurrentUnits.Temperature != "°C" || response.CurrentUnits.Humidity != "%" || response.Current.Time == nil {
		return Reading{}, providerFailure("invalid_response", false)
	}
	return response.reading("current", *response.Current.Time, response.Current.Interval, response.Current.Temperature, response.Current.Humidity)
}
func (provider *Provider) History(ctx context.Context, chunk DateRange) (HistoryResult, error) {
	parameters := url.Values{"hourly": {"temperature_2m,relative_humidity_2m"}, "start_date": {chunk.StartUTC.Format(time.DateOnly)}, "end_date": {chunk.EndUTC.Add(-time.Second).Format(time.DateOnly)}}
	response, err := provider.fetch(ctx, historyEndpoint, parameters)
	if err != nil {
		return HistoryResult{}, err
	}
	hourly := response.Hourly
	if response.HourlyUnits.Temperature != "°C" || response.HourlyUnits.Humidity != "%" || len(hourly.Time) != len(hourly.Temperature) || len(hourly.Time) != len(hourly.Humidity) {
		return HistoryResult{}, providerFailure("invalid_response", false)
	}
	result := HistoryResult{}
	for index, stamp := range hourly.Time {
		if stamp%3600 != 0 || index > 0 && stamp <= hourly.Time[index-1] {
			return HistoryResult{}, providerFailure("invalid_response", false)
		}
		at := time.Unix(stamp, 0).UTC()
		if at.Before(chunk.StartUTC) || !at.Before(chunk.EndUTC) {
			continue
		}
		if hourly.Temperature[index] == nil || hourly.Humidity[index] == nil {
			continue
		}
		reading, err := response.reading("history", stamp, 3600, hourly.Temperature[index], hourly.Humidity[index])
		if err != nil {
			return HistoryResult{}, err
		}
		result.Readings = append(result.Readings, reading)
	}
	result.Complete = len(result.Readings) == hourlyCount(chunk)
	return result, nil
}
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	var problem *ProviderError
	if errors.As(err, &problem) {
		switch problem.Code {
		case "provider_http", "provider_transport", "invalid_response", "clock_anomaly", "invalid_request", "provider_cooldown":
			return problem.Code
		default:
			return "operation_failed"
		}
	}
	for _, code := range []string{"location_selector_required", "invalid_plus_code", "plus_code_place_required", "geocoding_failed", "geocoding_no_unique_match", "location_update_cancelled", "config_write_failed", "config_changed", "observation_conflict", "data_unavailable", "invalid_date_range", "lease_lost", "write_outcome_unknown", "invalid_arguments", "invalid_configuration", "invalid_database_configuration", "invalid_credentials", "private_file_unavailable", "private_file_required", "private_file_read_failed", "private_directory_required", "private_parent_required", "symlink_refused", "invalid_json", "schema_invalid", "grants_invalid", "database_identity_invalid", "location_mismatch", "collector_already_running", "state_lock_unavailable"} {
		if err.Error() == code {
			return code
		}
	}
	if retryableDatabase(err) {
		return "database_unavailable"
	}
	return "operation_failed"
}
