package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSetLocationCommandFullCodeWithoutDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.json")
	original := []byte(`{"version":1,"location":{"id":"home","name":"Example location","latitude":40,"longitude":-105,"timezone":"America/Los_Angeles"},"poll_seconds":900,"mariadb_file":"missing.json","state_dir":"/missing/state"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	input.WriteString("yes\n")
	input.Seek(0, 0)
	oldInput := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = oldInput }()
	if err := command(context.Background(), []string{"set-location", "--config", path, "--plus-code", "849VCWC8+R9"}); err != nil {
		t.Fatalf("set-location should work without MariaDB: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if math.Abs(*config.Location.Latitude-37.4220625) > 1e-10 || math.Abs(*config.Location.Longitude+122.0840625) > 1e-10 {
		t.Fatalf("wrong cell center: %s", data)
	}
	if config.MariaDBFile != "missing.json" || config.Location.Name != "Example location" || config.StateDir != "/missing/state" {
		t.Fatalf("unrelated settings changed: %s", data)
	}
}

func locationFixture(t *testing.T) (string, []byte, os.FileInfo) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "collector.json")
	data := []byte(`{"version":1,"location":{"id":"home","name":"Example location","latitude":40,"longitude":-105,"timezone":"America/Los_Angeles"},"poll_seconds":900,"mariadb_file":"missing.json","state_dir":"/missing/state"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, data, info
}

func TestResolvePlusCodeOffline(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("full/invalid code made a network request")
		return nil, errors.New("offline")
	})}
	for _, text := range []string{"849VCWC8+R9", "  849vcwc8+r9  ", "849VCWC8+R9 Mountain View, California"} {
		got, err := resolvePlusCode(context.Background(), text, client)
		if err != nil || got.Code != "849VCWC8+R9" || math.Abs(got.Latitude-37.4220625) > 1e-10 || math.Abs(got.Longitude+122.0840625) > 1e-10 {
			t.Fatalf("%q: %+v %v", text, got, err)
		}
	}
	for _, text := range []string{"", "+", "nonsense", "849VCWC8+R", "849VCWC8+R99999999", "CWC8+R9", "X49VCWC8+R9 Mountain View", strings.Repeat("x", 513)} {
		if _, err := resolvePlusCode(context.Background(), text, client); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestResolveShortPlusCode(t *testing.T) {
	body := `{"results":[{"name":"Mountain View","admin1":"California","country":"United States","latitude":37.38605,"longitude":-122.08385},{"name":"Mountain View Park","latitude":40,"longitude":-105}]}`
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "geocoding-api.open-meteo.com" || r.URL.Path != "/v1/search" || r.URL.Query().Get("name") != "Mountain View, California" {
			t.Fatalf("wrong city lookup: %s", r.URL)
		}
		if strings.Contains(r.URL.String(), "CWC8") {
			t.Fatal("sent full copied location instead of city")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	got, err := resolvePlusCode(context.Background(), "cwc8+r9 Mountain View, California", client)
	if err != nil || got.Code != "849VCWC8+R9" || got.Place != "Mountain View, California, United States" || math.Abs(got.Latitude-37.4220625) > 1e-10 || math.Abs(got.Longitude+122.0840625) > 1e-10 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestGeocodingFailures(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"no matches", `{}`, 200},
		{"prefix only", `{"results":[{"name":"Mountain View Park","latitude":37,"longitude":-122}]}`, 200},
		{"ambiguous", `{"results":[{"name":"Mountain View","latitude":37,"longitude":-122},{"name":"Mountain View","latitude":38,"longitude":-121}]}`, 200},
		{"missing coordinates", `{"results":[{"name":"Mountain View","longitude":-122}]}`, 200},
		{"null coordinates", `{"results":[{"name":"Mountain View","latitude":null,"longitude":-122}]}`, 200},
		{"out of range", `{"results":[{"name":"Mountain View","latitude":91,"longitude":-122}]}`, 200},
		{"longitude", `{"results":[{"name":"Mountain View","latitude":37,"longitude":-181}]}`, 200},
		{"bad JSON", `{`, 200}, {"trailing JSON", `{} {}`, 200},
		{"too large", strings.Repeat(" ", (1<<20)+1), 200}, {"status", `{}`, 429}, {"redirect", `{}`, 302},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			if _, err := resolvePlusCode(context.Background(), "CWC8+R9 Mountain View, California", client); err == nil {
				t.Fatal("bad geocoding accepted")
			}
		})
	}
	client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("network down") })}
	if _, err := resolvePlusCode(context.Background(), "CWC8+R9 Mountain View", client); err == nil {
		t.Fatal("network failure hidden")
	}
}

