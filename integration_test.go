package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// testConnectString is the local replica set these tests expect.  RULE: a Go client reaching a
// single-node replica set from the host needs directConnection, or it hangs until timeout.
const testConnectString = "mongodb://127.0.0.1:27017/?directConnection=true&replicaSet=rs0"

// testEnvironment is one throwaway database, plus the configuration file that points at it.
// Both are discarded when the test finishes.
type testEnvironment struct {
	client     *mongo.Client
	database   string
	collection *mongo.Collection
	statePath  string
	configPath string
}

// newTestEnvironment creates an isolated database, and skips the test when MongoDB is not running
func newTestEnvironment(t *testing.T) testEnvironment {

	t.Helper()

	if testing.Short() {
		t.Skip("skipping a test that needs MongoDB")
	}

	connectString := os.Getenv("TRIAGE_TEST_URI")

	if connectString == "" {
		connectString = testConnectString
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connectString))

	if err != nil {
		t.Skip("MongoDB is not available:", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		t.Skip("MongoDB is not responding:", err)
	}

	// RULE: Every test gets its own database, so a failure can never reach the real error log
	database := "triage_test_" + primitive.NewObjectID().Hex()

	t.Cleanup(func() {
		derpReport(client.Database(database).Drop(context.Background()))
		derpReport(client.Disconnect(context.Background()))
	})

	environment := testEnvironment{
		client:     client,
		database:   database,
		collection: client.Database(database).Collection(collectionName),
		statePath:  filepath.Join(t.TempDir(), "decisions.json"),
		configPath: filepath.Join(t.TempDir(), configFileName),
	}

	// Every command reads its settings from a file, so each test writes its own
	require.NoError(t, writeConfig(environment.configPath, environment.Config()))

	return environment
}

// derpReport swallows a cleanup error that nothing can act on
func derpReport(err error) {
	_ = err
}

// Config returns the resolved configuration for this environment
func (environment testEnvironment) Config() Config {
	return Config{
		ConnectString: testConnectString,
		Database:      environment.database,
		StateFile:     environment.statePath,
		ScanLimit:     defaultScanLimit,
	}
}

// Arguments returns the command-line flags that point a subcommand at this environment
func (environment testEnvironment) Arguments(extra ...string) []string {
	return append([]string{"-config", environment.configPath}, extra...)
}

// Insert writes one error record, and returns its ID
func (environment testEnvironment) Insert(t *testing.T, statusCode int, rootLocation string, rootMessage string, age time.Duration) primitive.ObjectID {

	t.Helper()

	recordID := primitive.NewObjectID()

	document := bson.M{
		"_id":        recordID,
		"statusCode": statusCode,
		"location":   rootLocation,
		"message":    rootMessage,
		"createDate": time.Now().Add(-age),
		"error": bson.M{
			"code":     statusCode,
			"location": "handler.Outer",
			"message":  "Something went wrong",
			"details":  bson.A{"url: https://x.com/a?secret=hunter2"},
			"wrappedvalue": bson.M{
				"code":         statusCode,
				"location":     rootLocation,
				"message":      rootMessage,
				"wrappedvalue": bson.M{"request": bson.M{"header": bson.M{"Signature": bson.A{"secret-blob"}}}},
			},
		},
	}

	_, err := environment.collection.InsertOne(t.Context(), document)
	require.NoError(t, err)

	return recordID
}

// Count returns how many records are left in the error log
func (environment testEnvironment) Count(t *testing.T) int64 {

	t.Helper()

	total, err := environment.collection.CountDocuments(t.Context(), bson.M{})
	require.NoError(t, err)

	return total
}

// capture runs a command and returns whatever it printed to stdout
func capture(t *testing.T, action func() error) (string, error) {

	t.Helper()

	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	original := os.Stdout
	os.Stdout = writer

	output := make(chan string, 1)

	go func() {
		contents, _ := io.ReadAll(reader)
		output <- string(contents)
	}()

	actionError := action()

	os.Stdout = original
	require.NoError(t, writer.Close())

	contents := <-output
	require.NoError(t, reader.Close())

	return contents, actionError
}

// decodeOutput parses captured JSON output into the requested shape
func decodeOutput[T any](t *testing.T, contents string) T {

	t.Helper()

	result := *new(T)
	require.NoError(t, json.Unmarshal([]byte(contents), &result), "decoding %q", contents)

	return result
}

func TestIntegration_ConnectAndLatest(t *testing.T) {

	environment := newTestEnvironment(t)
	environment.Insert(t, 500, "service.A", "broken", time.Minute)
	environment.Insert(t, 403, "remote.Transaction.Send", "403 Forbidden", 2*time.Minute)

	store, err := Connect(t.Context(), environment.Config())
	require.NoError(t, err)
	defer store.Close(context.Background())

	records, err := store.Latest(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, records, 2)

	assert.Equal(t, "service.A", records[0].Location, "the newest record comes first")
}

