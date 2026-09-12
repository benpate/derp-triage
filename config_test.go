package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolate points the configuration search path at empty temporary directories, so a test can
// never read (or be influenced by) the real configuration on this machine.
func isolate(t *testing.T) string {

	t.Helper()

	home := t.TempDir()

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(configVariable, "")
	t.Chdir(t.TempDir())

	return home
}

// writeFile writes a file and returns its path
func writeFile(t *testing.T, directory string, name string, contents string) string {

	t.Helper()

	path := filepath.Join(directory, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0600))

	return path
}

func TestNewConfig_Defaults(t *testing.T) {

	isolate(t)
	config := NewConfig()

	assert.Equal(t, defaultScanLimit, config.ScanLimit)
	assert.NotEmpty(t, config.StateFile)
	assert.False(t, config.IncludeAll)
	assert.False(t, config.IncludeDecided)
	assert.False(t, config.IncludeDuplicates)
}

func TestDefaultStateFile_LivesWithTheConfiguration(t *testing.T) {

	isolate(t)
	path := DefaultStateFile()

	assert.Contains(t, path, configDirectory)
	assert.True(t, strings.HasSuffix(path, ".json"))

	configured, err := os.UserConfigDir()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(path, configured), "decisions belong beside the configuration")

	// RULE: decisions outlive the records they describe.  A cache directory that the system
	// clears would silently resurrect work that was already finished.
	if cached, err := os.UserCacheDir(); err == nil {
		assert.False(t, strings.HasPrefix(path, cached), "decisions are not a cache")
	}
}

func TestSearchPath_Order(t *testing.T) {

	isolate(t)
	t.Setenv(configVariable, "/named/by/environment.json")

	paths := SearchPath()

	require.Len(t, paths, 3)
	assert.Equal(t, "/named/by/environment.json", paths[0], "the environment variable is tried first")
	assert.Equal(t, DefaultConfigFile(), paths[1], "then the working directory")
	assert.Contains(t, paths[2], configDirectory, "then the user configuration directory")
}

func TestSearchPath_WithoutTheEnvironmentVariable(t *testing.T) {

	isolate(t)

	paths := SearchPath()

	require.Len(t, paths, 2)
	assert.Equal(t, DefaultConfigFile(), paths[0])
}

func TestLoadConfig_NamedFile(t *testing.T) {

	isolate(t)
	directory := t.TempDir()

	path := writeFile(t, directory, "elsewhere.json", `{
		"connectString": "mongodb://named",
		"database": "Named",
		"stateFile": "/tmp/named.json",
		"scanLimit": 42,
		"includeAll": true
	}`)

	config, err := LoadConfig(path)

	require.NoError(t, err)
	assert.Equal(t, "mongodb://named", config.ConnectString)
	assert.Equal(t, "Named", config.Database)
	assert.Equal(t, "/tmp/named.json", config.StateFile)
	assert.Equal(t, int64(42), config.ScanLimit)
	assert.True(t, config.IncludeAll)
	assert.Equal(t, path, config.Source)
}

func TestLoadConfig_NamedFileMustExist(t *testing.T) {

	isolate(t)

	// RULE: falling back to another file would point a delete at a database nobody asked for
	_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	assert.Error(t, err)
}

func TestLoadConfig_FindsTheWorkingDirectory(t *testing.T) {

	isolate(t)
	writeFile(t, ".", configFileName, `{"connectString":"mongodb://here","database":"Here"}`)

	config, err := LoadConfig("")

	require.NoError(t, err)
	assert.Equal(t, "mongodb://here", config.ConnectString)
	assert.Equal(t, defaultScanLimit, config.ScanLimit, "an absent key keeps its default")
}

func TestLoadConfig_EnvironmentVariableWins(t *testing.T) {

	isolate(t)
	writeFile(t, ".", configFileName, `{"connectString":"mongodb://here","database":"Here"}`)

	named := writeFile(t, t.TempDir(), "named.json", `{"connectString":"mongodb://named","database":"Named"}`)
	t.Setenv(configVariable, named)

	config, err := LoadConfig("")

	require.NoError(t, err)
	assert.Equal(t, "mongodb://named", config.ConnectString)
}

