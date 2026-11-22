package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type testTransport func(*http.Request) (*http.Response, error)

func (transport testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
func providerFixture(testingContext *testing.T, body string, status int) *Provider {
	testingContext.Helper()
	latitude, longitude := 40.0, -105.0
	provider := newProvider(Config{Location: Location{Latitude: &latitude, Longitude: &longitude}})
	provider.client = &http.Client{Transport: testTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Query().Get("timezone") != "GMT" || request.URL.Query().Get("timeformat") != "unixtime" {
			testingContext.Fatal("incorrect request")
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return provider
}
func TestProviderCurrent(testingContext *testing.T) {
	body := fmt.Sprintf(`{"latitude":40,"longitude":-105,"elevation":12,"current_units":{"temperature_2m":"°C","relative_humidity_2m":"%%"},"current":{"time":%d,"interval":900,"temperature_2m":0,"relative_humidity_2m":0}}`, time.Now().Unix())
	result, err := providerFixture(testingContext, body, 200).Current(context.Background())
	if err != nil || result.TemperatureC != 0 || result.HumidityPct != 0 || result.Source != "current" || result.IntervalSeconds != 900 {
		testingContext.Fatal(result, err)
	}
}
func TestProviderValidation(testingContext *testing.T) {
	for _, body := range []string{`{}`, `{"current_units":{"temperature_2m":"°F","relative_humidity_2m":"%"}}`, `{"current_units":{"temperature_2m":"°C","relative_humidity_2m":"%"},"current":{"time":0,"interval":900,"temperature_2m":null,"relative_humidity_2m":30}}`} {
		if _, err := providerFixture(testingContext, body, 200).Current(context.Background()); err == nil {
			testingContext.Fatal("invalid response accepted")
		}
	}
	for _, current := range []string{`"temperature_2m":20,"relative_humidity_2m":101`, `"temperature_2m":20,"relative_humidity_2m":-1`, `"temperature_2m":null,"relative_humidity_2m":50`} {
		body := fmt.Sprintf(`{"current_units":{"temperature_2m":"°C","relative_humidity_2m":"%%"},"current":{"time":%d,"interval":900,%s}}`, time.Now().Unix(), current)
		if _, err := providerFixture(testingContext, body, 200).Current(context.Background()); err == nil {
			testingContext.Fatal("invalid numeric reading accepted")
		}
	}
}
func TestProviderHistory(testingContext *testing.T) {
	request, _ := calendarRange("2026-03-08", "2026-03-08")
	var timestamps, temperatures, humidities []string
	for observed := request.StartUTC; observed.Before(request.EndUTC); observed = observed.Add(time.Hour) {
		timestamps = append(timestamps, fmt.Sprint(observed.Unix()))
		temperatures = append(temperatures, "20")
		humidities = append(humidities, "50")
	}
	body := `{"hourly_units":{"temperature_2m":"°C","relative_humidity_2m":"%"},"hourly":{"time":[` + strings.Join(timestamps, ",") + `],"temperature_2m":[` + strings.Join(temperatures, ",") + `],"relative_humidity_2m":[` + strings.Join(humidities, ",") + `]}}`
	result, err := providerFixture(testingContext, body, 200).History(context.Background(), request)
	if err != nil || !result.Complete || len(result.Readings) != 23 {
		testingContext.Fatal(result, err)
	}
	body = strings.Replace(body, `"temperature_2m":[20`, `"temperature_2m":[null`, 1)
	result, err = providerFixture(testingContext, body, 200).History(context.Background(), request)
	if err != nil || result.Complete || len(result.Readings) != 22 {
		testingContext.Fatal("partial history mishandled", result, err)
	}
}
func TestProviderLimits(testingContext *testing.T) {
	provider := providerFixture(testingContext, strings.Repeat(" ", (1<<20)+1), 200)
	if _, err := provider.Current(context.Background()); err == nil {
		testingContext.Fatal("unbounded response")
	}
	provider = providerFixture(testingContext, `{}`, 200)
	provider.gate.nextStart = time.Now().Add(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Current(ctx); err == nil {
		testingContext.Fatal("ignored cancellation")
	}
}
func TestProviderRetryAfter(testingContext *testing.T) {
	_, err := providerFixture(testingContext, `{}`, 429).Current(context.Background())
	var providerError *ProviderError
	if !errors.As(err, &providerError) || !providerError.Retryable || time.Until(providerError.RetryAt) < 119*time.Second {
		testingContext.Fatal(err)
	}
	_, err = providerFixture(testingContext, `{}`, 400).Current(context.Background())
	if !errors.As(err, &providerError) || providerError.Retryable {
		testingContext.Fatal(err)
	}
}
