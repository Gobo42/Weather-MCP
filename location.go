package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	olc "github.com/google/open-location-code/go"
)

type codeLocation struct {
	Code      string
	Place     string
	Latitude  float64
	Longitude float64
}

func parseCopiedCode(text string) (string, string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 || len(text) > 512 {
		return "", "", failure("invalid_plus_code")
	}
	code := strings.ToUpper(fields[0])
	if len(code) > 16 || olc.Check(code) != nil {
		return "", "", failure("invalid_plus_code")
	}
	if olc.CheckFull(code) == nil {
		return code, "", nil
	}
	if olc.CheckShort(code) != nil {
		return "", "", failure("invalid_plus_code")
	}
	if len(fields) < 2 {
		return "", "", failure("plus_code_place_required")
	}
	return code, strings.Join(fields[1:], " "), nil
}

func resolvePlusCode(ctx context.Context, text string, client *http.Client) (codeLocation, error) {
	result := codeLocation{Place: "not needed (full code; offline)"}
	code, place, err := parseCopiedCode(text)
	if err != nil {
		return result, err
	}
	if place != "" {
		reference, err := geocodePlace(ctx, place, client)
		if err != nil {
			return result, err
		}
		result.Place = strings.Join([]string{reference.Name, reference.Admin1, reference.Country}, ", ")
		code, err = olc.RecoverNearest(code, *reference.Latitude, *reference.Longitude)
		if err != nil {
			return result, failure("invalid_plus_code")
		}
	}
	area, err := olc.Decode(code)
	if err != nil {
		return result, failure("invalid_plus_code")
	}
	result.Code = code
	result.Latitude, result.Longitude = area.Center()
	return result, nil
}

