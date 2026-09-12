package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// summaryLength bounds the root cause printed on a one-line summary
const summaryLength = 160

// fingerprintLength is how much of the hash identifies an error.  Twelve hex characters is
// plenty to keep thousands of distinct errors apart, and short enough to type.
const fingerprintLength = 12

// Frame is one level of a derp error's nested chain
type Frame struct {
	Code     int      `json:"code,omitempty"`
	Location string   `json:"location,omitempty"`
	Message  string   `json:"message,omitempty"`
	Details  []string `json:"details,omitempty"`
}

// Report is one error record, reduced to what a troubleshooter needs and stripped of secrets
type Report struct {
	RecordID    string   `json:"recordId"`
	CreateDate  string   `json:"createDate"`
	StatusCode  int      `json:"statusCode"`
	Category    Category `json:"category"`
	Fingerprint string   `json:"fingerprint"`
	Origin      string   `json:"origin"`
	RootCause   string   `json:"rootCause"`
	Regression  bool     `json:"regression,omitempty"`
	Chain       []Frame  `json:"chain,omitempty"`
	Payload     any      `json:"payload,omitempty"`
}

// Summary renders a Report as the single JSON line that watch mode emits
func (report Report) Summary() string {

	line := struct {
		RecordID    string   `json:"recordId"`
		CreateDate  string   `json:"createDate"`
		StatusCode  int      `json:"statusCode"`
		Category    Category `json:"category"`
		Regression  bool     `json:"regression,omitempty"`
		Fingerprint string   `json:"fingerprint"`
		Origin      string   `json:"origin"`
		RootCause   string   `json:"rootCause"`
	}{
		RecordID:    report.RecordID,
		CreateDate:  report.CreateDate,
		StatusCode:  report.StatusCode,
		Category:    report.Category,
		Regression:  report.Regression,
		Fingerprint: report.Fingerprint,
		Origin:      report.Origin,
		RootCause:   Truncate(report.RootCause, summaryLength),
	}

	encoded, err := json.Marshal(line)

	// A struct of strings and ints cannot fail to marshal, so this path is unreachable
	if err != nil {
		return `{"recordId":"` + report.RecordID + `","error":"cannot render summary"}`
	}

	return string(encoded)
}

// Patterns that replace the parts of an error message that change from one occurrence to the next
var (
	variableURL      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	variableObjectID = regexp.MustCompile(`\b[0-9a-f]{24}\b`)
	variableHostname = regexp.MustCompile(`\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9][a-z0-9-]*)*\.[a-z]{2,24}\b`)
	variableNumber   = regexp.MustCompile(`\d{4,}`)
	repeatedSpace    = regexp.MustCompile(`\s+`)
)

// Placeholders that stand in for the parts of a message that change between occurrences
const (
	placeholderURL      = "<url>"
	placeholderObjectID = "<id>"
	placeholderHostname = "<host>"
	placeholderNumber   = "<n>"
)

// NormalizeMessage removes the parts of an error message that vary between occurrences, so that
// the same failure against two different hosts still produces one fingerprint.
//
// RULE: This is a SINGLE-PASS transform, and must never be applied to its own output.  Every
// replacement rewrites the word boundaries around it, so a second pass matches text the first
// one could not reach.  Fingerprint is the only caller, and it normalizes a raw message once.
func NormalizeMessage(message string) string {

	// Order matters: a URL contains hostnames and digits, so it is replaced whole, first
	message = variableURL.ReplaceAllString(message, placeholderURL)
	message = variableObjectID.ReplaceAllString(message, placeholderObjectID)
	message = variableHostname.ReplaceAllString(message, placeholderHostname)
	message = variableNumber.ReplaceAllString(message, placeholderNumber)

	return strings.TrimSpace(repeatedSpace.ReplaceAllString(message, " "))
}

// Fingerprint builds a stable identity for an error, so that a failure already investigated is
// recognized the next time it happens.
func Fingerprint(statusCode int, origin string, rootLocation string, rootMessage string) string {

	seed := strings.Join([]string{
		strconv.Itoa(statusCode),
		origin,
		rootLocation,
		NormalizeMessage(rootMessage),
	}, "|")

	sum := sha256.Sum256([]byte(seed))

	return hex.EncodeToString(sum[:])[:fingerprintLength]
}

// Truncate shortens a string to a maximum length, marking anything it removed
func Truncate(value string, length int) string {

	if length <= 0 {
		return ""
	}

	if len(value) <= length {
		return value
	}

	return value[:length] + "...[truncated]"
}