func TestSetLocationConfirmation(t *testing.T) {
	for _, reply := range []string{"", "no\n", "y\n", "YES\n", strings.Repeat("y", 1025)} {
		t.Run(fmt.Sprint(len(reply), reply[:min(len(reply), 3)]), func(t *testing.T) {
			path, before, _ := locationFixture(t)
			var output bytes.Buffer
			err := setLocationCommand(context.Background(), []string{"--config", path, "--plus-code", "849VCWC8+R9"}, strings.NewReader(reply), &output)
			after, _ := os.ReadFile(path)
			if err == nil || !bytes.Equal(before, after) {
				t.Fatalf("unconfirmed edit: %v", err)
			}
			if !strings.Contains(output.String(), "37.4220625000, -122.0840625000") {
				t.Fatal("preview missing")
			}
		})
	}
	path, _, before := locationFixture(t)
	var output bytes.Buffer
	if err := setLocationCommand(context.Background(), []string{"--config", path, "--plus-code", "849VCWC8+R9"}, strings.NewReader("yes\n"), &output); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if after.Mode().Perm() != 0600 || a.Uid != b.Uid || a.Gid != b.Gid {
		t.Fatal("ownership or mode changed")
	}
	if !strings.Contains(output.String(), "Updated latitude/longitude") {
		t.Fatal("completion missing")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".location-*"))
	if len(files) != 0 {
		t.Fatal("temporary config left behind")
	}
}

func TestSetLocationUnsafeInputs(t *testing.T) {
	path, before, _ := locationFixture(t)
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {"--bad"}, {"--once"}, {"--plus-code", "849VCWC8+R9", "extra"}, {"--config", link, "--plus-code", "849VCWC8+R9"}, {"--config", path + "/", "--plus-code", "849VCWC8+R9"}, {"--config", path, "--plus-code", "invalid"}} {
		if err := setLocationCommand(context.Background(), args, strings.NewReader("yes\n"), io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid arguments modified config")
	}
	if err := setLocationCommand(context.Background(), []string{"--help"}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{"version":1,"version":1}`, `{"unknown":true}`, `{"location":null}`, `{}`, strings.Replace(string(before), `"poll_seconds":900`, `"poll_seconds":1`, 1)} {
		os.WriteFile(path, []byte(data), 0600)
		if err := setLocationCommand(context.Background(), []string{"--config", path, "--plus-code", "849VCWC8+R9"}, strings.NewReader("yes\n"), io.Discard); err == nil {
			t.Fatal("bad config accepted")
		}
		after, _ := os.ReadFile(path)
		if string(after) != data {
			t.Fatal("bad config modified")
		}
	}
	os.WriteFile(path, before, 0600)
	os.Chmod(path, 0644)
	if err := setLocationCommand(context.Background(), []string{"--config", path, "--plus-code", "849VCWC8+R9"}, strings.NewReader("yes\n"), io.Discard); err == nil {
		t.Fatal("public config accepted")
	}
}

type confirmationEdit struct{ edit func() }

func (c confirmationEdit) Read(p []byte) (int, error) { c.edit(); return copy(p, "yes\n"), io.EOF }

func TestSetLocationConcurrentEdit(t *testing.T) {
	for _, edit := range []string{"content", "inode", "mode", "symlink"} {
		t.Run(edit, func(t *testing.T) {
			path, before, _ := locationFixture(t)
			reader := confirmationEdit{edit: func() {
				switch edit {
				case "content":
					os.WriteFile(path, []byte("edited"), 0600)
				case "inode":
					os.WriteFile(path+".new", before, 0600)
					os.Rename(path+".new", path)
				case "mode":
					os.Chmod(path, 0400)
				case "symlink":
					os.Remove(path)
					os.Symlink("missing", path)
				}
			}}
			if err := setLocationCommand(context.Background(), []string{"--config", path, "--plus-code", "849VCWC8+R9"}, reader, io.Discard); err == nil {
				t.Fatal("overwrote concurrent edit")
			}
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".location-*"))
			if len(files) != 0 {
				t.Fatal("temporary config left behind")
			}
			if edit == "content" {
				after, _ := os.ReadFile(path)
				if string(after) != "edited" {
					t.Fatal("lost edit")
				}
			}
		})
	}
}

func TestSetLocationCancelledContext(t *testing.T) {
	path, before, _ := locationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := setLocationCommand(ctx, []string{"--config", path, "--plus-code", "849VCWC8+R9"}, strings.NewReader("yes\n"), io.Discard); err == nil {
		t.Fatal("cancellation ignored")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled command wrote config")
	}
}

type promptNotice chan struct{}

func (n promptNotice) Write(p []byte) (int, error) {
	select {
	case n <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestSetLocationCancellationWhileWaiting(t *testing.T) {
	path, before, _ := locationFixture(t)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := make(promptNotice, 1)
	go func() {
		done <- setLocationCommand(ctx, []string{"--config", path, "--plus-code", "849VCWC8+R9"}, reader, ready)
	}()
	<-ready
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation ignored")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancelled prompt is blocked waiting for input")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("cancellation changed config")
	}
}

func TestPlusCodeMaximumLengths(t *testing.T) {
	for _, text := range []string{"849VCWC8+R9CFGHJ", "849VCWC8+R9" + strings.Repeat(" ", 501)} {
		if _, _, err := parseCopiedCode(text); err != nil {
			t.Fatalf("valid boundary rejected: %v", err)
		}
	}
}

func TestSetLocationExtraArgumentsWithValidConfig(t *testing.T) {
	path, before, _ := locationFixture(t)
	for _, args := range [][]string{{"--config", path, "extra"}, {"--config", path, "--plus-code", "849VCWC8+R9", "extra"}} {
		err := setLocationCommand(context.Background(), args, strings.NewReader("yes\n"), io.Discard)
		if err == nil || err.Error() != "invalid_arguments" {
			t.Fatalf("extra argument: %v", err)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid command modified config")
	}
}

type failedRead struct{}

func (failedRead) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestGeocodingExactResponseLimit(t *testing.T) {
	valid := `{"results":[{"name":"Testville","latitude":37.38605,"longitude":-122.08385}]}`
	for _, size := range []int{1 << 20, (1 << 20) + 1} {
		body := valid + strings.Repeat(" ", size-len(valid))
		client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		got, err := resolvePlusCode(context.Background(), "CWC8+R9 Testville", client)
		if size == 1<<20 {
			if err != nil || got.Code != "849VCWC8+R9" {
				t.Fatalf("bounded valid response: %+v %v", got, err)
			}
		} else if err == nil {
			t.Fatal("oversize valid JSON accepted")
		}
	}
	client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(io.MultiReader(strings.NewReader(valid), failedRead{}))}, nil
	})}
	if _, err := resolvePlusCode(context.Background(), "CWC8+R9 Testville", client); err == nil {
		t.Fatal("body read failure accepted")
	}
}

func TestSetLocationShortCodeCommand(t *testing.T) {
	path, _, _ := locationFixture(t)
	original := http.DefaultTransport
	http.DefaultTransport = testTransport(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Second || time.Until(deadline) < 29*time.Second {
			t.Fatal("missing 30-second lookup deadline")
		}
		select {
		case <-request.Context().Done():
			return nil, request.Context().Err()
		case <-time.After(5 * time.Millisecond):
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":[{"name":"Testville","latitude":37.38605,"longitude":-122.08385}]}`))}, nil
	})
	defer func() { http.DefaultTransport = original }()
	var output bytes.Buffer
	if err := setLocationCommand(context.Background(), []string{"--config", path, "--plus-code", "CWC8+R9 Testville"}, strings.NewReader("yes\n"), &output); err != nil {
		t.Fatal(err)
	}
	var config Config
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &config)
	if math.Abs(*config.Location.Latitude-37.4220625) > 1e-10 || !strings.Contains(output.String(), "Full Plus Code: 849VCWC8+R9") {
		t.Fatal("short code not saved")
	}
}

