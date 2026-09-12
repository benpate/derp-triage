/******************************************
 * Command Line
 *
 * The subcommands, their flags, and the JSON each one
 * prints.  WHERE to look is configuration; these flags
 * are per-invocation choices only, and every command
 * writes to stdout so its output can be piped onward.
 ******************************************/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	derpconsole "github.com/EmissarySocial/emissary/tools/derp-console"
	"github.com/benpate/derp"
	"go.mongodb.org/mongo-driver/bson"
)

// main dispatches to a subcommand and reports whatever goes wrong
func main() {

	// Errors from this tool are for a human (or an agent) to read, not to store
	derp.SetPlugins(derpconsole.New())

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// A change stream runs until it is interrupted, so every command honors Ctrl-C
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		derp.Report(err)
		os.Exit(1)
	}
}

// run executes a single subcommand
func run(ctx context.Context, command string, arguments []string) error {

	const location = "triage.run"

	switch command {

	case "next":
		return runNext(ctx, arguments)

	case "latest":
		return runLatest(ctx, arguments)

	case "show":
		return runShow(ctx, arguments)

	case "watch":
		return runWatch(ctx, arguments)

	case "fixed":
		return runDecide(ctx, arguments, DispositionFixed)

	case "ignore":
		return runDecide(ctx, arguments, DispositionIgnored)

	case "decisions":
		return runDecisions(arguments)

	case "forget":
		return runForget(arguments)

	case "config":
		return runConfig(arguments)

	case "init":
		return runInit(arguments)

	case "help", "-h", "--help":
		usage()
		return nil
	}

	usage()

	return derp.BadRequest(location, "Unknown command", command)
}

// commandFlags collects what a subcommand accepts on the command line.  Everything describing
// WHERE to look lives in the configuration file; these are per-invocation choices only.
type commandFlags struct {
	configPath        string
	note              string
	limit             int64
	oldest            bool
	includeAll        bool
	includeDecided    bool
	includeDuplicates bool
	dryRun            bool
}

// newFlagSet builds the flag set for a subcommand, wired to a shared commandFlags struct
func newFlagSet(name string) (*flag.FlagSet, *commandFlags) {

	typed := commandFlags{}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)

	flags.StringVar(&typed.configPath, "config", "", "Configuration file to read, instead of searching for one")

	return flags, &typed
}

// addSelectorFlags adds the flags that override which errors a subcommand prints
func addSelectorFlags(flags *flag.FlagSet, typed *commandFlags) {
	flags.BoolVar(&typed.includeAll, "all", false, "Include errors caused by a remote server (overrides includeAll)")
	flags.BoolVar(&typed.includeDecided, "decided", false, "Include errors already fixed or ignored (overrides includeDecided)")
	flags.BoolVar(&typed.includeDuplicates, "duplicates", false, "Print every occurrence (overrides includeDuplicates)")
}

// addScanFlag adds the flag that overrides how much of the error log is read
func addScanFlag(flags *flag.FlagSet, typed *commandFlags) {
	flags.Int64Var(&typed.limit, "n", 0, "How many records to read (overrides scanLimit)")
}

// parseArguments reads the flags, allowing them on either side of the positional arguments.
// Go's flag package stops at the first non-flag word, so parsing resumes after each one.
func parseArguments(flags *flag.FlagSet, arguments []string) ([]string, error) {

	const location = "triage.parseArguments"

	positional := make([]string, 0, len(arguments))

	for len(arguments) > 0 {

		if err := flags.Parse(arguments); err != nil {

			// A request for help is not a failure
			if errors.Is(err, flag.ErrHelp) {
				return nil, nil
			}

			return nil, derp.BadRequest(location, "Invalid arguments", err.Error())
		}

		arguments = flags.Args()

		if len(arguments) == 0 {
			break
		}

		positional = append(positional, arguments[0])
		arguments = arguments[1:]
	}

	return positional, nil
}

