package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newConfiguredFlagSet builds a flag set carrying every flag that can override the configuration
func newConfiguredFlagSet(t *testing.T) (*flag.FlagSet, *commandFlags) {

	t.Helper()

	flags, typed := newFlagSet("test")
	flags.SetOutput(discard{})
	addSelectorFlags(flags, typed)
	addScanFlag(flags, typed)

	return flags, typed
}

func TestLoadConfig_ReadsTheNamedFile(t *testing.T) {

	isolate(t)
	path := writeFile(t, t.TempDir(), configFileName, `{"connectString":"mongodb://x","database":"X","scanLimit":11}`)

	flags, typed := newConfiguredFlagSet(t)
	_, err := parseArguments(flags, []string{"-config", path})
	require.NoError(t, err)

	config, err := loadConfig(flags, *typed)

	require.NoError(t, err)
	assert.Equal(t, "mongodb://x", config.ConnectString)
	assert.Equal(t, int64(11), config.ScanLimit)
}

func TestLoadConfig_FlagsOverrideTheFile(t *testing.T) {

	isolate(t)
	path := writeFile(t, t.TempDir(), configFileName, `{"connectString":"mongodb://x","database":"X","scanLimit":11}`)

	flags, typed := newConfiguredFlagSet(t)
	_, err := parseArguments(flags, []string{"-config", path, "-n", "7", "-all", "-duplicates"})
	require.NoError(t, err)

	config, err := loadConfig(flags, *typed)

	require.NoError(t, err)
	assert.Equal(t, int64(7), config.ScanLimit)
	assert.True(t, config.IncludeAll)
	assert.True(t, config.IncludeDuplicates)
	assert.False(t, config.IncludeDecided, "a flag that was not typed changes nothing")
}

func TestLoadConfig_UntypedFlagNeverUndoesTheFile(t *testing.T) {

	isolate(t)

	// RULE: every boolean flag defaults to false, which is also a legitimate configured value.
	// Only the flags actually typed may override the file.
	path := writeFile(t, t.TempDir(), configFileName, `{
		"connectString":"mongodb://x", "database":"X",
		"includeAll": true, "includeDecided": true, "includeDuplicates": true
	}`)

	flags, typed := newConfiguredFlagSet(t)
	_, err := parseArguments(flags, []string{"-config", path})
	require.NoError(t, err)

	config, err := loadConfig(flags, *typed)

	require.NoError(t, err)
	assert.True(t, config.IncludeAll)
	assert.True(t, config.IncludeDecided)
	assert.True(t, config.IncludeDuplicates)
}

func TestLoadConfig_FlagCanTurnAConfiguredValueOff(t *testing.T) {

	isolate(t)
	path := writeFile(t, t.TempDir(), configFileName, `{"connectString":"mongodb://x","database":"X","includeAll":true}`)

	flags, typed := newConfiguredFlagSet(t)
	_, err := parseArguments(flags, []string{"-config", path, "-all=false"})
	require.NoError(t, err)

	config, err := loadConfig(flags, *typed)

	require.NoError(t, err)
	assert.False(t, config.IncludeAll)
}

func TestLoadConfig_RejectsAFlagThatBreaksTheConfiguration(t *testing.T) {

	isolate(t)
	path := writeFile(t, t.TempDir(), configFileName, `{"connectString":"mongodb://x","database":"X"}`)

	for _, limit := range []string{"0", "-5", "100001"} {

		flags, typed := newConfiguredFlagSet(t)
		_, err := parseArguments(flags, []string{"-config", path, "-n", limit})
		require.NoError(t, err)

		_, err = loadConfig(flags, *typed)
		assert.Error(t, err, "a scan limit of %s must be rejected", limit)
	}
}

func TestLoadConfig_ReportsAMissingFile(t *testing.T) {

	isolate(t)

	flags, typed := newConfiguredFlagSet(t)
	_, err := parseArguments(flags, []string{"-config", filepath.Join(t.TempDir(), "absent.json")})
	require.NoError(t, err)

	_, err = loadConfig(flags, *typed)
	assert.Error(t, err)
}

func TestParseArguments_FlagsOnEitherSide(t *testing.T) {

	// Go stops parsing at the first non-flag word, so a record ID typed first would otherwise
	// swallow every flag after it.
	tests := map[string][]string{
		"flags first": {"-note", "hello", "6aa17bd9a102749e24915ab4"},
		"flags last":  {"6aa17bd9a102749e24915ab4", "-note", "hello"},
		"surrounded":  {"-dry-run", "6aa17bd9a102749e24915ab4", "-note", "hello"},
	}

	for name, arguments := range tests {

		flags, typed := newFlagSet("test")
		flags.StringVar(&typed.note, "note", "", "")
		flags.BoolVar(&typed.dryRun, "dry-run", false, "")

		positional, err := parseArguments(flags, arguments)

		require.NoError(t, err, "parsing %s", name)
		assert.Equal(t, []string{"6aa17bd9a102749e24915ab4"}, positional, "parsing %s", name)
		assert.Equal(t, "hello", typed.note, "parsing %s", name)
	}
}

func TestParseArguments_NoArguments(t *testing.T) {

	flags, _ := newFlagSet("test")
	positional, err := parseArguments(flags, nil)

	require.NoError(t, err)
	assert.Empty(t, positional)
}

