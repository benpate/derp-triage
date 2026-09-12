package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestIsCredentialName(t *testing.T) {

	credentials := []string{
		"Authorization", "authorization", "  AUTHORIZATION  ",
		"Proxy-Authorization", "WWW-Authenticate", "Proxy-Authenticate",
		"Signature", "signature-input", "Digest",
		"Cookie", "Set-Cookie",
		"X-Api-Key", "apikey", "Api-Key", "X-Auth-Token", "X-CSRF-Token",
		"password", "secret", "token",
	}

	for _, name := range credentials {
		assert.True(t, IsCredentialName(name), "%q must be treated as a credential", name)
	}

	harmless := []string{
		"", "Accept", "Content-Type", "Date", "User-Agent", "Host",
		"signatures", "cookies", "tokenizer", "my-secret-sauce", "keyId",
	}

	for _, name := range harmless {
		assert.False(t, IsCredentialName(name), "%q must not be treated as a credential", name)
	}
}

func TestRedactString_QueryParameters(t *testing.T) {

	tests := map[string]string{
		"https://x.com/a?secret=hunter2":          "https://x.com/a?secret=[REDACTED]",
		"https://x.com/a?b=1&token=abc&c=2":       "https://x.com/a?b=1&token=[REDACTED]&c=2",
		"https://x.com/a?access_token=xyz":        "https://x.com/a?access_token=[REDACTED]",
		"https://x.com/a?client_secret=xyz":       "https://x.com/a?client_secret=[REDACTED]",
		"https://x.com/a?api-key=xyz":             "https://x.com/a?api-key=[REDACTED]",
		"https://x.com/a?password=p&username=bob": "https://x.com/a?password=[REDACTED]&username=bob",
		"https://x.com/a?sig=abc":                 "https://x.com/a?sig=[REDACTED]",
		"nothing to redact here":                  "nothing to redact here",
		"https://x.com/a?keyId=abc":               "https://x.com/a?keyId=abc",
		"https://x.com/a?monkey=abc":              "https://x.com/a?monkey=abc",
		"https://x.com/secretariat?safe=1":        "https://x.com/secretariat?safe=1",
	}

	for input, expected := range tests {
		assert.Equal(t, expected, RedactString(input), "redacting %q", input)
	}
}

func TestRedactString_Truncates(t *testing.T) {

	short := strings.Repeat("a", maxStringLength)
	assert.Equal(t, short, RedactString(short), "a string at the limit is untouched")

	long := strings.Repeat("a", maxStringLength+1)
	result := RedactString(long)

	assert.True(t, strings.HasSuffix(result, "...[truncated]"))
	assert.Equal(t, maxStringLength+len("...[truncated]"), len(result))
}

func TestRedactString_Empty(t *testing.T) {
	assert.Equal(t, "", RedactString(""))
}

func TestRedact_HandlesBothMapTypes(t *testing.T) {

	// The BSON decoder produces bson.M and bson.A, which are NOT map[string]any and []any.
	// Missing either spelling would silently pass every value through unredacted.
	inputs := []any{
		bson.M{"Signature": "secret-blob", "Accept": "text/html"},
		map[string]any{"Signature": "secret-blob", "Accept": "text/html"},
	}

	for _, input := range inputs {

		result, isObject := Redact(input).(map[string]any)
		require.True(t, isObject, "redacting %T must produce a map", input)

		assert.Equal(t, redactedValue, result["Signature"])
		assert.Equal(t, "text/html", result["Accept"])
	}
}

func TestRedact_NestedHeaders(t *testing.T) {

	input := bson.M{
		"request": bson.M{
			"header": bson.M{
				"Signature": bson.A{"keyId=\"x\",signature=\"blob\""},
				"Cookie":    bson.A{"session=abc"},
				"Accept":    bson.A{"application/activity+json"},
			},
			"url": "https://x.com/inbox?secret=abc",
		},
	}

	result := Redact(input).(map[string]any)
	request := result["request"].(map[string]any)
	header := request["header"].(map[string]any)

	assert.Equal(t, redactedValue, header["Signature"])
	assert.Equal(t, redactedValue, header["Cookie"])
	assert.Equal(t, []any{"application/activity+json"}, header["Accept"])
	assert.Equal(t, "https://x.com/inbox?secret=[REDACTED]", request["url"])
}

func TestRedact_Scalars(t *testing.T) {

	assert.Nil(t, Redact(nil))
	assert.Equal(t, 42, Redact(42))
	assert.Equal(t, int32(7), Redact(int32(7)))
	assert.Equal(t, 1.5, Redact(1.5))
	assert.Equal(t, true, Redact(true))
}

func TestRedact_BoundsDepth(t *testing.T) {

	// Build a chain deeper than the walker is allowed to follow
	var deepest any = "bottom"

	for range maxDepth + 5 {
		deepest = bson.M{"next": deepest}
	}

	result := Redact(deepest)
	encoded := stringify(t, result)

	assert.Contains(t, encoded, "...[truncated]", "a chain past the depth limit must be cut off")
	assert.NotContains(t, encoded, "bottom", "nothing past the depth limit may be printed")
}

func TestRedact_BoundsWidth(t *testing.T) {

	wide := make(bson.A, 0, maxSliceLength*3)

	for index := range maxSliceLength * 3 {
		wide = append(wide, index)
	}

	result := Redact(wide).([]any)

	require.Len(t, result, maxSliceLength+1, "a long array keeps its head plus one marker")
	assert.Equal(t, "...[40 more]", result[maxSliceLength])
}

func TestRedact_BoundsNodeCount(t *testing.T) {

	// A map far wider than the node budget must terminate, not exhaust memory
	wide := bson.M{}

	for index := range maxNodes * 2 {
		wide[string(rune('a'+index%26))+strings.Repeat("x", index%7)] = bson.M{"inner": index}
	}

	result := Redact(wide).(map[string]any)
	assert.NotEmpty(t, result, "the walker still returns what it managed to read")
}

func TestRedact_EmptySliceAndMap(t *testing.T) {

	assert.Equal(t, []any{}, Redact(bson.A{}))
	assert.Equal(t, map[string]any{}, Redact(bson.M{}))
}

func FuzzRedactString(f *testing.F) {

	f.Add("https://x.com/a?secret=hunter2")
	f.Add("")
	f.Add("?token=")
	f.Add("&&&===")
	f.Add(strings.Repeat("a", 5000))
	f.Add("\x00\xff invalid utf8")

	f.Fuzz(func(t *testing.T, input string) {

		result := RedactString(input)

		// The result is always bounded, and never leaks a value we claimed to redact
		assert.LessOrEqual(t, len(result), maxStringLength+len("...[truncated]"))

		if !strings.Contains(input, "=") {
			assert.NotContains(t, result, redactedValue, "nothing to redact in %q", input)
		}
	})
}

func TestRedactParameter_WithoutAValue(t *testing.T) {

	// The regexp always supplies an "=", so this guard exists only so a future caller cannot
	// turn a malformed pair into a leak.
	assert.Equal(t, redactedValue, redactParameter("no-equals-sign"))
	assert.Equal(t, "token=[REDACTED]", redactParameter("token=abc"))
	assert.Equal(t, "token=[REDACTED]", redactParameter("token="))
}
