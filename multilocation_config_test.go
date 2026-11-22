package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMultipleLocationConfiguration(t *testing.T) {
	filename := configurationFixture(t)
	raw, _ := os.ReadFile(filename)
	var data map[string]any
	json.Unmarshal(raw, &data)
	first := data["location"]
	second := map[string]any{"id": "east", "name": "East", "latitude": 41.0, "longitude": -74.0, "timezone": "America/New_York"}
	delete(data, "location")
	data["version"] = 2
	data["locations"] = []any{first, second}
	data["log_level"] = "debug"
	write := func() {
		t.Helper()
		b, _ := json.Marshal(data)
		if err := os.WriteFile(filename, b, 0640); err != nil {
			t.Fatal(err)
		}
	}
	write()
	config, err := loadConfig(filename)
	if err != nil || len(config.locations()) != 2 || config.LogLevel != "debug" {
		t.Fatalf("multiple locations rejected: %v", err)
	}
	for _, bad := range []string{"duplicate", "empty", "mixed", "timezone", "log_level", "version", "null"} {
		original, _ := json.Marshal(data)
		switch bad {
		case "duplicate":
			data["locations"] = []any{first, first}
		case "empty":
			data["locations"] = []any{}
		case "mixed":
			data["location"] = first
		case "timezone":
			second["timezone"] = "bad/zone"
		case "log_level":
			data["log_level"] = "trace"
		case "version":
			data["version"] = 3
		case "null":
			data["locations"] = nil
		}
		write()
		if _, err := loadConfig(filename); err == nil {
			t.Errorf("accepted %s", bad)
		}
		json.Unmarshal(original, &data)
		second = data["locations"].([]any)[1].(map[string]any)
	}
	legacy := configurationFixture(t)
	config, err = loadConfig(legacy)
	if err != nil || len(config.locations()) != 1 || config.locations()[0].ID != "home" {
		t.Fatal("legacy configuration lost", err)
	}
}

func TestLocationCalendarZones(t *testing.T) {
	cases := []struct {
		zone, start, end string
		hours            int
	}{{"America/Los_Angeles", "2024-03-10", "2024-03-10", 23}, {"America/New_York", "2024-11-03", "2024-11-03", 25}, {"Asia/Taipei", "2024-03-10", "2024-03-10", 24}, {"Australia/Lord_Howe", "2024-10-06", "2024-10-06", 23}, {"Asia/Kathmandu", "2024-03-10", "2024-03-10", 24}}
	for _, c := range cases {
		t.Run(c.zone, func(t *testing.T) {
			r, err := calendarRange(c.start, c.end, c.zone)
			if err != nil {
				t.Fatal(err)
			}
			if r.Zone != c.zone || hourlyCount(r) != c.hours {
				t.Fatalf("wrong local-day hourly coverage: %+v count=%d", r, hourlyCount(r))
			}
			chunk, err := nextChunk(r, c.start)
			if err != nil || chunk != r {
				t.Fatal("chunk lost zone", err)
			}
		})
	}
	now := time.Date(2024, 3, 11, 3, 0, 0, 0, time.UTC)
	if _, err := parseDateRange("2024-03-10", "2024-03-10", now, "America/Los_Angeles"); err == nil {
		t.Fatal("accepted local today")
	}
	if _, err := parseDateRange("2024-03-10", "2024-03-10", now, "Asia/Taipei"); err != nil {
		t.Fatal("rejected Taipei yesterday", err)
	}
	if _, err := calendarRange("2024-03-10", "2024-03-10", "bad/zone"); err == nil {
		t.Fatal("invalid zone accepted")
	}
}

