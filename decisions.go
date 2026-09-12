package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/benpate/derp"
)

// Disposition records what was decided about an error fingerprint
type Disposition string

// DispositionFixed marks an error whose records were deleted because the defect was repaired
const DispositionFixed = Disposition("fixed")

// DispositionIgnored marks an error that is understood, and deliberately not being worked on
const DispositionIgnored = Disposition("ignored")

// Decision is the local, permanent note about one error fingerprint.  It outlives the error
// records themselves, which are deleted from the database once a defect is fixed.
type Decision struct {
	Fingerprint string      `json:"fingerprint"`
	Disposition Disposition `json:"disposition"`
	Origin      string      `json:"origin"`
	RootCause   string      `json:"rootCause"`
	Note        string      `json:"note,omitempty"`
	DecidedAt   time.Time   `json:"decidedAt"`
	Deleted     int         `json:"deleted,omitempty"`
}

// Decisions is the local file recording every error that has been fixed or ignored
type Decisions struct {
	path      string
	decisions map[string]Decision
}

// LoadDecisions reads the decision file, returning an empty set when it does not exist yet
func LoadDecisions(path string) (Decisions, error) {

	const location = "triage.LoadDecisions"

	result := Decisions{
		path:      path,
		decisions: make(map[string]Decision),
	}

	// The path is a command-line flag, typed by an operator who already has a shell
	// #nosec G304
	contents, err := os.ReadFile(path)

	if err != nil {

		// A missing file is the normal first run, not a failure
		if os.IsNotExist(err) {
			return result, nil
		}

		return result, derp.Internal(location, "Reading decision file", path, err.Error())
	}

	if err := json.Unmarshal(contents, &result.decisions); err != nil {
		return result, derp.BadRequest(location, "Parsing decision file", path, err.Error())
	}

	return result, nil
}

// Lookup returns the decision recorded for an error fingerprint, if there is one
func (decisions Decisions) Lookup(fingerprint string) (Decision, bool) {
	decision, exists := decisions.decisions[fingerprint]
	return decision, exists
}

// Record writes a decision about an error, replacing whatever was decided before
func (decisions Decisions) Record(report Report, disposition Disposition, note string, deleted int) Decision {

	decision := Decision{
		Fingerprint: report.Fingerprint,
		Disposition: disposition,
		Origin:      report.Origin,
		RootCause:   report.RootCause,
		Note:        note,
		DecidedAt:   time.Now().UTC(),
		Deleted:     deleted,
	}

	decisions.decisions[report.Fingerprint] = decision

	return decision
}

// Forget removes a decision, so its error is reported again
func (decisions Decisions) Forget(fingerprint string) bool {

	if _, exists := decisions.decisions[fingerprint]; !exists {
		return false
	}

	delete(decisions.decisions, fingerprint)

	return true
}

// All returns every recorded decision, most recent first
func (decisions Decisions) All() []Decision {

	result := make([]Decision, 0, len(decisions.decisions))

	for _, decision := range decisions.decisions {
		result = append(result, decision)
	}

	sort.Slice(result, func(outer int, inner int) bool {
		return result[outer].DecidedAt.After(result[inner].DecidedAt)
	})

	return result
}

// Save writes the decisions back to disk, replacing the file in a single atomic step
func (decisions Decisions) Save() error {

	const location = "triage.Decisions.Save"

	contents, err := json.MarshalIndent(decisions.decisions, "", "\t")

	if err != nil {
		return derp.Internal(location, "Encoding decisions", err.Error())
	}

	directory := filepath.Dir(decisions.path)

	if err := os.MkdirAll(directory, 0700); err != nil {
		return derp.Internal(location, "Creating decision directory", directory, err.Error())
	}

	// Write a temporary file first, so an interrupted save cannot leave a half-written record
	temporary, err := os.CreateTemp(directory, "decisions-*.json")

	if err != nil {
		return derp.Internal(location, "Creating temporary decision file", directory, err.Error())
	}

	temporaryPath := temporary.Name()

	// A successful save renames this file away, so a failure to remove it means it was never
	// there.  Either way there is nothing a caller could do about it.
	defer func() {
		_ = os.Remove(temporaryPath)
	}()

	if _, err := temporary.Write(contents); err != nil {

		// The deferred cleanup removes this file, so a close failure here changes nothing
		_ = temporary.Close()

		return derp.Internal(location, "Writing temporary decision file", temporaryPath, err.Error())
	}

	if err := temporary.Close(); err != nil {
		return derp.Internal(location, "Closing temporary decision file", temporaryPath, err.Error())
	}

	if err := os.Rename(temporaryPath, decisions.path); err != nil {
		return derp.Internal(location, "Replacing decision file", decisions.path, err.Error())
	}

	// Mischief managed
	return nil
}