// loadConfig reads the configuration file, then applies the flags the caller actually typed
func loadConfig(flags *flag.FlagSet, typed commandFlags) (Config, error) {

	const location = "triage.loadConfig"

	config, err := LoadConfig(typed.configPath)

	if err != nil {
		return Config{}, derp.Wrap(err, location, "Loading configuration")
	}

	// RULE: Visit reports ONLY the flags that were typed, so a configured `true` is never
	// undone by a flag sitting at its own default.
	flags.Visit(func(entry *flag.Flag) {

		switch entry.Name {

		case "n":
			config.ScanLimit = typed.limit

		case "all":
			config.IncludeAll = typed.includeAll

		case "decided":
			config.IncludeDecided = typed.includeDecided

		case "duplicates":
			config.IncludeDuplicates = typed.includeDuplicates
		}
	})

	// A flag can make a valid configuration invalid, so the result is checked again
	if err := config.Validate(); err != nil {
		return Config{}, derp.Wrap(err, location, "Invalid configuration", config.Source)
	}

	return config, nil
}

// Backlog answers "what should I work on, and how much is left?"
type Backlog struct {
	Scanned   int     `json:"scanned"`
	Remaining int     `json:"remaining"`
	Next      *Report `json:"next"`
}

// runNext prints a single error to work on, plus how many distinct errors are still waiting
func runNext(ctx context.Context, arguments []string) error {

	const location = "triage.runNext"

	flags, typed := newFlagSet("next")
	flags.BoolVar(&typed.includeAll, "all", false, "Include errors caused by a remote server (overrides includeAll)")
	flags.BoolVar(&typed.oldest, "oldest", false, "Work the backlog from the oldest error forward")
	addScanFlag(flags, typed)

	if _, err := parseArguments(flags, arguments); err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	reports, scanned, err := collect(ctx, config)

	if err != nil {
		return derp.Wrap(err, location, "Collecting errors")
	}

	backlog := Backlog{
		Scanned:   scanned,
		Remaining: len(reports),
	}

	if len(reports) > 0 {

		// The query returns newest first, so the far end of the list is the oldest error
		chosen := reports[0]

		if typed.oldest {
			chosen = reports[len(reports)-1]
		}

		backlog.Next = &chosen
	}

	return printJSON(backlog)
}

// runLatest prints the recent errors worth investigating
func runLatest(ctx context.Context, arguments []string) error {

	const location = "triage.runLatest"

	flags, typed := newFlagSet("latest")
	addSelectorFlags(flags, typed)
	addScanFlag(flags, typed)

	if _, err := parseArguments(flags, arguments); err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	reports, _, err := collect(ctx, config)

	if err != nil {
		return derp.Wrap(err, location, "Collecting errors")
	}

	return printJSON(reports)
}