func TestLoadConfig_FindsTheUserDirectory(t *testing.T) {

	home := isolate(t)
	directory := filepath.Join(home, ".config", configDirectory)

	if configured, err := os.UserConfigDir(); err == nil {
		directory = filepath.Join(configured, configDirectory)
	}

	writeFile(t, directory, configFileName, `{"connectString":"mongodb://user","database":"User"}`)

	config, err := LoadConfig("")

	require.NoError(t, err)
	assert.Equal(t, "mongodb://user", config.ConnectString)
}

func TestLoadConfig_NoDatabaseAnywhere(t *testing.T) {

	isolate(t)

	_, err := LoadConfig("")
	assert.Error(t, err, "a configuration with nothing to connect to must be rejected")
}

func TestLoadConfig_NeverReadsEmissarysConfiguration(t *testing.T) {

	// RULE: the connection comes from the triage configuration and nowhere else
	isolate(t)

	writeFile(t, ".", "config.json", `{"activityPubCache":{"connectString":"mongodb://emissary","database":"Common"}}`)

	_, err := LoadConfig("")
	assert.Error(t, err, "a database beside the working directory must not be adopted")

	writeFile(t, ".", configFileName, `{"database":"Production"}`)

	_, err = LoadConfig("")
	assert.Error(t, err, "a half-written connection must not be completed from somewhere else")
}

func TestLoadConfig_Malformed(t *testing.T) {

	isolate(t)
	path := writeFile(t, t.TempDir(), "broken.json", "this is not json")

	_, err := LoadConfig(path)
	assert.Error(t, err)
}

func TestConfig_Validate(t *testing.T) {

	isolate(t)

	valid := Config{ConnectString: "mongodb://x", Database: "X", StateFile: "/tmp/x.json", ScanLimit: 10}
	require.NoError(t, valid.Validate())

	tests := map[string]Config{
		"no connect string": {Database: "X", StateFile: "/tmp/x.json", ScanLimit: 10},
		"no database":       {ConnectString: "mongodb://x", StateFile: "/tmp/x.json", ScanLimit: 10},
		"no state file":     {ConnectString: "mongodb://x", Database: "X", ScanLimit: 10},
		"zero limit":        {ConnectString: "mongodb://x", Database: "X", StateFile: "/tmp/x.json", ScanLimit: 0},
		"negative limit":    {ConnectString: "mongodb://x", Database: "X", StateFile: "/tmp/x.json", ScanLimit: -1},
		"limit too large":   {ConnectString: "mongodb://x", Database: "X", StateFile: "/tmp/x.json", ScanLimit: maximumScanLimit + 1},
	}

	for name, config := range tests {
		assert.Error(t, config.Validate(), "a configuration with %s must be rejected", name)
	}
}

func TestDefaultConfigFile_IsTheWorkingDirectory(t *testing.T) {

	// RULE: `triage init` writes where it is run, so each checkout carries its own settings
	isolate(t)

	assert.Equal(t, filepath.Join(".", configFileName), DefaultConfigFile())
	assert.NotEqual(t, UserConfigFile(), DefaultConfigFile())
}

func TestConfig_ValidateNamesTheFileToEdit(t *testing.T) {

	// Telling somebody to run `triage init` is useless once the file it would write exists
	fromFile := Config{Source: "/somewhere/triage.json", StateFile: "/tmp/x.json", ScanLimit: 10}
	fileError := fromFile.Validate()

	require.Error(t, fileError)
	assert.Contains(t, fileError.Error(), "/somewhere/triage.json")

	fromDefaults := Config{Source: sourceDefaults, StateFile: "/tmp/x.json", ScanLimit: 10}
	defaultsError := fromDefaults.Validate()

	require.Error(t, defaultsError)
	assert.Contains(t, defaultsError.Error(), "triage init")
}
