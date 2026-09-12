package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/benpate/derp"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// maxFrames bounds how far the derp chain is walked, so a self-referential record cannot loop
const maxFrames = 32

// Record is one error report, exactly as tools/derp-mongo writes it into MongoDB
type Record struct {
	RecordID   primitive.ObjectID `bson:"_id"`
	StatusCode int                `bson:"statusCode"`
	Location   string             `bson:"location"`
	Message    string             `bson:"message"`
	Error      bson.M             `bson:"error"`
	CreateDate primitive.DateTime `bson:"createDate"`
}

// Analyze reduces a stored Record into a classified, fingerprinted, redacted Report
func (record Record) Analyze() Report {

	origin, rootLocation, rootMessage := record.identity()
	chain, payload := walkFrames(record.Error)

	return Report{
		RecordID:    record.RecordID.Hex(),
		CreateDate:  record.CreateDate.Time().UTC().Format(time.RFC3339),
		StatusCode:  record.StatusCode,
		Category:    Categorize(rootLocation, rootMessage),
		Fingerprint: Fingerprint(record.StatusCode, origin, rootLocation, rootMessage),
		Origin:      origin,
		RootCause:   RedactString(rootLocation + ": " + rootMessage),
		Chain:       chain,
		Payload:     payload,
	}
}

// Fingerprint returns the stable identity of a Record, without the cost of building a full Report
func (record Record) Fingerprint() string {
	origin, rootLocation, rootMessage := record.identity()
	return Fingerprint(record.StatusCode, origin, rootLocation, rootMessage)
}

// Criteria returns a database filter that matches every record this one could share a
// fingerprint with, so that a search does not have to read the whole collection.
func (record Record) Criteria() bson.M {
	return bson.M{
		"statusCode": record.StatusCode,
		"location":   record.Location,
	}
}

// identity returns the outermost location, plus the root location and message, that together
// identify an error.  Analyze and Fingerprint must always agree, so both read it from here.
func (record Record) identity() (string, string, string) {

	origin := record.Location
	rootLocation := record.Location
	rootMessage := record.Message

	outer, isObject := toObject(record.Error)

	if !isObject {
		return origin, rootLocation, rootMessage
	}

	// The outermost frame says where in Emissary the failure surfaced
	if location, isString := outer["location"].(string); isString {
		origin = location
	}

	// RULE: tools/derp-mongo stores the root cause at the top of the record.  Only a truncated
	// or hand-written record lacks it, and then the deepest frame is the next best answer.
	if rootLocation != "" {
		return origin, rootLocation, rootMessage
	}

	deepest := outer

	for range maxFrames {

		next, isObject := toObject(deepest["wrappedvalue"])

		if !isObject {
			break
		}

		if _, isFrame := next["location"].(string); !isFrame {
			break
		}

		deepest = next
	}

	return origin, toString(deepest["location"]), toString(deepest["message"])
}

// walkFrames unpacks a derp error chain from the outermost frame down to the root, and returns
// whatever non-derp value the chain finally wrapped.
func walkFrames(value any) ([]Frame, any) {

	frames := make([]Frame, 0, 8)

	for range maxFrames {

		object, isObject := toObject(value)

		// A value that is not a document ends the chain
		if !isObject {
			return frames, redactPayload(value)
		}

		// RULE: A derp frame is identified by its "location" field.  Anything else is the
		// wrapped error that derp could not unpack, and belongs in the payload.
		location, isFrame := object["location"].(string)

		if !isFrame {
			return frames, redactPayload(value)
		}

		frames = append(frames, Frame{
			Code:     toInt(object["code"]),
			Location: location,
			Message:  RedactString(toString(object["message"])),
			Details:  frameDetails(object["details"]),
		})

		value = object["wrappedvalue"]
	}

	return frames, redactPayload(value)
}

// redactPayload strips secrets from the non-derp tail of an error chain, and drops it entirely
// when there is nothing left worth printing.
func redactPayload(value any) any {

	if value == nil {
		return nil
	}

	if object, isObject := toObject(value); isObject && len(object) == 0 {
		return nil
	}

	return Redact(value)
}

// frameDetails converts a derp frame's details into redacted, readable strings
func frameDetails(value any) []string {

	entries, isSlice := toSlice(value)

	if !isSlice {
		return nil
	}

	details := make([]string, 0, len(entries))

	for index, entry := range entries {

		if index >= maxSliceLength {
			details = append(details, fmt.Sprintf("...[%d more]", len(entries)-index))
			break
		}

		details = append(details, detailString(entry))
	}

	return details
}

// detailString renders one detail value, redacting whatever it happens to contain
func detailString(value any) string {

	if text, isString := value.(string); isString {
		return RedactString(text)
	}

	encoded, err := json.Marshal(Redact(value))

	// A value the JSON encoder rejects still has a readable Go rendering
	if err != nil {
		return RedactString(fmt.Sprint(value))
	}

	return Truncate(string(encoded), maxStringLength)
}

// toObject converts a decoded BSON value into a plain map, whichever map type the driver used
func toObject(value any) (map[string]any, bool) {

	switch typed := value.(type) {

	case bson.M:
		return typed, typed != nil

	case map[string]any:
		return typed, typed != nil
	}

	return nil, false
}

// toSlice converts a decoded BSON value into a plain slice, whichever slice type the driver used
func toSlice(value any) ([]any, bool) {

	switch typed := value.(type) {

	case bson.A:
		return typed, typed != nil

	case []any:
		return typed, typed != nil
	}

	return nil, false
}

// toInt converts any of the numeric types the BSON decoder produces into an int
func toInt(value any) int {

	switch typed := value.(type) {

	case int:
		return typed

	case int32:
		return int(typed)

	case int64:
		return int(typed)

	case float64:
		return int(typed)
	}

	return 0
}

// toString returns a decoded BSON value as a string, or empty when it is not one
func toString(value any) string {

	if text, isString := value.(string); isString {
		return text
	}

	return ""
}

// objectIDFromHex parses a hexadecimal record ID into a MongoDB ObjectID
func objectIDFromHex(recordID string) (primitive.ObjectID, error) {

	const location = "triage.objectIDFromHex"

	objectID, err := primitive.ObjectIDFromHex(recordID)

	if err != nil {
		return primitive.NilObjectID, derp.BadRequest(location, "Record ID must be 24 hexadecimal characters", recordID)
	}

	return objectID, nil
}
