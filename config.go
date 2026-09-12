package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/benpate/derp"
)

// collectionName is the MongoDB collection that tools/derp-mongo writes error reports into
const collectionName = "ErrorLog"

// configFileName is the file every command reads its settings from
const configFileName = "triage.json"

// configDirectory is where a configuration file lives when it is not in the working directory
const configDirectory = "emissary-triage"

// configVariable names the environment variable that overrides the search path
const configVariable = "TRIAGE_CONFIG"

// sourceDefaults marks a configuration that no file contributed to
const sourceDefaults = "Defaults"

// Limits that bound how much of the error log a single command reads
const (
	defaultScanLimit = int64(500)
	maximumScanLimit = int64(100000)
)

// Config is the resolved triage configuration: a configuration file, with defaults applied
// and the connection settings filled in.  It is read once, and never changed afterwards.
type Config struct {
	ConnectString     string `json:"connectString"`     // MongoDB URI for the database holding the error log
	Database          string `json:"database"`          // Name of that database
	StateFile         string `json:"stateFile"`         // File recording which errors have been fixed or ignored
	ScanLimit         int64  `json:"scanLimit"`         // How many records a command reads from the log
	IncludeAll        bool   `json:"includeAll"`        // TRUE to report errors that a remote server caused
	IncludeDecided    bool   `json:"includeDecided"`    // TRUE to report errors already fixed or ignored
	IncludeDuplicates bool   `json:"includeDuplicates"` // TRUE to report every occurrence, not one per error

	Source string `json:"-"` // READONLY: where this configuration was read from
}

// NewConfig returns a Config carrying the built-in defaults
func NewConfig() Config {
	return Config{
		StateFile: DefaultStateFile(),
		ScanLimit: defaultScanLimit,
		Source:    sourceDefaults,
	}
}

// SearchPath returns the configuration file locations that are tried, in order.  A path named
// on the command line is not part of it: that path is used, or the command fails.
func SearchPath() []string {

	paths := make([]string, 0, 3)

	if named := os.Getenv(configVariable); named != "" {
		paths = append(paths, named)
	}

	paths = append(paths, DefaultConfigFile())

	if shared := UserConfigFile(); shared != "" {
		paths = append(paths, shared)
	}

	return paths
}

// DefaultConfigFile returns where `triage init` writes a new configuration file.  It is the
// working directory, so each checkout or deployment carries the settings for its own database.
func DefaultConfigFile() string {
	return filepath.Join(".", configFileName)
}

// UserConfigFile returns the machine-wide configuration file, which is the last place searched
func UserConfigFile() string {

	directory, err := os.UserConfigDir()

	if err != nil {
		return ""
	}

	return filepath.Join(directory, configDirectory, configFileName)
}

// DefaultStateFile returns where triage records the errors it has already dealt with.
// RULE: this is a CONFIG directory, never a cache.  Decisions outlive the records they
// describe, and a cache that the system clears would silently resurrect finished work.
func DefaultStateFile() string {

	directory, err := os.UserConfigDir()

	if err != nil {
		return filepath.Join(".", "emissary-triage-decisions.json")
	}

	return filepath.Join(directory, configDirectory, "decisions.json")
}

// LoadConfig reads the triage configuration.  An empty path searches the standard locations,
// and falls back to the built-in defaults when none of them holds a file.
func LoadConfig(path string) (Config, error) {

	const location = "triage.LoadConfig"

	config, err := readConfig(path)

	if err != nil {
		return Config{}, derp.Wrap(err, location, "Reading configuration")
	}

	if err := config.Validate(); err != nil {
		return Config{}, derp.Wrap(err, location, "Invalid configuration", config.Source)
	}

	return config, nil
}

// readConfig loads a configuration file over the built-in defaults, without resolving anything
func readConfig(path string) (Config, error) {

	const location = "triage.readConfig"

	// RULE: A path named on the command line must exist.  Silently falling back to another
	// file would point a delete at a database the caller did not ask for.
	if path != "" {
		return decodeConfig(path)
	}

	for _, candidate := range SearchPath() {

		if _, err := os.Stat(candidate); err != nil {
			continue
		}

		config, err := decodeConfig(candidate)

		if err != nil {
			return Config{}, derp.Wrap(err, location, "Reading configuration file", candidate)
		}

		return config, nil
	}

	// No configuration file anywhere, so the defaults have to carry it
	return NewConfig(), nil
}

// decodeConfig reads one configuration file over the built-in defaults
func decodeConfig(path string) (Config, error) {

	const location = "triage.decodeConfig"

	// The path comes from a flag or a configuration file, both typed by an operator who
	// already has a shell on this machine, so it is not untrusted input.
	// #nosec G304
	contents, err := os.ReadFile(path)

	if err != nil {
		return Config{}, derp.NotFound(location, "Reading configuration file", path, err.Error())
	}

	config := NewConfig()

	if err := json.Unmarshal(contents, &config); err != nil {
		return Config{}, derp.BadRequest(location, "Parsing configuration file", path, err.Error())
	}

	config.Source = path

	return config, nil
}

// Validate reports whether a configuration can actually be used
func (config Config) Validate() error {

	const location = "triage.Config.Validate"

	if config.ConnectString == "" {
		return derp.BadRequest(location, "No database connection.  "+config.advice("connectString"))
	}

	if config.Database == "" {
		return derp.BadRequest(location, "No database name.  "+config.advice("database"))
	}

	if config.StateFile == "" {
		return derp.BadRequest(location, "No state file.  Set stateFile to where decisions should be recorded")
	}

	// RULE: The scan limit bounds a database query, so it must be a sane number
	if config.ScanLimit < 1 || config.ScanLimit > maximumScanLimit {
		return derp.BadRequest(location, "scanLimit must be between 1 and 100000", config.ScanLimit)
	}

	// Configuration achieved
	return nil
}

// advice names the file a caller should edit, which is more useful than telling somebody to
// run `triage init` when the file it would write is already there.
func (config Config) advice(key string) string {

	if config.Source == "" || config.Source == sourceDefaults {
		return "Run `triage init` to write a configuration file, then set " + key
	}

	return "Set " + key + " in " + config.Source
}
