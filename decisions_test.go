package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDecisions_MissingFileIsEmpty(t *testing.T) {

	decisions, err := LoadDecisions(filepath.Join(t.TempDir(), "nope", "decisions.json"))

	require.NoError(t, err, "a first run has no file yet, and that is not a failure")
	assert.Empty(t, decisions.All())
}

func TestLoadDecisions_RejectsGarbage(t *testing.T) {

	path := filepath.Join(t.TempDir(), "decisions.json")
	require.NoError(t, os.WriteFile(path, []byte("this is not json"), 0600))

	_, err := LoadDecisions(path)
	assert.Error(t, err)
}

func TestDecisions_RoundTrip(t *testing.T) {

	path := filepath.Join(t.TempDir(), "nested", "decisions.json")

	decisions, err := LoadDecisions(path)
	require.NoError(t, err)

	report := Report{Fingerprint: "abc123456789", Origin: "service.A", RootCause: "service.B: broken"}
	decision := decisions.Record(report, DispositionFixed, "added an index", 4)

	assert.Equal(t, DispositionFixed, decision.Disposition)
	assert.Equal(t, 4, decision.Deleted)
	require.NoError(t, decisions.Save())

	reloaded, err := LoadDecisions(path)
	require.NoError(t, err)

	stored, exists := reloaded.Lookup("abc123456789")
	require.True(t, exists)

	assert.Equal(t, "service.A", stored.Origin)
	assert.Equal(t, "service.B: broken", stored.RootCause)
	assert.Equal(t, "added an index", stored.Note)
	assert.Equal(t, DispositionFixed, stored.Disposition)
	assert.Equal(t, 4, stored.Deleted)
	assert.False(t, stored.DecidedAt.IsZero())
}

func TestDecisions_RecordReplaces(t *testing.T) {

	decisions, err := LoadDecisions(filepath.Join(t.TempDir(), "decisions.json"))
	require.NoError(t, err)

	report := Report{Fingerprint: "abc", Origin: "service.A"}

	decisions.Record(report, DispositionIgnored, "waiting on upstream", 0)
	decisions.Record(report, DispositionFixed, "shipped the fix", 9)

	stored, exists := decisions.Lookup("abc")
	require.True(t, exists)

	assert.Equal(t, DispositionFixed, stored.Disposition, "the newest decision wins")
	assert.Equal(t, "shipped the fix", stored.Note)
	assert.Equal(t, 9, stored.Deleted)
}

func TestDecisions_Lookup(t *testing.T) {

	decisions, err := LoadDecisions(filepath.Join(t.TempDir(), "decisions.json"))
	require.NoError(t, err)

	_, exists := decisions.Lookup("nothing")
	assert.False(t, exists)

	_, exists = decisions.Lookup("")
	assert.False(t, exists)
}

func TestDecisions_Forget(t *testing.T) {

	decisions, err := LoadDecisions(filepath.Join(t.TempDir(), "decisions.json"))
	require.NoError(t, err)

	decisions.Record(Report{Fingerprint: "abc"}, DispositionIgnored, "", 0)

	assert.True(t, decisions.Forget("abc"))
	assert.False(t, decisions.Forget("abc"), "forgetting twice is not an error, but reports nothing happened")
	assert.False(t, decisions.Forget("never recorded"))

	_, exists := decisions.Lookup("abc")
	assert.False(t, exists)
}

func TestDecisions_AllIsSortedNewestFirst(t *testing.T) {

	decisions, err := LoadDecisions(filepath.Join(t.TempDir(), "decisions.json"))
	require.NoError(t, err)

	decisions.decisions["old"] = Decision{Fingerprint: "old", DecidedAt: time.Now().Add(-time.Hour)}
	decisions.decisions["new"] = Decision{Fingerprint: "new", DecidedAt: time.Now()}
	decisions.decisions["older"] = Decision{Fingerprint: "older", DecidedAt: time.Now().Add(-time.Hour * 2)}

	all := decisions.All()

	require.Len(t, all, 3)
	assert.Equal(t, "new", all[0].Fingerprint)
	assert.Equal(t, "old", all[1].Fingerprint)
	assert.Equal(t, "older", all[2].Fingerprint)
}

func TestDecisions_AllOfEmptySet(t *testing.T) {

	decisions, err := LoadDecisions(filepath.Join(t.TempDir(), "decisions.json"))
	require.NoError(t, err)

	assert.Equal(t, []Decision{}, decisions.All())
}

func TestDecisions_SaveIsAtomic(t *testing.T) {

	directory := t.TempDir()
	path := filepath.Join(directory, "decisions.json")

	decisions, err := LoadDecisions(path)
	require.NoError(t, err)

	decisions.Record(Report{Fingerprint: "abc"}, DispositionFixed, "", 1)
	require.NoError(t, decisions.Save())

	// The temporary file must not survive a successful save
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "decisions.json", entries[0].Name())
}

func TestLoadDecisions_RejectsUnreadablePath(t *testing.T) {

	// A path whose parent is a FILE can never be read or created
	directory := t.TempDir()
	blocker := filepath.Join(directory, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0600))

	_, err := LoadDecisions(filepath.Join(blocker, "decisions.json"))
	assert.Error(t, err)
}

func TestDecisions_SaveRejectsUnwritablePath(t *testing.T) {

	directory := t.TempDir()
	blocker := filepath.Join(directory, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0600))

	decisions := Decisions{
		path:      filepath.Join(blocker, "decisions.json"),
		decisions: map[string]Decision{"abc": {Fingerprint: "abc"}},
	}

	assert.Error(t, decisions.Save())
}