func TestIntegration_ConnectRejectsADeadServer(t *testing.T) {

	if testing.Short() {
		t.Skip("skipping a test that needs the network")
	}

	config := Config{
		ConnectString: "mongodb://127.0.0.1:1/?directConnection=true&serverSelectionTimeoutMS=250",
		Database:      "nothing",
	}

	_, err := Connect(t.Context(), config)
	assert.Error(t, err, "mongo.Connect is lazy, so only the Ping proves the server is there")
}

func TestIntegration_ByID(t *testing.T) {

	environment := newTestEnvironment(t)
	recordID := environment.Insert(t, 500, "service.A", "broken", time.Minute)

	store, err := Connect(t.Context(), environment.Config())
	require.NoError(t, err)
	defer store.Close(context.Background())

	record, err := store.ByID(t.Context(), recordID.Hex())
	require.NoError(t, err)
	assert.Equal(t, "service.A", record.Location)

	_, err = store.ByID(t.Context(), primitive.NewObjectID().Hex())
	assert.Error(t, err, "a purged record is simply gone")

	_, err = store.ByID(t.Context(), "not-an-id")
	assert.Error(t, err)
}

func TestIntegration_FindFingerprintAndDelete(t *testing.T) {

	environment := newTestEnvironment(t)

	// Three occurrences of one error, and one of another
	first := environment.Insert(t, 524, "data.Query", "Timeout exceeded", time.Minute)
	environment.Insert(t, 524, "data.Query", "Timeout exceeded", 2*time.Minute)
	environment.Insert(t, 524, "data.Query", "Timeout exceeded", 3*time.Minute)
	environment.Insert(t, 500, "service.B", "different", 4*time.Minute)

	store, err := Connect(t.Context(), environment.Config())
	require.NoError(t, err)
	defer store.Close(context.Background())

	record, err := store.ByID(t.Context(), first.Hex())
	require.NoError(t, err)

	// The criteria narrows the scan to records that COULD share the fingerprint
	match, err := store.FindFingerprint(t.Context(), record.Fingerprint(), record.Criteria(), 100)
	require.NoError(t, err)

	assert.Equal(t, 3, match.Scanned, "the criteria excluded the unrelated record")
	require.Len(t, match.Records, 3)

	deleted, err := store.Delete(t.Context(), match.RecordIDs())
	require.NoError(t, err)

	assert.Equal(t, int64(3), deleted)
	assert.Equal(t, int64(1), environment.Count(t), "the unrelated error survives")
}

func TestIntegration_FindFingerprintScansEverything(t *testing.T) {

	environment := newTestEnvironment(t)
	recordID := environment.Insert(t, 500, "service.A", "broken", time.Minute)
	environment.Insert(t, 500, "service.B", "other", 2*time.Minute)

	store, err := Connect(t.Context(), environment.Config())
	require.NoError(t, err)
	defer store.Close(context.Background())

	record, err := store.ByID(t.Context(), recordID.Hex())
	require.NoError(t, err)

	match, err := store.FindFingerprint(t.Context(), record.Fingerprint(), bson.M{}, 100)
	require.NoError(t, err)

	assert.Equal(t, 2, match.Scanned, "an unfiltered search reads the whole log")
	assert.Len(t, match.Records, 1, "but only one record matches")
}

func TestIntegration_Watch(t *testing.T) {

	environment := newTestEnvironment(t)

	store, err := Connect(t.Context(), environment.Config())
	require.NoError(t, err)
	defer store.Close(context.Background())

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	received := make(chan Record, 4)
	finished := make(chan error, 1)

	go func() {
		finished <- store.Watch(ctx, func(record Record) {
			received <- record
		})
	}()

	// Give the change stream a moment to open before writing anything it must see
	time.Sleep(time.Second)
	environment.Insert(t, 500, "service.Watched", "broken", 0)

	select {

	case record := <-received:
		assert.Equal(t, "service.Watched", record.Location)

	case <-ctx.Done():
		t.Fatal("the change stream never delivered the inserted record")
	}

	cancel()
	assert.NoError(t, <-finished, "a cancelled watch ends cleanly")
}

