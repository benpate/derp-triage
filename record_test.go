package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// newRecord builds a Record the way tools/derp-mongo writes one
func newRecord(statusCode int, rootLocation string, rootMessage string, wrapped bson.M) Record {
	return Record{
		RecordID:   primitive.NewObjectID(),
		StatusCode: statusCode,
		Location:   rootLocation,
		Message:    rootMessage,
		Error:      wrapped,
		CreateDate: primitive.NewDateTimeFromTime(time.Now()),
	}
}

func TestRecord_Analyze(t *testing.T) {

	record := newRecord(500, "remote.Transaction.Send", "403 Forbidden", bson.M{
		"code":     500,
		"location": "consumer.PollFollowing",
		"message":  "Loading Actor",
		"details":  bson.A{"url: https://x.com/a?secret=abc"},
		"wrappedvalue": bson.M{
			"code":         403,
			"location":     "remote.Transaction.Send",
			"message":      "403 Forbidden",
			"wrappedvalue": bson.M{"request": bson.M{"header": bson.M{"Signature": bson.A{"blob"}}}},
		},
	})

	report := record.Analyze()

	assert.Equal(t, record.RecordID.Hex(), report.RecordID)
	assert.Equal(t, 500, report.StatusCode)
	assert.Equal(t, CategoryPeer, report.Category, "a 403 from the far end is not our defect")
	assert.Equal(t, "consumer.PollFollowing", report.Origin, "the origin is the OUTERMOST frame")
	assert.Equal(t, "remote.Transaction.Send: 403 Forbidden", report.RootCause)
	assert.Len(t, report.Fingerprint, fingerprintLength)

	require.Len(t, report.Chain, 2)
	assert.Equal(t, "consumer.PollFollowing", report.Chain[0].Location)
	assert.Equal(t, "remote.Transaction.Send", report.Chain[1].Location)
	assert.Equal(t, []string{"url: https://x.com/a?secret=[REDACTED]"}, report.Chain[0].Details)

	assert.Contains(t, stringify(t, report.Payload), redactedValue)
	assert.NotContains(t, stringify(t, report), "blob", "a signature never reaches the report")
}

func TestRecord_AnalyzeCreateDateIsUTC(t *testing.T) {

	record := newRecord(500, "a", "b", bson.M{"location": "a"})
	report := record.Analyze()

	parsed, err := time.Parse(time.RFC3339, report.CreateDate)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, parsed.Location())
}

func TestRecord_IdentityPrefersStoredRoot(t *testing.T) {

	record := newRecord(500, "stored.Root", "stored message", bson.M{
		"location":     "outer.Frame",
		"message":      "outer message",
		"wrappedvalue": bson.M{"location": "deepest.Frame", "message": "deepest message"},
	})

	origin, rootLocation, rootMessage := record.identity()

	assert.Equal(t, "outer.Frame", origin)
	assert.Equal(t, "stored.Root", rootLocation)
	assert.Equal(t, "stored message", rootMessage)
}

func TestRecord_IdentityFallsBackToDeepestFrame(t *testing.T) {

	// A record with no stored root must still identify its own root cause
	record := newRecord(500, "", "", bson.M{
		"location":     "outer.Frame",
		"message":      "outer message",
		"wrappedvalue": bson.M{"location": "deepest.Frame", "message": "deepest message"},
	})

	origin, rootLocation, rootMessage := record.identity()

	assert.Equal(t, "outer.Frame", origin)
	assert.Equal(t, "deepest.Frame", rootLocation)
	assert.Equal(t, "deepest message", rootMessage)
}

func TestRecord_IdentityWithoutAnError(t *testing.T) {

	record := newRecord(500, "stored.Root", "stored message", nil)
	origin, rootLocation, rootMessage := record.identity()

	assert.Equal(t, "stored.Root", origin)
	assert.Equal(t, "stored.Root", rootLocation)
	assert.Equal(t, "stored message", rootMessage)
}

func TestRecord_FingerprintMatchesAnalyze(t *testing.T) {

	// The cheap path and the full path must never disagree, or "fixed" would delete the
	// wrong records -- or none at all.
	records := []Record{
		newRecord(500, "a.B", "broken", bson.M{"location": "outer.C", "wrappedvalue": bson.M{"location": "a.B"}}),
		newRecord(404, "", "", bson.M{"location": "only.Frame", "message": "gone"}),
		newRecord(0, "", "", nil),
	}

	for _, record := range records {
		assert.Equal(t, record.Analyze().Fingerprint, record.Fingerprint())
	}
}

func TestRecord_Criteria(t *testing.T) {

	record := newRecord(524, "data-mongo.Collection.Query", "Timeout exceeded", nil)

	assert.Equal(t, bson.M{"statusCode": 524, "location": "data-mongo.Collection.Query"}, record.Criteria())
}

