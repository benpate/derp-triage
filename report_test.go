package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeMessage(t *testing.T) {

	tests := map[string]string{
		"lookup bsky.brid.gy: no such host":       "lookup <host>: no such host",
		"Loading https://x.com/users/1 failed":    "Loading <url> failed",
		"object 6aa17bd9a102749e24915ab4 is gone": "object <id> is gone",
		"timed out after 30000 ms":                "timed out after <n> ms",
		"  collapse   the   spaces  ":             "collapse the spaces",
		"":                                        "",
		"context deadline exceeded":               "context deadline exceeded",
	}

	for input, expected := range tests {
		assert.Equal(t, expected, NormalizeMessage(input), "normalizing %q", input)
	}
}

func TestNormalizeMessage_KeepsGoIdentifiers(t *testing.T) {

	// A Go type looks like a hostname but must survive, or two unrelated template errors
	// would collapse into one fingerprint and only the first would ever be investigated.
	input := `can't evaluate field Object in type build.Follower`
	assert.Equal(t, input, NormalizeMessage(input))

	assert.Equal(t, "service.Widget.Save failed", NormalizeMessage("service.Widget.Save failed"))
}

func TestNormalizeMessage_ShortNumbersSurvive(t *testing.T) {
	// Line and column numbers distinguish two errors in the same template
	assert.Equal(t, "template: x:1:17: bad", NormalizeMessage("template: x:1:17: bad"))
	assert.Equal(t, "403 Forbidden", NormalizeMessage("403 Forbidden"))
}

func TestFingerprint_IsStable(t *testing.T) {

	first := Fingerprint(500, "service.A", "service.B", "lookup one.example.com: no such host")
	second := Fingerprint(500, "service.A", "service.B", "lookup two.example.org: no such host")

	assert.Equal(t, first, second, "the same failure against two hosts is one error")
	assert.Len(t, first, fingerprintLength)
}

func TestFingerprint_SeparatesDifferentErrors(t *testing.T) {

	base := Fingerprint(500, "service.A", "service.B", "broken")

	assert.NotEqual(t, base, Fingerprint(404, "service.A", "service.B", "broken"))
	assert.NotEqual(t, base, Fingerprint(500, "service.Z", "service.B", "broken"))
	assert.NotEqual(t, base, Fingerprint(500, "service.A", "service.Z", "broken"))
	assert.NotEqual(t, base, Fingerprint(500, "service.A", "service.B", "different"))
}

func TestFingerprint_HandlesEmptyInput(t *testing.T) {
	assert.Len(t, Fingerprint(0, "", "", ""), fingerprintLength)
}

func TestFingerprint_FieldsCannotBleedTogether(t *testing.T) {

	// The separator must keep "ab"+"c" apart from "a"+"bc"
	assert.NotEqual(t,
		Fingerprint(500, "ab", "c", "x"),
		Fingerprint(500, "a", "bc", "x"),
	)
}

func TestTruncate(t *testing.T) {

	assert.Equal(t, "abc", Truncate("abc", 5))
	assert.Equal(t, "abc", Truncate("abc", 3))
	assert.Equal(t, "ab...[truncated]", Truncate("abc", 2))
	assert.Equal(t, "", Truncate("abc", 0))
	assert.Equal(t, "", Truncate("abc", -1))
	assert.Equal(t, "", Truncate("", 5))
}

func TestReport_Summary(t *testing.T) {

	report := Report{
		RecordID:    "6aa17bd9a102749e24915ab4",
		CreateDate:  "2026-09-11T15:08:10Z",
		StatusCode:  500,
		Category:    CategoryLocal,
		Fingerprint: "62988498029f",
		Origin:      "service.template.loadTemplates",
		RootCause:   strings.Repeat("x", summaryLength+50),
		Chain:       []Frame{{Location: "a"}, {Location: "b"}},
		Payload:     map[string]any{"big": "payload"},
	}

	summary := report.Summary()

	assert.Equal(t, 1, strings.Count(summary+"\n", "\n"), "a summary is exactly one line")
	assert.NotContains(t, summary, "payload", "the bulk of the report stays out of the summary")
	assert.NotContains(t, summary, `"chain"`)

	decoded := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(summary), &decoded))

	assert.Equal(t, "62988498029f", decoded["fingerprint"])
	assert.Equal(t, "service.template.loadTemplates", decoded["origin"])
	assert.Contains(t, decoded["rootCause"], "...[truncated]")
	assert.NotContains(t, decoded, "regression", "a normal error carries no regression flag")
}

func TestReport_SummaryFlagsRegression(t *testing.T) {

	report := Report{Fingerprint: "abc", Regression: true}

	decoded := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(report.Summary()), &decoded))

	assert.Equal(t, true, decoded["regression"])
}

func TestReport_SummaryOfEmptyReport(t *testing.T) {

	decoded := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(Report{}.Summary()), &decoded))
}

func TestNormalizeMessage_IsSinglePass(t *testing.T) {

	// Normalizing is NOT idempotent, and Fingerprint relies on calling it exactly once.  A
	// placeholder rewrites the word boundaries around it, which exposes matches the first pass
	// could not reach: below, "<n>" puts a boundary in front of text that then reads as a host.
	once := NormalizeMessage("A000000000000a.aaaaa")
	assert.Equal(t, "A<n>a.aaaaa", once)
	assert.Equal(t, "A<n><host>", NormalizeMessage(once), "a second pass matches more, which is why there is never one")
}

func FuzzNormalizeMessage(f *testing.F) {

	f.Add("lookup bsky.brid.gy: no such host")
	f.Add("https://x.com/a?b=c")
	f.Add("")
	f.Add("....")
	f.Add("\x00\xff")
	f.Add("0000://0")
	f.Add("0.0000")
	f.Add(strings.Repeat("a.b", 2000))

	f.Fuzz(func(t *testing.T, input string) {

		result := NormalizeMessage(input)

		// The same message must always normalize the same way, or one error would fingerprint
		// as two and be investigated twice.
		assert.Equal(t, result, NormalizeMessage(input), "normalizing %q must be deterministic", input)

		// Whitespace is always collapsed, so a reformatted message is still one error
		assert.Equal(t, strings.TrimSpace(result), result, "normalizing %q left outer whitespace", input)
		assert.NotRegexp(t, `\s\s`, result, "normalizing %q left a run of whitespace", input)
	})
}

func FuzzFingerprint(f *testing.F) {

	f.Add(500, "a", "b", "c")
	f.Add(0, "", "", "")
	f.Add(-1, "|", "|", "|")

	f.Fuzz(func(t *testing.T, code int, origin string, root string, message string) {

		first := Fingerprint(code, origin, root, message)

		assert.Len(t, first, fingerprintLength)
		assert.Equal(t, first, Fingerprint(code, origin, root, message), "fingerprints must be deterministic")
	})
}
