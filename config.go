package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type Location struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	Timezone  string   `json:"timezone"`
}
type Config struct {
	Version     int            `json:"version"`
	Location    Location       `json:"location,omitempty"`
	Locations   []Location     `json:"locations,omitempty"`
	LogLevel    string         `json:"log_level,omitempty"`
	PollSeconds int            `json:"poll_seconds"`
	MariaDBFile string         `json:"mariadb_file"`
	StateDir    string         `json:"state_dir"`
	Database    DatabaseConfig `json:"-"`
}
type DatabaseConfig struct {
	Version         int    `json:"version"`
	Socket          string `json:"socket"`
	Database        string `json:"database"`
	CredentialsFile string `json:"credentials_file"`
	Username        string `json:"-"`
	Password        string `json:"-"`
}

const (
	configUnsafePermissions     os.FileMode = 0037
	credentialUnsafePermissions os.FileMode = 0077
)

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)

func failure(code string) error { return errors.New(code) }
func safePath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return failure("invalid_path")
	}
	for {
		info, err := os.Lstat(absolute)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return failure("symlink_refused")
		}
		if err != nil && !os.IsNotExist(err) {
			return failure("path_unavailable")
		}
		parent := filepath.Dir(absolute)
		if parent == absolute {
			break
		}
		absolute = parent
	}
	return nil
}
func privateDirectory(path string, create bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return failure("invalid_directory")
	}
	if err = safePath(absolute); err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		parent, parentErr := os.Stat(filepath.Dir(absolute))
		if parentErr != nil || !parent.IsDir() || parent.Mode().Perm()&0027 != 0 {
			return failure("private_parent_required")
		}
		if !create {
			return nil
		}
		if os.Mkdir(absolute, 0750) != nil {
			return failure("directory_create_failed")
		}
		if os.Chmod(absolute, 0750) != nil {
			return failure("directory_create_failed")
		}
		info, err = os.Lstat(absolute)
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0027 != 0 {
		return failure("private_directory_required")
	}
	return nil
}
func readProtected(path string, unsafePermissions os.FileMode) ([]byte, error) {
	if err := safePath(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, failure("private_file_unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&unsafePermissions != 0 || info.Size() > 1<<20 {
		return nil, failure("private_file_required")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, failure("private_file_read_failed")
	}
	return data, nil
}
func decodeJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return failure("invalid_json")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return failure("invalid_json")
		}
		token, err := decoder.Token()
		if err != nil || token == nil {
			return failure("invalid_json")
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return failure("invalid_json")
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return failure("invalid_json")
				}
				seen[name] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if walk(0) != nil {
		return failure("invalid_json")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return failure("invalid_json")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return failure("invalid_json")
	}
	return nil
}
func loadJSON(path string, target any, unsafePermissions os.FileMode) error {
	data, err := readProtected(path, unsafePermissions)
	if err != nil {
		return err
	}
	return decodeJSON(data, target)
}
func resolveReference(file, reference string) string {
	if !filepath.IsAbs(reference) {
		return filepath.Join(filepath.Dir(file), reference)
	}
	return reference
}
func loadConfig(filename string) (Config, error) {
	var config Config
	if err := loadJSON(filename, &config, configUnsafePermissions); err != nil {
		return config, err
	}
	if err := validateCollector(config); err != nil {
		return config, err
	}
	config.MariaDBFile = resolveReference(filename, config.MariaDBFile)
	config.StateDir = filepath.Clean(resolveReference(filename, config.StateDir))
	if err := privateDirectory(config.StateDir, false); err != nil {
		return config, err
	}
	if err := loadJSON(config.MariaDBFile, &config.Database, configUnsafePermissions); err != nil {
		return config, err
	}
	database := &config.Database
	if database.Version != 1 || !filepath.IsAbs(database.Socket) || strings.HasSuffix(database.Socket, "/") || !identifier.MatchString(database.Database) || database.CredentialsFile == "" || strings.HasSuffix(database.CredentialsFile, string(filepath.Separator)) {
		return config, failure("invalid_database_configuration")
	}
	database.CredentialsFile = resolveReference(config.MariaDBFile, database.CredentialsFile)
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := loadJSON(database.CredentialsFile, &credentials, credentialUnsafePermissions); err != nil {
		return config, err
	}
	if !identifier.MatchString(credentials.Username) || strings.TrimSpace(credentials.Password) == "" {
		return config, failure("invalid_credentials")
	}
	database.Username = credentials.Username
	database.Password = credentials.Password
	return config, nil
}

func validReference(value *float64, limit float64) bool {
	return validNumber(value) && math.Abs(*value) <= limit
}
func validLocationIdentity(location Location) bool {
	_, err := time.LoadLocation(location.Timezone)
	return identifier.MatchString(location.ID) && strings.TrimSpace(location.Name) != "" && len(location.Name) <= 128 && location.Timezone != "" && location.Timezone != "Local" && err == nil
}
func (config Config) locations() []Location {
	if config.Version == 2 {
		return config.Locations
	}
	return []Location{config.Location}
}
func validCollectorHeader(config Config) bool {
	return (config.Version == 1 || config.Version == 2) && config.PollSeconds == 900 && config.MariaDBFile != "" && !strings.HasSuffix(config.MariaDBFile, string(filepath.Separator)) && config.StateDir != "" && validLogLevel(config.LogLevel)
}
func validLogLevel(level string) bool { return level == "" || level == "info" || level == "debug" }
func validLocationRepresentation(config Config) bool {
	if config.Version == 1 {
		return config.Locations == nil
	}
	return config.Location == (Location{}) && len(config.Locations) > 0
}
func validateCollector(config Config) error {
	if !validCollectorHeader(config) || !validLocationRepresentation(config) {
		return failure("invalid_configuration")
	}
	seen := map[string]bool{}
	for _, location := range config.locations() {
		if !validLocationIdentity(location) || !validReference(location.Latitude, 90) || !validReference(location.Longitude, 180) || seen[location.ID] {
			return failure("invalid_configuration")
		}
		seen[location.ID] = true
	}
	return nil
}

func (config *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var value plain
	if err := decodeJSON(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return failure("invalid_json")
	}
	if value.Version == 2 {
		if _, ok := fields["location"]; ok {
			return failure("invalid_configuration")
		}
	}
	*config = Config(value)
	return nil
}