func TestWalkFrames_BoundsACycle(t *testing.T) {

	// A record that nests further than the walker follows must terminate
	deepest := bson.M{"location": "bottom.Frame"}

	for range maxFrames * 2 {
		deepest = bson.M{"location": "frame", "wrappedvalue": deepest}
	}

	frames, _ := walkFrames(deepest)

	assert.Len(t, frames, maxFrames)
}

func TestWalkFrames_EmptyAndMissing(t *testing.T) {

	frames, payload := walkFrames(nil)
	assert.Empty(t, frames)
	assert.Nil(t, payload)

	frames, payload = walkFrames(bson.M{})
	assert.Empty(t, frames)
	assert.Nil(t, payload, "an empty tail is dropped rather than printed")

	frames, payload = walkFrames("not a document")
	assert.Empty(t, frames)
	assert.Equal(t, "not a document", payload)
}

func TestWalkFrames_StopsAtANonFrame(t *testing.T) {

	frames, payload := walkFrames(bson.M{
		"location":     "outer.Frame",
		"wrappedvalue": bson.M{"err": bson.M{}, "name": "follower-unsubscribe"},
	})

	require.Len(t, frames, 1)
	assert.Equal(t, "follower-unsubscribe", payload.(map[string]any)["name"])
}

func TestFrameDetails(t *testing.T) {

	assert.Nil(t, frameDetails(nil))
	assert.Nil(t, frameDetails("not a slice"))
	assert.Equal(t, []string{}, frameDetails(bson.A{}))

	details := frameDetails(bson.A{
		"https://x.com?token=abc",
		42,
		bson.M{"Cookie": "session=1"},
		nil,
	})

	require.Len(t, details, 4)
	assert.Equal(t, "https://x.com?token=[REDACTED]", details[0])
	assert.Equal(t, "42", details[1])
	assert.Contains(t, details[2], redactedValue)
	assert.Equal(t, "null", details[3])
}

func TestFrameDetails_BoundsWidth(t *testing.T) {

	wide := make(bson.A, 0, maxSliceLength*2)

	for index := range maxSliceLength * 2 {
		wide = append(wide, index)
	}

	details := frameDetails(wide)

	require.Len(t, details, maxSliceLength+1)
	assert.Equal(t, "...[20 more]", details[maxSliceLength])
}

func TestDetailString_TruncatesLongValues(t *testing.T) {

	long := detailString(bson.M{"key": strings.Repeat("x", maxStringLength*3)})
	assert.Contains(t, long, "...[truncated]")
}

func TestToObject(t *testing.T) {

	object, ok := toObject(bson.M{"a": 1})
	assert.True(t, ok)
	assert.Equal(t, 1, object["a"])

	object, ok = toObject(map[string]any{"a": 1})
	assert.True(t, ok)
	assert.Equal(t, 1, object["a"])

	_, ok = toObject(nil)
	assert.False(t, ok)

	_, ok = toObject("string")
	assert.False(t, ok)

	_, ok = toObject(bson.A{})
	assert.False(t, ok)
}

func TestToSlice(t *testing.T) {

	entries, ok := toSlice(bson.A{1, 2})
	assert.True(t, ok)
	assert.Len(t, entries, 2)

	entries, ok = toSlice([]any{1})
	assert.True(t, ok)
	assert.Len(t, entries, 1)

	_, ok = toSlice(nil)
	assert.False(t, ok)

	_, ok = toSlice("string")
	assert.False(t, ok)
}

func TestToInt(t *testing.T) {

	assert.Equal(t, 7, toInt(7))
	assert.Equal(t, 7, toInt(int32(7)))
	assert.Equal(t, 7, toInt(int64(7)))
	assert.Equal(t, 7, toInt(7.9))
	assert.Equal(t, 0, toInt(nil))
	assert.Equal(t, 0, toInt("7"))
	assert.Equal(t, -1, toInt(int32(-1)))
}

func TestToString(t *testing.T) {

	assert.Equal(t, "a", toString("a"))
	assert.Equal(t, "", toString(nil))
	assert.Equal(t, "", toString(7))
}

func TestObjectIDFromHex(t *testing.T) {

	objectID, err := objectIDFromHex("6aa17bd9a102749e24915ab4")
	require.NoError(t, err)
	assert.Equal(t, "6aa17bd9a102749e24915ab4", objectID.Hex())

	for _, invalid := range []string{"", "nope", "6aa17bd9a102749e24915ab", "ZZa17bd9a102749e24915ab4"} {
		_, err := objectIDFromHex(invalid)
		assert.Error(t, err, "%q must be rejected", invalid)
	}
}
