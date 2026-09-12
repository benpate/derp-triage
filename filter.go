package main

import (
	"regexp"
	"strings"
	"time"
)

// Category names where an error actually originated, which decides whether it is worth triaging
type Category string

// CategoryLocal marks an error raised inside Emissary or one of its libraries
const CategoryLocal = Category("local")

// CategoryTransport marks a failure to reach a remote server: DNS, TLS, timeouts, refused connections
const CategoryTransport = Category("remote-transport")

// CategoryPeer marks an HTTP error status returned by a remote server
const CategoryPeer = Category("peer-response")

// IsActionable returns TRUE if a Category describes a failure that Emissary itself could fix
func (category Category) IsActionable() bool {
	return category == CategoryLocal
}

// peerStatus matches a message that is really an HTTP status line echoed back from a remote server
var peerStatus = regexp.MustCompile(`^[1-5][0-9]{2}(\s|$)`)

// Categorize reports where an error came from, using the root location and message that
// tools/derp-mongo stores alongside every record.
func Categorize(rootLocation string, rootMessage string) Category {

	// RULE: Anything that did not fail in the outbound HTTP client is ours to investigate
	if !isOutboundRequest(rootLocation) {
		return CategoryLocal
	}

	// A bare HTTP status is the far end's answer, not a defect on this side
	if peerStatus.MatchString(strings.TrimSpace(rootMessage)) {
		return CategoryPeer
	}

	// A network-level failure says the far end was unreachable
	if isTransportFailure(rootMessage) {
		return CategoryTransport
	}

	// Anything else in the HTTP client -- a URL we built wrong, a body we could not encode --
	// is still our defect, so an unrecognized message stays actionable on purpose.
	return CategoryLocal
}

// isOutboundRequest returns TRUE if a location belongs to the outbound HTTP client
func isOutboundRequest(location string) bool {
	return strings.HasPrefix(location, "remote.")
}

// isTransportFailure returns TRUE if a message describes a network-level failure to reach a peer
func isTransportFailure(message string) bool {

	phrases := []string{
		"no such host",
		"server misbehaving",
		"tls handshake timeout",
		"connection refused",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"context deadline exceeded",
		"network is unreachable",
		"no route to host",
		"certificate",
		"eof",
		"stream error",
	}

	for _, phrase := range phrases {
		if containsPhrase(message, phrase) {
			return true
		}
	}

	return false
}

// containsPhrase returns TRUE if a phrase appears in a string on word boundaries, so that a
// short token such as "eof" cannot match inside a longer word.
func containsPhrase(haystack string, phrase string) bool {

	// RULE: An empty phrase matches nothing.  Searching for one would never advance.
	if phrase == "" {
		return false
	}

	haystack = strings.ToLower(haystack)
	phrase = strings.ToLower(phrase)

	for offset := 0; offset <= len(haystack)-len(phrase); {

		index := strings.Index(haystack[offset:], phrase)

		if index < 0 {
			return false
		}

		start := offset + index

		// The match counts only when neither edge sits inside a longer word
		if end := start + len(phrase); !isWordCharacter(haystack, start-1) && !isWordCharacter(haystack, end) {
			return true
		}

		offset = start + 1
	}

	return false
}

// isWordCharacter returns TRUE if the byte at an index is a letter or digit
func isWordCharacter(value string, index int) bool {

	if index < 0 {
		return false
	}

	if index >= len(value) {
		return false
	}

	character := value[index]

	if character >= 'a' && character <= 'z' {
		return true
	}

	if character >= 'A' && character <= 'Z' {
		return true
	}

	return character >= '0' && character <= '9'
}

// Selector decides which reports are worth printing, and flags the ones that have come back
// after being marked as fixed.
type Selector struct {
	decisions         Decisions
	includeAll        bool // Include errors that a remote server caused
	includeDecided    bool // Include errors already recorded as fixed or ignored
	includeDuplicates bool // Print every occurrence, instead of one per fingerprint
	reported          map[string]bool
}

// NewSelector returns a Selector that also remembers which fingerprints it has already accepted
func NewSelector(decisions Decisions, includeAll bool, includeDecided bool, includeDuplicates bool) *Selector {
	return &Selector{
		decisions:         decisions,
		includeAll:        includeAll,
		includeDecided:    includeDecided,
		includeDuplicates: includeDuplicates,
		reported:          make(map[string]bool),
	}
}

// Select returns a Report annotated with what is known about it, and reports whether it
// should be printed at all.
func (selector *Selector) Select(report Report) (Report, bool) {

	// RULE: A failure caused by the far end is not ours to fix
	if !selector.includeAll && !report.Category.IsActionable() {
		return report, false
	}

	// An error that has come back after being fixed is a regression, and always news
	report.Regression = selector.isRegression(report)

	if report.Regression {
		return report, selector.claim(report.Fingerprint)
	}

	// RULE: An error already fixed or ignored stays quiet until the caller asks for it
	if !selector.includeDecided && selector.isDecided(report.Fingerprint) {
		return report, false
	}

	return report, selector.claim(report.Fingerprint)
}

// isRegression returns TRUE if an error was recorded as fixed, and has happened again since
func (selector *Selector) isRegression(report Report) bool {

	decision, exists := selector.decisions.Lookup(report.Fingerprint)

	if !exists {
		return false
	}

	if decision.Disposition != DispositionFixed {
		return false
	}

	// A record written before the fix is a leftover, not a recurrence
	createDate, err := time.Parse(time.RFC3339, report.CreateDate)

	if err != nil {
		return false
	}

	// RULE: A report's date is only accurate to the second, so a decision made within the same
	// second counts as earlier.  Missing a regression is worse than reporting one twice.
	return !createDate.Before(decision.DecidedAt.Truncate(time.Second))
}

// isDecided returns TRUE if a fingerprint has already been fixed or ignored
func (selector *Selector) isDecided(fingerprint string) bool {
	_, exists := selector.decisions.Lookup(fingerprint)
	return exists
}

// claim records that a fingerprint has been printed, and reports whether this was the first time
func (selector *Selector) claim(fingerprint string) bool {

	// Every occurrence counts when the caller is browsing the whole log
	if selector.includeDuplicates {
		return true
	}

	if selector.reported[fingerprint] {
		return false
	}

	selector.reported[fingerprint] = true

	return true
}
