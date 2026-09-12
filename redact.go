package main

import (
	"fmt"
	"regexp"
	"strings"
)

// redactedValue replaces anything that must never leave the database
const redactedValue = "[REDACTED]"

// Limits that keep one error record small enough to read, and immune to a pathological payload
const (
	maxStringLength = 400 // Longest string kept before truncation
	maxSliceLength  = 20  // Most elements kept from a single array
	maxDepth        = 12  // Deepest nesting walked
	maxNodes        = 500 // Most values walked in one record
)

// secretParameter matches a query-string parameter whose value is a credential
var secretParameter = regexp.MustCompile(`(?i)\b(secret|token|password|passwd|pwd|key|api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|signature|sig|auth|session)=[^&\s"'<>]+`)

// IsCredentialName returns TRUE if a map key (usually an HTTP header) holds a secret
func IsCredentialName(name string) bool {

	switch strings.ToLower(strings.TrimSpace(name)) {

	case "authorization", "proxy-authorization", "www-authenticate", "proxy-authenticate":
		return true

	case "signature", "digest", "signature-input":
		return true

	case "cookie", "set-cookie":
		return true

	case "api-key", "apikey", "x-api-key", "x-auth-token", "x-csrf-token", "password", "secret", "token":
		return true
	}

	return false
}

// RedactString removes credentials embedded in a string and trims it to a readable length
func RedactString(value string) string {

	value = secretParameter.ReplaceAllStringFunc(value, redactParameter)

	if len(value) <= maxStringLength {
		return value
	}

	return value[:maxStringLength] + "...[truncated]"
}

// redactParameter rewrites one "name=value" pair, keeping the name and discarding the value
func redactParameter(pair string) string {

	name, _, found := strings.Cut(pair, "=")

	if !found {
		return redactedValue
	}

	return name + "=" + redactedValue
}

// redactor walks a decoded error payload once, spending a fixed budget of nodes
type redactor struct {
	nodes int
}

// Redact returns a copy of a decoded error payload with credentials removed and bulk data trimmed
func Redact(value any) any {
	walker := redactor{nodes: maxNodes}
	return walker.value(value, 0)
}

// value redacts a single node, recursing into maps and slices until a budget is spent
func (walker *redactor) value(value any, depth int) any {

	// RULE: A record that nests deeper, or wider, than these limits is truncated rather than
	// walked.  Error payloads are attacker-influenced, so the budget is not negotiable.
	if depth > maxDepth {
		return "...[truncated]"
	}

	if walker.nodes <= 0 {
		return "...[truncated]"
	}

	walker.nodes--

	if value == nil {
		return nil
	}

	if text, isString := value.(string); isString {
		return RedactString(text)
	}

	// The BSON decoder produces its own map and slice types, so both spellings are unpacked
	if object, isObject := toObject(value); isObject {
		return walker.object(object, depth)
	}

	if entries, isSlice := toSlice(value); isSlice {
		return walker.slice(entries, depth)
	}

	// Numbers, booleans, dates, and anything else carry no secrets and no bulk
	return value
}

// object redacts every entry of a map, dropping the values of credential-bearing keys
func (walker *redactor) object(value map[string]any, depth int) map[string]any {

	result := make(map[string]any, len(value))

	for key, entry := range value {

		// RULE: A credential is discarded by NAME, without ever inspecting its value
		if IsCredentialName(key) {
			result[key] = redactedValue
			continue
		}

		result[key] = walker.value(entry, depth+1)
	}

	return result
}

// slice redacts the leading elements of an array and reports how many were dropped
func (walker *redactor) slice(value []any, depth int) []any {

	// An absent array carries nothing to walk
	if value == nil {
		return []any{}
	}

	kept := min(len(value), maxSliceLength)
	result := make([]any, 0, kept+1)

	for _, entry := range value[:kept] {
		result = append(result, walker.value(entry, depth+1))
	}

	if len(value) > kept {
		result = append(result, fmt.Sprintf("...[%d more]", len(value)-kept))
	}

	return result
}