// runShow prints the full, redacted report for a single error record
func runShow(ctx context.Context, arguments []string) error {

	const location = "triage.runShow"

	flags, typed := newFlagSet("show")
	positional, err := parseArguments(flags, arguments)

	if err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	// RULE: A record ID is required, because there is nothing sensible to show without one
	if len(positional) != 1 {
		return derp.BadRequest(location, "Usage: triage show <recordID>")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	store, _, err := open(ctx, config)

	if err != nil {
		return derp.Wrap(err, location, "Opening error log")
	}

	defer store.Close(context.Background())

	record, err := store.ByID(ctx, positional[0])

	if err != nil {
		return derp.Wrap(err, location, "Loading error record")
	}

	return printJSON(record.Analyze())
}

// runWatch streams every new error as a single line, until the process is interrupted
func runWatch(ctx context.Context, arguments []string) error {

	const location = "triage.runWatch"

	flags, typed := newFlagSet("watch")
	addSelectorFlags(flags, typed)

	if _, err := parseArguments(flags, arguments); err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	store, decisions, err := open(ctx, config)

	if err != nil {
		return derp.Wrap(err, location, "Opening error log")
	}

	defer store.Close(context.Background())

	selector := NewSelector(decisions, config.IncludeAll, config.IncludeDecided, config.IncludeDuplicates)

	// One line per error, written straight to stdout so a watcher sees it immediately
	err = store.Watch(ctx, func(record Record) {

		if report, accepted := selector.Select(record.Analyze()); accepted {
			fmt.Println(report.Summary())
		}
	})

	if err != nil {
		return derp.Wrap(err, location, "Watching error log")
	}

	return nil
}

// Outcome is what a "fixed" or "ignore" command actually did
type Outcome struct {
	Fingerprint string      `json:"fingerprint"`
	Disposition Disposition `json:"disposition"`
	Database    string      `json:"database"`
	Scanned     int         `json:"scanned"`
	Matched     int         `json:"matched"`
	Deleted     int64       `json:"deleted"`
	DryRun      bool        `json:"dryRun,omitempty"`
	Decision    *Decision   `json:"decision,omitempty"`
}

// runDecide records what was decided about an error.  A fixed error also has its records
// deleted, so the log drains as defects are repaired.
func runDecide(ctx context.Context, arguments []string, disposition Disposition) error {

	const location = "triage.runDecide"

	flags, typed := newFlagSet(string(disposition))
	flags.StringVar(&typed.note, "note", "", "What was decided about this error")
	flags.BoolVar(&typed.dryRun, "dry-run", false, "Report what would happen, without changing anything")
	addScanFlag(flags, typed)

	positional, err := parseArguments(flags, arguments)

	if err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	// RULE: A decision always names one error.  There is no command that empties the log.
	if len(positional) != 1 {
		return derp.BadRequest(location, "Usage: triage "+string(disposition)+" <recordID|fingerprint> [-note text] [-dry-run]")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	store, decisions, err := open(ctx, config)

	if err != nil {
		return derp.Wrap(err, location, "Opening error log")
	}

	defer store.Close(context.Background())

	report, match, err := resolveTarget(ctx, store, decisions, positional[0], config.ScanLimit)

	if err != nil {
		return derp.Wrap(err, location, "Finding error", positional[0])
	}

	// The database is named in the result, because this is the command that deletes things
	outcome := Outcome{
		Fingerprint: match.Fingerprint,
		Disposition: disposition,
		Database:    config.Database,
		Scanned:     match.Scanned,
		Matched:     len(match.Records),
		DryRun:      typed.dryRun,
	}

	// A dry run reports the damage it would do, and stops there
	if typed.dryRun {
		return printJSON(outcome)
	}

	// RULE: Only a fixed error is deleted.  An ignored error keeps its records as evidence.
	if disposition == DispositionFixed {

		deleted, err := store.Delete(ctx, match.RecordIDs())

		if err != nil {
			return derp.Wrap(err, location, "Deleting error records", match.Fingerprint)
		}

		outcome.Deleted = deleted
	}

	decision := decisions.Record(report, disposition, typed.note, int(outcome.Deleted))

	if err := decisions.Save(); err != nil {
		return derp.Wrap(err, location, "Saving decision")
	}

	outcome.Decision = &decision

	return printJSON(outcome)
}

// runDecisions prints every error that has already been fixed or ignored
func runDecisions(arguments []string) error {

	const location = "triage.runDecisions"

	flags, typed := newFlagSet("decisions")

	if _, err := parseArguments(flags, arguments); err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	decisions, err := LoadDecisions(config.StateFile)

	if err != nil {
		return derp.Wrap(err, location, "Loading decisions")
	}

	return printJSON(decisions.All())
}

// runForget removes a decision, so its error is reported again
func runForget(arguments []string) error {

	const location = "triage.runForget"

	flags, typed := newFlagSet("forget")
	positional, err := parseArguments(flags, arguments)

	if err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	if len(positional) != 1 {
		return derp.BadRequest(location, "Usage: triage forget <fingerprint>")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	decisions, err := LoadDecisions(config.StateFile)

	if err != nil {
		return derp.Wrap(err, location, "Loading decisions")
	}

	if !decisions.Forget(positional[0]) {
		return derp.NotFound(location, "No decision recorded for this fingerprint", positional[0])
	}

	if err := decisions.Save(); err != nil {
		return derp.Wrap(err, location, "Saving decisions")
	}

	return printJSON(decisions.All())
}

// runConfig prints the configuration every other command is using, and where it came from.
// This is the command to run before deleting anything, to confirm which database is in play.
func runConfig(arguments []string) error {

	const location = "triage.runConfig"

	flags, typed := newFlagSet("config")

	if _, err := parseArguments(flags, arguments); err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	config, err := loadConfig(flags, *typed)

	if err != nil {
		return derp.Wrap(err, location, "Reading configuration")
	}

	return printJSON(struct {
		Config
		Source     string   `json:"source"`
		SearchPath []string `json:"searchPath"`
	}{
		Config:     config,
		Source:     config.Source,
		SearchPath: SearchPath(),
	})
}

// runInit writes a starter configuration file for an operator to fill in
func runInit(arguments []string) error {

	const location = "triage.runInit"

	flags, typed := newFlagSet("init")

	if _, err := parseArguments(flags, arguments); err != nil {
		return derp.Wrap(err, location, "Reading arguments")
	}

	path := typed.configPath

	if path == "" {
		path = DefaultConfigFile()
	}

	// RULE: Never overwrite a configuration that already exists.  It may point at production.
	if _, err := os.Stat(path); err == nil {
		return derp.Conflict(location, "A configuration file is already here.  Edit it, or name another with -config", path)
	}

	config := NewConfig()

	if err := writeConfig(path, config); err != nil {
		return derp.Wrap(err, location, "Writing configuration file", path)
	}

	// RULE: Say so, loudly.  The file starts with no connection, it cannot be used until
	// somebody writes one, and `init` refuses to overwrite it on the second attempt.
	const warning = "connectString and database are EMPTY.  Fill them in before running any other command."

	return printJSON(struct {
		Config
		Written string `json:"written"`
		Warning string `json:"warning,omitempty"`
	}{
		Config:  config,
		Written: path,
		Warning: warning,
	})
}

// writeConfig saves a configuration file, readable only by the account that owns it
func writeConfig(path string, config Config) error {

	const location = "triage.writeConfig"

	contents, err := json.MarshalIndent(config, "", "\t")

	if err != nil {
		return derp.Internal(location, "Encoding configuration", err.Error())
	}

	directory := filepath.Dir(path)

	if err := os.MkdirAll(directory, 0700); err != nil {
		return derp.Internal(location, "Creating configuration directory", directory, err.Error())
	}

	// RULE: A connection string can carry a password, so this file is never world-readable
	if err := os.WriteFile(path, contents, 0600); err != nil {
		return derp.Internal(location, "Writing configuration file", path, err.Error())
	}

	// Configured, and ready for duty
	return nil
}

// collect reads the recent error log and returns the reports the caller asked to see,
// alongside the number of records actually scanned.
func collect(ctx context.Context, config Config) ([]Report, int, error) {

	const location = "triage.collect"

	store, decisions, err := open(ctx, config)

	if err != nil {
		return nil, 0, derp.Wrap(err, location, "Opening error log")
	}

	defer store.Close(context.Background())

	records, err := store.Latest(ctx, config.ScanLimit)

	if err != nil {
		return nil, 0, derp.Wrap(err, location, "Reading error log")
	}

	selector := NewSelector(decisions, config.IncludeAll, config.IncludeDecided, config.IncludeDuplicates)
	reports := make([]Report, 0, len(records))

	for _, record := range records {

		if report, accepted := selector.Select(record.Analyze()); accepted {
			reports = append(reports, report)
		}
	}

	return reports, len(records), nil
}

// resolveTarget finds every record belonging to one error, named by a record ID or a fingerprint
func resolveTarget(ctx context.Context, store Store, decisions Decisions, argument string, limit int64) (Report, Match, error) {

	const location = "triage.resolveTarget"

	// A record ID identifies one error exactly, and narrows the search for its siblings
	if len(argument) == 24 {

		record, err := store.ByID(ctx, argument)

		if err != nil {
			return Report{}, Match{}, derp.Wrap(err, location, "Loading error record")
		}

		match, err := store.FindFingerprint(ctx, record.Fingerprint(), record.Criteria(), limit)

		if err != nil {
			return Report{}, Match{}, derp.Wrap(err, location, "Finding matching records")
		}

		return record.Analyze(), match, nil
	}

	// RULE: Anything else must be a fingerprint, which costs a scan of the whole log
	if len(argument) != fingerprintLength {
		return Report{}, Match{}, derp.BadRequest(location, "Expected a 24-character record ID or a 12-character fingerprint", argument)
	}

	match, err := store.FindFingerprint(ctx, argument, bson.M{}, limit)

	if err != nil {
		return Report{}, Match{}, derp.Wrap(err, location, "Finding matching records")
	}

	if len(match.Records) > 0 {
		return match.Records[0].Analyze(), match, nil
	}

	// Every record may already be gone, in which case an earlier decision still describes it
	decision, exists := decisions.Lookup(argument)

	if !exists {
		return Report{}, Match{}, derp.NotFound(location, "No records match this fingerprint, and nothing was decided about it before", argument)
	}

	report := Report{
		Fingerprint: decision.Fingerprint,
		Origin:      decision.Origin,
		RootCause:   decision.RootCause,
	}

	return report, match, nil
}

// open connects to the error log and loads the local decisions
func open(ctx context.Context, config Config) (Store, Decisions, error) {

	const location = "triage.open"

	decisions, err := LoadDecisions(config.StateFile)

	if err != nil {
		return Store{}, Decisions{}, derp.Wrap(err, location, "Loading decisions")
	}

	store, err := Connect(ctx, config)

	if err != nil {
		return Store{}, Decisions{}, derp.Wrap(err, location, "Connecting to error log")
	}

	return store, decisions, nil
}

// printJSON writes a value to stdout as indented JSON
func printJSON(value any) error {

	const location = "triage.printJSON"

	encoded, err := json.MarshalIndent(value, "", "\t")

	if err != nil {
		return derp.Internal(location, "Rendering output", err.Error())
	}

	fmt.Println(string(encoded))

	return nil
}

// usage prints the command-line summary
func usage() {

	lines := []string{
		"Emissary error triage.  The error log is the work queue: pull an error, fix it, delete it.",
		"",
		"  triage next      [-oldest] [-all]              One error to work on, and how many remain",
		"  triage latest    [-all] [-decided]             Every distinct error worth investigating",
		"  triage show      <recordID>                    The full, redacted report for one error",
		"  triage watch     [-all]                        One line per new error, as it happens",
		"  triage fixed     <recordID|fingerprint>        DELETE every record of a repaired error",
		"  triage ignore    <recordID|fingerprint>        Stop reporting an error, keep its records",
		"  triage decisions                               Every error already fixed or ignored",
		"  triage forget    <fingerprint>                 Undo a decision, so the error returns",
		"  triage config                                  The settings in use, and where they came from",
		"  triage init                                    Write a starter configuration file",
		"",
		"Which database to read, where to record decisions, and the defaults for every flag",
		"below all live in " + configFileName + ".  It is searched for in this order:",
		"",
		"  1. the file named by -config",
		"  2. the file named by the " + configVariable + " environment variable",
		"  3. " + DefaultConfigFile() + "  (where `triage init` writes)",
		"  4. " + UserConfigFile(),
		"",
		"Per-invocation flags:",
		"  -config PATH   Configuration file to read, instead of searching for one",
		"  -n COUNT       How many records to read (overrides scanLimit)",
		"  -all           Include errors a remote server caused (overrides includeAll)",
		"  -decided       Include errors already fixed or ignored (overrides includeDecided)",
		"  -duplicates    Print every occurrence (overrides includeDuplicates)",
		"  -note TEXT     What was decided (with 'fixed' and 'ignore')",
		"  -dry-run       Report what would be deleted, without deleting it",
		"",
		"An error recorded as fixed that happens again is reported as a regression.",
		"",
	}

	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
}