func TestSetupSelectsLocation(t *testing.T) {
	filename := configurationFixture(t)
	raw, _ := os.ReadFile(filename)
	var data map[string]any
	json.Unmarshal(raw, &data)
	first := data["location"]
	delete(data, "location")
	data["version"] = 2
	second := map[string]any{"id": "east", "name": "East", "latitude": 41.0, "longitude": -74.0, "timezone": "America/New_York"}
	data["locations"] = []any{first, second}
	raw, _ = json.Marshal(data)
	os.WriteFile(filename, raw, 0640)
	if err := os.Chmod(filename, 0640); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"", "missing"} {
		var out bytes.Buffer
		args := []string{"--config", filename, "--plus-code", "849VCWC8+R9"}
		if selector != "" {
			args = append(args, "--location", selector)
		}
		if err := setLocationCommand(context.Background(), args, strings.NewReader("yes\n"), &out); err == nil {
			t.Fatal("ambiguous/unknown selector accepted")
		}
		after, _ := os.ReadFile(filename)
		if !bytes.Equal(raw, after) {
			t.Fatal("refused edit changed config")
		}
	}
	var out bytes.Buffer
	if err := setLocationCommand(context.Background(), []string{"--config", filename, "--location", "east", "--plus-code", "849VCWC8+R9"}, strings.NewReader("yes\n"), &out); err != nil {
		t.Fatal(err)
	}
	updated, _, _, err := loadLocationConfig(filename)
	if err != nil {
		t.Fatal(err)
	}
	if *updated.Locations[0].Latitude != 40 || *updated.Locations[1].Latitude == 41 || updated.Locations[1].Timezone != "America/New_York" {
		t.Fatal("wrong location modified")
	}
	b, _ := os.ReadFile(filename)
	var saved map[string]any
	json.Unmarshal(b, &saved)
	if _, ok := saved["location"]; ok {
		t.Fatal("v2 edit introduced legacy field")
	}
	info, _ := os.Stat(filename)
	if info.Mode().Perm() != 0640 {
		t.Fatal("config mode changed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filename), "state")); !os.IsNotExist(err) {
		t.Fatal("setup created state")
	}
}

func TestLoggingLevelsAndSanitization(t *testing.T) {
	var output bytes.Buffer
	logger := newLogger(&output, "info", false)
	logger.Debug("current_poll_completed", "location_id", "home")
	logger.Info("service_started", "locations", 2)
	if strings.Contains(output.String(), "current_poll_completed") || !strings.Contains(output.String(), "service_started") {
		t.Fatal(output.String())
	}
	output.Reset()
	logger = newLogger(&output, "info", true)
	logger.Debug("current_poll_completed", "location_id", "home")
	if !strings.Contains(output.String(), "level=DEBUG") || !strings.Contains(output.String(), "location_id=home") {
		t.Fatal(output.String())
	}
	if errorCode(failure("password=secret-canary; latitude=12.345")) != "operation_failed" {
		t.Fatal("raw error escaped sanitization")
	}
	output.Reset()
	logger = newLogger(&output, "debug", false)
	logger.Debug("debug_enabled")
	if !strings.Contains(output.String(), "debug_enabled") {
		t.Fatal("config debug ignored")
	}
}

func TestHourlyGridEmptyWindow(t *testing.T) {
	start := time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC)
	if count := hourlyCount(DateRange{StartUTC: start, EndUTC: start.Add(15 * time.Minute)}); count != 0 {
		t.Fatal("fabricated an hourly sample", count)
	}
}

func TestConfigLocationBoundaries(t *testing.T) {
	filename := configurationFixture(t)
	original, _ := os.ReadFile(filename)
	var legacy map[string]any
	json.Unmarshal(original, &legacy)
	first := legacy["location"]
	delete(legacy, "location")
	legacy["version"] = 2
	legacy["locations"] = []any{first}
	legacy["log_level"] = "info"
	base, _ := json.Marshal(legacy)
	write := func(value map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(value)
		if err := os.WriteFile(filename, raw, 0640); err != nil {
			t.Fatal(err)
		}
	}
	write(legacy)
	if _, err := loadConfig(filename); err != nil {
		t.Fatal("one-location v2/info configuration refused", err)
	}
	for _, kind := range []string{"id", "blank_name", "long_name", "empty_zone", "local_zone", "latitude", "longitude"} {
		var data map[string]any
		json.Unmarshal(base, &data)
		location := data["locations"].([]any)[0].(map[string]any)
		switch kind {
		case "id":
			location["id"] = "bad-id"
		case "blank_name":
			location["name"] = " "
		case "long_name":
			location["name"] = strings.Repeat("n", 129)
		case "empty_zone":
			location["timezone"] = ""
		case "local_zone":
			location["timezone"] = "Local"
		case "latitude":
			location["latitude"] = 91
		case "longitude":
			location["longitude"] = -181
		}
		write(data)
		if _, err := loadConfig(filename); err == nil {
			t.Errorf("accepted invalid %s", kind)
		}
	}
	var bound map[string]any
	json.Unmarshal(base, &bound)
	location := bound["locations"].([]any)[0].(map[string]any)
	location["name"] = strings.Repeat("n", 128)
	location["latitude"] = 90
	location["longitude"] = -180
	write(bound)
	if _, err := loadConfig(filename); err != nil {
		t.Fatal("valid coordinate/name boundaries refused", err)
	}
	// A legacy file with an empty extra array is still a contradictory representation.
	var mixed map[string]any
	json.Unmarshal(original, &mixed)
	mixed["locations"] = []any{}
	write(mixed)
	if _, err := loadConfig(filename); err == nil {
		t.Fatal("legacy mixed representation accepted")
	}
}
