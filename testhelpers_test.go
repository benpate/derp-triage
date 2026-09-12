package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// stringify renders a value as JSON, so a test can assert on its whole shape at once
func stringify(t *testing.T, value any) string {

	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return string(encoded)
}