func TestUnchangedConfigOwnership(t *testing.T) {
	path, before, _ := locationFixture(t)
	info, _ := os.Stat(path)
	owner := info.Sys().(*syscall.Stat_t)
	for _, change := range []string{"uid", "gid"} {
		old := *owner
		if change == "uid" {
			old.Uid++
		} else {
			old.Gid++
		}
		// Exercise ownership comparison independently of inode matching.
		if sameConfigOwner(&old, owner) {
			t.Fatalf("%s change accepted", change)
		}
	}
	if !sameConfigOwner(owner, owner) {
		t.Fatal("unchanged owner rejected")
	}
	if err := unchangedConfig(path, before, info); err != nil {
		t.Fatal(err)
	}
}

func TestSetLocationPreservesGroupReadableConfig(t *testing.T) {
	path, _, owner := locationFixture(t)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if err := setLocationCommand(context.Background(), []string{"--config", path, "--plus-code", "849VCWC8+R9"}, strings.NewReader("yes\n"), io.Discard); err != nil {
		t.Fatalf("group-readable setup failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 || !sameConfigOwner(owner.Sys().(*syscall.Stat_t), info.Sys().(*syscall.Stat_t)) {
		t.Fatal("config group-read or ownership was lost")
	}
	data, _ := os.ReadFile(path)
	var config Config
	json.Unmarshal(data, &config)
	if math.Abs(*config.Location.Latitude-37.4220625) > 1e-10 {
		t.Fatal("coordinates were not updated")
	}
}
