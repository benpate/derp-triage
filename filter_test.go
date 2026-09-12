package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainsPhrase_WordBoundaries(t *testing.T) {

	// A short token must not match inside a longer word.  "eof" hiding in "neofetch" would
	// classify a real defect as a network hiccup, and it would never be investigated.
	assert.True(t, containsPhrase("unexpected EOF", "eof"))
	assert.True(t, containsPhrase("EOF", "eof"))
	assert.True(t, containsPhrase("eof: done", "eof"))
	assert.False(t, containsPhrase("neofetch failed", "eof"))
	assert.False(t, containsPhrase("geoffrey", "eof"))
	assert.False(t, containsPhrase("eofs", "eof"))
	assert.False(t, containsPhrase("", "eof"))
	assert.False(t, containsPhrase("anything", ""))
}

func TestContainsPhrase_CaseInsensitive(t *testing.T) {
	assert.True(t, containsPhrase("TLS Handshake Timeout", "tls handshake timeout"))
	assert.True(t, containsPhrase("No Such Host", "no such host"))
}

func TestIsWordCharacter(t *testing.T) {

	assert.True(t, isWordCharacter("abc", 0))
	assert.True(t, isWordCharacter("ABC", 1))
	assert.True(t, isWordCharacter("a1c", 1))
	assert.False(t, isWordCharacter("a c", 1))
	assert.False(t, isWordCharacter("a-c", 1))
	assert.False(t, isWordCharacter("abc", -1), "before the start is not a word character")
	assert.False(t, isWordCharacter("abc", 3), "past the end is not a word character")
	assert.False(t, isWordCharacter("", 0))
}

func TestCategorize(t *testing.T) {

	tests := []struct {
		location string
		message  string
		expected Category
	}{
		// Anything outside the HTTP client is ours
		{"build.StepViewHTML.Get", "template parse failed", CategoryLocal},
		{"data-mongo.Collection.HardDelete", "context deadline exceeded", CategoryLocal},
		{"service.Widget.Save", "no such host", CategoryLocal},
		{"", "", CategoryLocal},

		// An HTTP status is the far end's answer
		{"remote.Transaction.Send", "403 Forbidden", CategoryPeer},
		{"remote.Transaction.Send", "500 Internal Server Error", CategoryPeer},
		{"remote.Transaction.Send", "404", CategoryPeer},
		{"remote.Transaction.Send", "  502 Bad Gateway", CategoryPeer},

		// A network failure means the far end was unreachable
		{"remote.Transaction.executeRequest", "lookup bsky.brid.gy: no such host", CategoryTransport},
		{"remote.Transaction.executeRequest", "net/http: TLS handshake timeout", CategoryTransport},
		{"remote.Transaction.executeRequest", "dial tcp: connection refused", CategoryTransport},
		{"remote.Transaction.executeRequest", "unexpected EOF", CategoryTransport},
		{"remote.Transaction.executeRequest", "x509: certificate has expired", CategoryTransport},

		// RULE: an unrecognized failure in the HTTP client is still ours, on purpose
		{"remote.Transaction.Send", "Error building request URL", CategoryLocal},
		{"remote.Transaction.Send", "6000 is not a status", CategoryLocal},
	}

	for _, test := range tests {
		actual := Categorize(test.location, test.message)
		assert.Equal(t, test.expected, actual, "categorizing %q / %q", test.location, test.message)
	}
}

func TestCategory_IsActionable(t *testing.T) {
	assert.True(t, CategoryLocal.IsActionable())
	assert.False(t, CategoryPeer.IsActionable())
	assert.False(t, CategoryTransport.IsActionable())
	assert.False(t, Category("anything else").IsActionable())
}

// newReport builds a Report for the Selector tests
func newReport(fingerprint string, category Category, createDate time.Time) Report {
	return Report{
		RecordID:    "aaaaaaaaaaaaaaaaaaaaaaaa",
		CreateDate:  createDate.UTC().Format(time.RFC3339),
		Category:    category,
		Fingerprint: fingerprint,
		Origin:      "service.Test",
		RootCause:   "service.Test: broken",
	}
}

// newDecisions builds an in-memory decision set for the Selector tests
func newDecisions(entries ...Decision) Decisions {

	decisions := Decisions{path: "", decisions: make(map[string]Decision)}

	for _, entry := range entries {
		decisions.decisions[entry.Fingerprint] = entry
	}

	return decisions
}