func TestParseArguments_SeveralPositionals(t *testing.T) {

	flags, _ := newFlagSet("test")
	positional, err := parseArguments(flags, []string{"one", "two", "three"})

	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two", "three"}, positional)
}

func TestParseArguments_RejectsUnknownFlags(t *testing.T) {

	flags, _ := newFlagSet("test")
	flags.SetOutput(discard{})

	_, err := parseArguments(flags, []string{"-nonsense"})
	assert.Error(t, err)
}

func TestParseArguments_HelpIsNotAFailure(t *testing.T) {

	flags, _ := newFlagSet("test")
	flags.SetOutput(discard{})

	positional, err := parseArguments(flags, []string{"-h"})

	require.NoError(t, err)
	assert.Empty(t, positional)
}

func TestNewFlagSet_TakesNoConnectionSettings(t *testing.T) {

	// RULE: where to look is configuration, never a command-line parameter
	flags, _ := newFlagSet("test")

	for _, removed := range []string{"uri", "db", "state"} {
		assert.Nil(t, flags.Lookup(removed), "-%s must not be a flag", removed)
	}

	assert.NotNil(t, flags.Lookup("config"))
}

// discard swallows the usage text that the flag package writes on a parse error
type discard struct{}

// Write implements io.Writer, and keeps test output readable
func (discard) Write(value []byte) (int, error) {
	return len(value), nil
}

func TestWriteConfig(t *testing.T) {

	path := filepath.Join(t.TempDir(), "nested", configFileName)
	config := Config{ConnectString: "mongodb://x", Database: "X", StateFile: "/tmp/x.json", ScanLimit: 10}

	require.NoError(t, writeConfig(path, config))

	// RULE: a connection string can carry a password, so the file is never world-readable
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	reloaded, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "mongodb://x", reloaded.ConnectString)
	assert.Equal(t, "X", reloaded.Database)
}

func TestRunInit(t *testing.T) {

	// The trap this closes: a silently empty file that no command can use, and that `init`
	// then refuses to overwrite on the second try.
	isolate(t)
	path := filepath.Join(t.TempDir(), configFileName)

	contents, err := capture(t, func() error { return runInit([]string{"-config", path}) })

	require.NoError(t, err)
	assert.Contains(t, contents, "EMPTY", "a starter configuration must say that it is unusable")

	_, loadError := LoadConfig(path)
	assert.Error(t, loadError, "and the file it wrote is rejected until somebody fills it in")
}

func TestRunInit_WritesToTheWorkingDirectoryByDefault(t *testing.T) {

	isolate(t)

	contents, err := capture(t, func() error { return runInit(nil) })
	require.NoError(t, err)

	assert.Contains(t, contents, DefaultConfigFile())

	_, statError := os.Stat(DefaultConfigFile())
	require.NoError(t, statError, "the file lands where `triage init` was run")
}

func TestRunInit_NeverOverwrites(t *testing.T) {

	isolate(t)
	path := writeFile(t, t.TempDir(), configFileName, `{"connectString":"mongodb://production","database":"Production"}`)

	// RULE: the file it would replace may be the one pointing at production
	_, err := capture(t, func() error { return runInit([]string{"-config", path}) })
	assert.Error(t, err)

	contents, readError := os.ReadFile(path)
	require.NoError(t, readError)
	assert.Contains(t, string(contents), "mongodb://production")
}

func TestRunConfig(t *testing.T) {

	isolate(t)
	path := writeFile(t, t.TempDir(), configFileName, `{"connectString":"mongodb://x","database":"X"}`)

	contents, err := capture(t, func() error { return runConfig([]string{"-config", path}) })

	require.NoError(t, err)
	assert.Contains(t, contents, "mongodb://x")
	assert.Contains(t, contents, `"source"`, "the report says which file it read")
	assert.Contains(t, contents, `"searchPath"`)
}

func TestPrintJSON(t *testing.T) {

	assert.NoError(t, printJSON(Report{Fingerprint: "abc"}))
	assert.NoError(t, printJSON(nil))

	// A channel cannot be rendered as JSON
	assert.Error(t, printJSON(make(chan int)))
}

func TestRun_UnknownCommand(t *testing.T) {

	flag.CommandLine.SetOutput(discard{})

	err := run(t.Context(), "nonsense", nil)
	assert.Error(t, err)
}

func TestRun_Help(t *testing.T) {
	assert.NoError(t, run(t.Context(), "help", nil))
}

func TestRun_RoutesEveryCommand(t *testing.T) {

	isolate(t)

	// Every command name must reach its handler.  Naming a configuration file that does not
	// exist makes them fail for that reason, rather than as an unknown command.
	arguments := []string{"-config", filepath.Join(t.TempDir(), "absent.json")}

	commands := map[string][]string{
		"next":      arguments,
		"latest":    arguments,
		"show":      append([]string{"6aa17bd9a102749e24915ab4"}, arguments...),
		"watch":     arguments,
		"fixed":     append([]string{"6aa17bd9a102749e24915ab4"}, arguments...),
		"ignore":    append([]string{"6aa17bd9a102749e24915ab4"}, arguments...),
		"forget":    append([]string{"aaaaaaaaaaaa"}, arguments...),
		"decisions": arguments,
		"config":    arguments,
	}

	for command, commandArguments := range commands {

		err := run(t.Context(), command, commandArguments)

		require.Error(t, err, "%q must reach a handler that reports the missing configuration", command)
		assert.NotContains(t, err.Error(), "Unknown command", "%q must be a known command", command)
	}
}
