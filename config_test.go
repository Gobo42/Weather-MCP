package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func configurationFixture(testingContext *testing.T) string {
	testingContext.Helper()
	root := testingContext.TempDir()
	os.Chmod(root, 0700)
	files := map[string]any{
		"collector.json":   map[string]any{"version": 1, "location": map[string]any{"id": "home", "name": "Example", "latitude": 40.0, "longitude": -105.0, "timezone": "America/Los_Angeles"}, "poll_seconds": 900, "mariadb_file": "database.json", "state_dir": "state///"},
		"database.json":    map[string]any{"version": 1, "socket": "/run/mysqld/mysqld.sock", "database": "weatheragent", "credentials_file": "credentials.json"},
		"credentials.json": map[string]any{"username": "weatheragent", "password": "test-only-password"},
	}
	for name, value := range files {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			testingContext.Fatal(err)
		}
	}
	return filepath.Join(root, "collector.json")
}

func TestConfigStrictPrivateFiles(testingContext *testing.T) {
	file := configurationFixture(testingContext)
	config, err := loadConfig(file)
	if err != nil || config.PollSeconds != 900 || config.StateDir != filepath.Join(filepath.Dir(file), "state") {
		testingContext.Fatalf("config=%+v err=%v", config, err)
	}
	original, _ := os.ReadFile(file)
	for _, data := range []string{strings.Replace(string(original), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(original), `"version":1`, `"version":1,"extra":true`, 1), string(original) + ` {}`, strings.Replace(string(original), `"latitude":40`, `"latitude":null`, 1), strings.Replace(string(original), `"poll_seconds":900`, `"poll_seconds":1`, 1)} {
		os.WriteFile(file, []byte(data), 0600)
		if _, err = loadConfig(file); err == nil {
			testingContext.Fatal("accepted invalid configuration")
		}
	}
	os.WriteFile(file, original, 0600)
	os.Chmod(file, 0644)
	if _, err = loadConfig(file); err == nil {
		testingContext.Fatal("accepted public config")
	}
}

func TestPrivatePaths(testingContext *testing.T) {
	root := testingContext.TempDir()
	os.Chmod(root, 0700)
	for _, suffix := range []string{"", "/", "///"} {
		path := filepath.Join(root, "state") + suffix
		if err := privateDirectory(path, true); err != nil {
			testingContext.Fatal(err)
		}
		if err := privateDirectory(path, false); err != nil {
			testingContext.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	os.Symlink(filepath.Join(root, "state"), link)
	if err := privateDirectory(link+"///", true); err == nil {
		testingContext.Fatal("followed directory symlink")
	}
	if err := privateDirectory("/", true); err == nil {
		testingContext.Fatal("accepted public filesystem root")
	}
	if err := privateDirectory(filepath.Join(root, "absent", "child"), true); err == nil {
		testingContext.Fatal("created missing ancestors")
	}
	previous, _ := os.Getwd()
	defer os.Chdir(previous)
	os.Chdir(root)
	if err := privateDirectory("relative///", true); err != nil {
		testingContext.Fatal(err)
	}
	file := filepath.Join(root, "plain")
	os.WriteFile(file, []byte("{}"), 0600)
	if _, err := readProtected(file+"/", credentialUnsafePermissions); err == nil {
		testingContext.Fatal("normalized file separator")
	}
	if err := privateDirectory(file, true); err == nil {
		testingContext.Fatal("replaced file")
	}
}

func TestConfigFileReferenceSeparators(testingContext *testing.T) {
	file := configurationFixture(testingContext)
	original, _ := os.ReadFile(file)
	os.WriteFile(file, []byte(strings.Replace(string(original), `"database.json"`, `"database.json///"`, 1)), 0600)
	if _, err := loadConfig(file); err == nil {
		testingContext.Fatal("normalized a database file reference with trailing separators")
	}
	os.WriteFile(file, original, 0600)
	databaseFile := filepath.Join(filepath.Dir(file), "database.json")
	data, _ := os.ReadFile(databaseFile)
	os.WriteFile(databaseFile, []byte(strings.Replace(string(data), `"credentials.json"`, `"credentials.json///"`, 1)), 0600)
	if _, err := loadConfig(file); err == nil {
		testingContext.Fatal("normalized a credential file reference with trailing separators")
	}
}

func TestGroupReadableApplicationDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0750); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0077)
	defer syscall.Umask(old)
	path := filepath.Join(root, "state")
	if err := privateDirectory(path+"///", true); err != nil {
		t.Fatalf("0750 application parent refused: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatalf("new application directory must be 0750: %v %v", info, err)
	}
	for _, mode := range []os.FileMode{0700, 0750} {
		os.Chmod(path, mode)
		if err := privateDirectory(path, false); err != nil {
			t.Fatalf("compatible directory mode %o refused: %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0770, 0755} {
		os.Chmod(path, mode)
		if err := privateDirectory(path, false); err == nil {
			t.Fatalf("unsafe mode %o accepted", mode)
		}
	}
}

func TestGroupReadableConfigsKeepCredentialsPrivate(t *testing.T) {
	filename := configurationFixture(t)
	database := filepath.Join(filepath.Dir(filename), "database.json")
	credentials := filepath.Join(filepath.Dir(filename), "credentials.json")
	for _, path := range []string{filename, database} {
		if err := os.Chmod(path, 0640); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loadConfig(filename); err != nil {
		t.Fatalf("group-readable non-secret config refused: %v", err)
	}
	for _, path := range []string{filename, database} {
		for _, mode := range []os.FileMode{0660, 0650, 0644} {
			os.Chmod(path, mode)
			if _, err := loadConfig(filename); err == nil {
				t.Fatalf("unsafe mode %o accepted for %s", mode, path)
			}
		}
		os.Chmod(path, 0640)
	}
	os.Chmod(credentials, 0640)
	if _, err := loadConfig(filename); err == nil {
		t.Fatal("group-readable credentials accepted")
	}
	os.Chmod(credentials, 0600)
	if _, err := loadConfig(filename); err != nil {
		t.Fatal(err)
	}
}