func TestSelector_FiltersRemoteCauses(t *testing.T) {

	selector := NewSelector(newDecisions(), false, false, true)

	_, local := selector.Select(newReport("aaa", CategoryLocal, time.Now()))
	_, peer := selector.Select(newReport("bbb", CategoryPeer, time.Now()))
	_, transport := selector.Select(newReport("ccc", CategoryTransport, time.Now()))

	assert.True(t, local)
	assert.False(t, peer)
	assert.False(t, transport)
}

func TestSelector_IncludeAll(t *testing.T) {

	selector := NewSelector(newDecisions(), true, false, true)

	_, peer := selector.Select(newReport("bbb", CategoryPeer, time.Now()))
	assert.True(t, peer)
}

func TestSelector_CollapsesDuplicates(t *testing.T) {

	selector := NewSelector(newDecisions(), false, false, false)

	_, first := selector.Select(newReport("aaa", CategoryLocal, time.Now()))
	_, second := selector.Select(newReport("aaa", CategoryLocal, time.Now()))
	_, other := selector.Select(newReport("bbb", CategoryLocal, time.Now()))

	assert.True(t, first)
	assert.False(t, second, "the same error is reported once")
	assert.True(t, other)
}

func TestSelector_KeepsDuplicatesWhenAsked(t *testing.T) {

	selector := NewSelector(newDecisions(), false, false, true)

	_, first := selector.Select(newReport("aaa", CategoryLocal, time.Now()))
	_, second := selector.Select(newReport("aaa", CategoryLocal, time.Now()))

	assert.True(t, first)
	assert.True(t, second)
}

func TestSelector_HidesDecidedErrors(t *testing.T) {

	decided := time.Now()

	decisions := newDecisions(
		Decision{Fingerprint: "fixed", Disposition: DispositionFixed, DecidedAt: decided},
		Decision{Fingerprint: "ignored", Disposition: DispositionIgnored, DecidedAt: decided},
	)

	selector := NewSelector(decisions, false, false, true)

	// A record written BEFORE the decision is a leftover, not news
	_, stale := selector.Select(newReport("fixed", CategoryLocal, decided.Add(-time.Hour)))
	_, ignored := selector.Select(newReport("ignored", CategoryLocal, decided.Add(time.Hour)))

	assert.False(t, stale)
	assert.False(t, ignored)
}

func TestSelector_FlagsRegressions(t *testing.T) {

	decided := time.Now()
	decisions := newDecisions(Decision{Fingerprint: "fixed", Disposition: DispositionFixed, DecidedAt: decided})
	selector := NewSelector(decisions, false, false, true)

	report, accepted := selector.Select(newReport("fixed", CategoryLocal, decided.Add(time.Hour)))

	require.True(t, accepted, "an error that came back after a fix is always news")
	assert.True(t, report.Regression)
}

func TestSelector_IgnoredErrorIsNeverARegression(t *testing.T) {

	decided := time.Now()
	decisions := newDecisions(Decision{Fingerprint: "ignored", Disposition: DispositionIgnored, DecidedAt: decided})
	selector := NewSelector(decisions, false, false, true)

	report, accepted := selector.Select(newReport("ignored", CategoryLocal, decided.Add(time.Hour)))

	assert.False(t, accepted)
	assert.False(t, report.Regression, "an ignored error recurring is exactly what was expected")
}

func TestSelector_UnparsableDateIsNotARegression(t *testing.T) {

	decisions := newDecisions(Decision{Fingerprint: "fixed", Disposition: DispositionFixed, DecidedAt: time.Now()})
	selector := NewSelector(decisions, false, false, true)

	report := newReport("fixed", CategoryLocal, time.Now())
	report.CreateDate = "not a date"

	annotated, accepted := selector.Select(report)

	assert.False(t, annotated.Regression)
	assert.False(t, accepted)
}

func TestSelector_IncludeDecided(t *testing.T) {

	decisions := newDecisions(Decision{Fingerprint: "ignored", Disposition: DispositionIgnored, DecidedAt: time.Now()})
	selector := NewSelector(decisions, false, true, true)

	_, accepted := selector.Select(newReport("ignored", CategoryLocal, time.Now()))
	assert.True(t, accepted)
}