func TestIntegration_NextAndFixedDrainTheQueue(t *testing.T) {

	environment := newTestEnvironment(t)
	environment.Insert(t, 524, "data.Query", "Timeout exceeded", time.Minute)
	environment.Insert(t, 524, "data.Query", "Timeout exceeded", 2*time.Minute)
	environment.Insert(t, 500, "service.B", "different", 3*time.Minute)
	environment.Insert(t, 403, "remote.Transaction.Send", "403 Forbidden", 4*time.Minute)

	// One error to work on, with the remote failure filtered out
	contents, err := capture(t, func() error { return runNext(t.Context(), environment.Arguments()) })
	require.NoError(t, err)

	backlog := decodeOutput[Backlog](t, contents)

	assert.Equal(t, 4, backlog.Scanned)
	assert.Equal(t, 2, backlog.Remaining, "two distinct errors are ours")
	require.NotNil(t, backlog.Next)
	assert.Equal(t, "data.Query: Timeout exceeded", backlog.Next.RootCause)
	assert.NotContains(t, contents, "hunter2", "a secret never reaches the output")
	assert.NotContains(t, contents, "secret-blob")

	// Fixing it deletes every occurrence
	contents, err = capture(t, func() error {
		return runDecide(t.Context(), environment.Arguments(backlog.Next.RecordID, "-note", "indexed it"), DispositionFixed)
	})
	require.NoError(t, err)

	outcome := decodeOutput[Outcome](t, contents)

	assert.Equal(t, 2, outcome.Matched)
	assert.Equal(t, int64(2), outcome.Deleted)
	require.NotNil(t, outcome.Decision)
	assert.Equal(t, "indexed it", outcome.Decision.Note)
	assert.Equal(t, int64(2), environment.Count(t))

	// The backlog has shrunk
	contents, err = capture(t, func() error { return runNext(t.Context(), environment.Arguments()) })
	require.NoError(t, err)

	assert.Equal(t, 1, decodeOutput[Backlog](t, contents).Remaining)
}

func TestIntegration_DryRunChangesNothing(t *testing.T) {

	environment := newTestEnvironment(t)
	recordID := environment.Insert(t, 500, "service.A", "broken", time.Minute)

	contents, err := capture(t, func() error {
		return runDecide(t.Context(), environment.Arguments(recordID.Hex(), "-dry-run"), DispositionFixed)
	})
	require.NoError(t, err)

	outcome := decodeOutput[Outcome](t, contents)

	assert.True(t, outcome.DryRun)
	assert.Equal(t, 1, outcome.Matched)
	assert.Equal(t, int64(0), outcome.Deleted)
	assert.Nil(t, outcome.Decision, "a dry run records no decision")
	assert.Equal(t, int64(1), environment.Count(t), "the record is still there")

	// And nothing was written to the decision file
	decisions, err := LoadDecisions(environment.statePath)
	require.NoError(t, err)
	assert.Empty(t, decisions.All())
}

func TestIntegration_IgnoreKeepsTheRecords(t *testing.T) {

	environment := newTestEnvironment(t)
	recordID := environment.Insert(t, 500, "service.A", "broken", time.Minute)

	contents, err := capture(t, func() error {
		return runDecide(t.Context(), environment.Arguments(recordID.Hex(), "-note", "upstream"), DispositionIgnored)
	})
	require.NoError(t, err)

	outcome := decodeOutput[Outcome](t, contents)

	assert.Equal(t, int64(0), outcome.Deleted, "an ignored error keeps its records as evidence")
	assert.Equal(t, int64(1), environment.Count(t))

	// It disappears from the queue
	contents, err = capture(t, func() error { return runNext(t.Context(), environment.Arguments()) })
	require.NoError(t, err)
	assert.Equal(t, 0, decodeOutput[Backlog](t, contents).Remaining)

	// Until it is forgotten
	_, err = capture(t, func() error {
		return runForget(environment.Arguments(outcome.Fingerprint))
	})
	require.NoError(t, err)

	contents, err = capture(t, func() error { return runNext(t.Context(), environment.Arguments()) })
	require.NoError(t, err)
	assert.Equal(t, 1, decodeOutput[Backlog](t, contents).Remaining)
}

func TestIntegration_RegressionAfterAFix(t *testing.T) {

	environment := newTestEnvironment(t)
	recordID := environment.Insert(t, 500, "service.A", "broken", time.Minute)

	_, err := capture(t, func() error {
		return runDecide(t.Context(), environment.Arguments(recordID.Hex()), DispositionFixed)
	})
	require.NoError(t, err)
	require.Equal(t, int64(0), environment.Count(t))

	// The same defect happens again, AFTER it was recorded as fixed
	environment.Insert(t, 500, "service.A", "broken", 0)

	contents, err := capture(t, func() error { return runNext(t.Context(), environment.Arguments()) })
	require.NoError(t, err)

	backlog := decodeOutput[Backlog](t, contents)

	require.NotNil(t, backlog.Next)
	assert.True(t, backlog.Next.Regression, "an error that comes back after a fix is always news")
}