type geocodedPlace struct {
	Name      string   `json:"name"`
	Admin1    string   `json:"admin1"`
	Country   string   `json:"country"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
}

func geocodePlace(ctx context.Context, place string, client *http.Client) (geocodedPlace, error) {
	query := url.Values{"name": {place}, "count": {"100"}, "language": {"en"}, "format": {"json"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://geocoding-api.open-meteo.com/v1/search?"+query.Encode(), nil)
	if err != nil {
		return geocodedPlace{}, failure("geocoding_failed")
	}
	response, err := client.Do(request)
	if err != nil {
		return geocodedPlace{}, failure("geocoding_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return geocodedPlace{}, failure("geocoding_failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return geocodedPlace{}, failure("geocoding_failed")
	}
	var data struct {
		Results []geocodedPlace `json:"results"`
	}
	if json.Unmarshal(body, &data) != nil {
		return geocodedPlace{}, failure("geocoding_failed")
	}
	return selectPlace(place, data.Results)
}

func selectPlace(place string, results []geocodedPlace) (geocodedPlace, error) {
	name := strings.TrimSpace(strings.SplitN(place, ",", 2)[0])
	var matches []geocodedPlace
	for _, result := range results {
		if strings.EqualFold(name, result.Name) {
			matches = append(matches, result)
		}
	}
	if len(matches) != 1 {
		return geocodedPlace{}, failure("geocoding_no_unique_match")
	}
	match := matches[0]
	if !validReference(match.Latitude, 90) || !validReference(match.Longitude, 180) {
		return geocodedPlace{}, failure("geocoding_failed")
	}
	return match, nil
}

func setLocationCommand(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("set-location", flag.ContinueOnError)
	flags.SetOutput(output)
	filename := flags.String("config", "/etc/weather-agent/collector.json", "Existing collector JSON file (0600 or 0640)")
	debug := flags.Bool("debug", false, "Enable diagnostic logging for this command")
	selector := flags.String("location", "", "Location ID (required with multiple locations)")
	text := flags.String("plus-code", "", "Full Plus Code or short Plus Code followed by a city and region")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return failure("invalid_arguments")
	}
	if flags.NArg() != 0 || strings.TrimSpace(*text) == "" {
		return failure("invalid_arguments")
	}
	config, _, _, err := loadLocationConfig(*filename)
	if err != nil {
		return err
	}
	logger := newLogger(os.Stderr, config.LogLevel, *debug)
	logger.Debug("location_setup_started")
	err = editLocation(ctx, *filename, *text, input, output, *selector)
	if err != nil {
		logger.Warn("location_setup_failed", "error_code", errorCode(err))
	} else {
		logger.Debug("location_setup_completed")
	}
	return err
}

func loadLocationConfig(filename string) (Config, []byte, os.FileInfo, error) {
	var config Config
	original, err := readProtected(filename, configUnsafePermissions)
	if err != nil {
		return config, nil, nil, err
	}
	if err = decodeJSON(original, &config); err != nil {
		return config, nil, nil, err
	}
	if err = validateCollector(config); err != nil {
		return config, nil, nil, err
	}
	info, err := os.Lstat(filename)
	return config, original, info, err
}

func editLocation(ctx context.Context, filename, text string, input io.Reader, output io.Writer, selectors ...string) error {
	config, original, info, err := loadLocationConfig(filename)
	if err != nil {
		return err
	}
	selected, err := selectLocation(config, selectors...)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	location, err := resolvePlusCode(ctx, text, client)
	if err != nil {
		return err
	}
	if err = confirmLocation(ctx, input, output, selected, location); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if config.Version == 1 {
		config.Location.Latitude, config.Location.Longitude = &location.Latitude, &location.Longitude
	} else {
		for i := range config.Locations {
			if config.Locations[i].ID == selected.ID {
				config.Locations[i].Latitude, config.Locations[i].Longitude = &location.Latitude, &location.Longitude
			}
		}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return failure("invalid_configuration")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return failure("invalid_configuration")
	}
	if config.Version == 2 {
		delete(fields, "location")
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return failure("invalid_configuration")
	}
	if err = replaceLocationConfig(filename, original, append(data, '\n'), info); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "Updated latitude/longitude in", filename)
	return err
}

func confirmLocation(ctx context.Context, input io.Reader, output io.Writer, before Location, after codeLocation) error {
	_, err := fmt.Fprintf(output, "Full Plus Code: %s\nReference place: %s\nCell center: %.10f, %.10f\nPrevious coordinates: %.10f, %.10f\nOnly latitude/longitude will change. Existing stored observations cannot be relocated by this command.\nType yes to save: ", after.Code, after.Place, after.Latitude, after.Longitude, *before.Latitude, *before.Longitude)
	if err != nil {
		return err
	}
	answer := make(chan bool)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 128), 1024)
		accepted := scanner.Scan() && strings.TrimSpace(scanner.Text()) == "yes"
		select {
		case answer <- accepted:
		case <-ctx.Done():
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case accepted := <-answer:
		if !accepted {
			return failure("location_update_cancelled")
		}
	}

	return nil
}

func replaceLocationConfig(path string, original, updated []byte, before os.FileInfo) error {
	if err := safePath(path); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".location-*")
	if err != nil {
		return failure("config_write_failed")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = writeLocationFile(file, updated, before); err != nil {
		return err
	}
	if err = unchangedConfig(path, original, before); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return failure("config_write_failed")
	}
	return nil
}

func unchangedConfig(path string, original []byte, before os.FileInfo) error {
	current, err := readProtected(path, configUnsafePermissions)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return failure("config_changed")
	}
	oldOwner, owner := before.Sys().(*syscall.Stat_t), info.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, info) || before.Mode() != info.Mode() || !sameConfigOwner(oldOwner, owner) || !bytes.Equal(original, current) {
		return failure("config_changed")
	}
	return nil
}

func writeLocationFile(file *os.File, data []byte, before os.FileInfo) error {
	owner := before.Sys().(*syscall.Stat_t)
	if err := file.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
		return failure("config_write_failed")
	}
	if err := file.Chmod(before.Mode().Perm()); err != nil {
		return failure("config_write_failed")
	}
	if _, err := file.Write(data); err != nil {
		return failure("config_write_failed")
	}
	if err := file.Sync(); err != nil {
		return failure("config_write_failed")
	}
	return nil
}

func sameConfigOwner(before, after *syscall.Stat_t) bool {
	return before.Uid == after.Uid && before.Gid == after.Gid
}

func selectLocation(config Config, selectors ...string) (Location, error) {
	selector := ""
	if len(selectors) > 0 {
		selector = selectors[0]
	}
	locations := config.locations()
	if selector == "" && len(locations) == 1 {
		return locations[0], nil
	}
	for _, location := range locations {
		if location.ID == selector {
			return location, nil
		}
	}
	return Location{}, failure("location_selector_required")
}