func TestIntegration_FixedByFingerprint(t *testing.T) {

	environment := newTestEnvironment(t)
	recordID := environment.Insert(t, 500, "service.A", "broken", time.Minute)

	store, err := Connect(t.Context(), environment.Config())
	require.NoError(t, err)

	record, err := store.ByID(t.Context(), recordID.Hex())
	require.NoError(t, err)
	store.Close(context.Background())

	contents, err := capture(t, func() error {
		return runDecide(t.Context(), environment.Arguments(record.Fingerprint()), DispositionFixed)
	})
	require.NoError(t, err)

	assert.Equal(t, int64(1), decodeOutput[Outcome](t, contents).Deleted)
	assert.Equal(t, int64(0), environment.Count(t))
}

func TestIntegration_DecideRejectsBadTargets(t *testing.T) {

	environment := newTestEnvironment(t)

	tests := map[string][]string{
		"no target":           {},
		"two targets":         {"aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb"},
		"wrong length":        {"abc"},
		"unknown fingerprint": {"aaaaaaaaaaaa"},
		"unknown record":      {primitive.NewObjectID().Hex()},
	}

	for name, target := range tests {
		_, err := capture(t, func() error {
			return runDecide(t.Context(), environment.Arguments(target...), DispositionFixed)
		})
		assert.Error(t, err, "a %s must be rejected", name)
	}
}

func TestIntegration_LatestShowAndDecisions(t *testing.T) {

	environment := newTestEnvironment(t)
	recordID := environment.Insert(t, 500, "service.A", "broken", time.Minute)

	// latest
	contents, err := capture(t, func() error { return runLatest(t.Context(), environment.Arguments()) })
	require.NoError(t, err)

	reports := decodeOutput[[]Report](t, contents)
	require.Len(t, reports, 1)
	assert.Equal(t, "handler.Outer", reports[0].Origin)

	// show
	contents, err = capture(t, func() error { return runShow(t.Context(), environment.Arguments(recordID.Hex())) })
	require.NoError(t, err)

	report := decodeOutput[Report](t, contents)
	assert.Equal(t, recordID.Hex(), report.RecordID)
	assert.Len(t, report.Chain, 2)
	assert.NotContains(t, contents, "hunter2")

	// show with no argument
	_, err = capture(t, func() error { return runShow(t.Context(), environment.Arguments()) })
	assert.Error(t, err)

	// decisions, before anything is decided
	contents, err = capture(t, func() error { return runDecisions(environment.Arguments()) })
	require.NoError(t, err)
	assert.Empty(t, decodeOutput[[]Decision](t, contents))
}

func TestIntegration_LatestRejectsASillyLimit(t *testing.T) {

	environment := newTestEnvironment(t)

	for _, limit := range []string{"0", "-5", "100001"} {
		_, err := capture(t, func() error { return runLatest(t.Context(), environment.Arguments("-n", limit)) })
		assert.Error(t, err, "a limit of %s must be rejected", limit)
	}
}

func TestIntegration_NextOldestFirst(t *testing.T) {

	environment := newTestEnvironment(t)
	environment.Insert(t, 500, "service.Newest", "broken", time.Minute)
	environment.Insert(t, 500, "service.Oldest", "broken", time.Hour)

	contents, err := capture(t, func() error { return runNext(t.Context(), environment.Arguments("-oldest")) })
	require.NoError(t, err)

	backlog := decodeOutput[Backlog](t, contents)
	require.NotNil(t, backlog.Next)
	assert.Equal(t, "service.Oldest: broken", backlog.Next.RootCause)
}

func TestIntegration_NextOnAnEmptyLog(t *testing.T) {

	environment := newTestEnvironment(t)

	contents, err := capture(t, func() error { return runNext(t.Context(), environment.Arguments()) })
	require.NoError(t, err)

	backlog := decodeOutput[Backlog](t, contents)

	assert.Equal(t, 0, backlog.Remaining)
	assert.Nil(t, backlog.Next, "an empty queue is reported, not invented")
}

func TestIntegration_ForgetRejectsAnUnknownFingerprint(t *testing.T) {

	environment := newTestEnvironment(t)

	_, err := capture(t, func() error { return runForget(environment.Arguments("aaaaaaaaaaaa")) })
	assert.Error(t, err)

	_, err = capture(t, func() error { return runForget(environment.Arguments()) })
	assert.Error(t, err)
}

func TestIntegration_WatchStopsWhenCancelled(t *testing.T) {

	environment := newTestEnvironment(t)

	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)

	// RULE: never call require from a goroutine, so the watch runs without the stdout capture.
	// It prints nothing here, because no records are inserted.
	go func() {
		finished <- runWatch(ctx, environment.Arguments())
	}()

	// Let the watch open, then interrupt it the way Ctrl-C would
	time.Sleep(time.Second)
	cancel()

	select {

	case err := <-finished:
		assert.NoError(t, err, "an interrupted watch is a normal exit, not a failure")

	case <-time.After(20 * time.Second):
		t.Fatal("the watch did not stop when its context was cancelled")
	}
}
