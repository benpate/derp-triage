package main

import (
	"context"
	"errors"

	"github.com/benpate/derp"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Store reads error records from Emissary's shared MongoDB error log
type Store struct {
	client     *mongo.Client
	collection *mongo.Collection
}

// Connect opens a connection to the error log described by the provided Config
func Connect(ctx context.Context, config Config) (Store, error) {

	const location = "triage.Connect"

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(config.ConnectString))

	if err != nil {
		return Store{}, derp.Internal(location, "Connecting to database", config.ConnectString, err.Error())
	}

	// RULE: mongo.Connect is lazy, so a Ping is the only thing that proves the server is there
	if err := client.Ping(ctx, nil); err != nil {
		return Store{}, derp.Internal(location, "Database is not responding", config.ConnectString, err.Error())
	}

	return Store{
		client:     client,
		collection: client.Database(config.Database).Collection(collectionName),
	}, nil
}

// Close releases the database connection
func (store Store) Close(ctx context.Context) {
	derp.Report(store.client.Disconnect(ctx))
}

// Latest returns the most recent error records, newest first
func (store Store) Latest(ctx context.Context, limit int64) ([]Record, error) {

	const location = "triage.Store.Latest"

	query := options.Find().
		SetSort(bson.D{{Key: "createDate", Value: -1}, {Key: "_id", Value: -1}}).
		SetLimit(limit)

	cursor, err := store.collection.Find(ctx, bson.M{}, query)

	if err != nil {
		return nil, derp.Internal(location, "Querying error log", err.Error())
	}

	return decodeAll(ctx, cursor)
}

// ByID returns a single error record, identified by its hexadecimal record ID
func (store Store) ByID(ctx context.Context, recordID string) (Record, error) {

	const location = "triage.Store.ByID"

	objectID, err := objectIDFromHex(recordID)

	if err != nil {
		return Record{}, derp.Wrap(err, location, "Invalid record ID", recordID)
	}

	result := Record{}

	if err := store.collection.FindOne(ctx, bson.M{"_id": objectID}).Decode(&result); err != nil {

		// A record that has aged past the seven-day purge is simply gone
		if errors.Is(err, mongo.ErrNoDocuments) {
			return Record{}, derp.NotFound(location, "Error record not found.  Records are purged after seven days", recordID)
		}

		return Record{}, derp.Internal(location, "Loading error record", recordID, err.Error())
	}

	return result, nil
}

// Watch streams every error record inserted from now on, calling the handler for each one.
// It returns only when the context is cancelled, or the change stream fails.
func (store Store) Watch(ctx context.Context, handler func(Record)) error {

	const location = "triage.Store.Watch"

	// RULE: Only insertions matter.  Nothing in Emissary ever updates an error record.
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"operationType": "insert"}}},
	}

	stream, err := store.collection.Watch(ctx, pipeline)

	if err != nil {
		return derp.Internal(location, "Opening change stream.  This requires a MongoDB replica set", err.Error())
	}

	defer func() {
		derp.Report(stream.Close(context.Background()))
	}()

	for stream.Next(ctx) {

		event := struct {
			FullDocument Record `bson:"fullDocument"`
		}{}

		// A record this tool cannot decode must not stop the watch
		if err := stream.Decode(&event); err != nil {
			derp.Report(derp.Internal(location, "Skipping undecodable error record", err.Error()))
			continue
		}

		handler(event.FullDocument)
	}

	if err := stream.Err(); err != nil {

		// RULE: Cancelling the context is how a watch is meant to end, so it is not a failure
		if ctx.Err() != nil {
			return nil
		}

		return derp.Internal(location, "Reading change stream", err.Error())
	}

	// And they watched happily ever after
	return nil
}

// decodeAll reads every record from a cursor, skipping any that cannot be decoded
func decodeAll(ctx context.Context, cursor *mongo.Cursor) ([]Record, error) {

	const location = "triage.decodeAll"

	defer func() {
		derp.Report(cursor.Close(context.Background()))
	}()

	records := make([]Record, 0)

	for cursor.Next(ctx) {

		record := Record{}

		// RULE: One malformed record must not hide every other error in the log
		if err := cursor.Decode(&record); err != nil {
			derp.Report(derp.Internal(location, "Skipping undecodable error record", err.Error()))
			continue
		}

		records = append(records, record)
	}

	if err := cursor.Err(); err != nil {
		return records, derp.Internal(location, "Reading error log", err.Error())
	}

	return records, nil
}

// Match is every error record in the log that shares one fingerprint
type Match struct {
	Fingerprint string   `json:"fingerprint"`
	Scanned     int      `json:"scanned"`
	Records     []Record `json:"-"`
}

// RecordIDs returns the database keys of every matched record
func (match Match) RecordIDs() []primitive.ObjectID {

	recordIDs := make([]primitive.ObjectID, 0, len(match.Records))

	for _, record := range match.Records {
		recordIDs = append(recordIDs, record.RecordID)
	}

	return recordIDs
}

// FindFingerprint scans the error log for every record that shares a fingerprint.  The criteria
// narrows the scan whenever the caller already knows the status code and root location.
func (store Store) FindFingerprint(ctx context.Context, fingerprint string, criteria bson.M, limit int64) (Match, error) {

	const location = "triage.Store.FindFingerprint"

	query := options.Find().
		SetSort(bson.D{{Key: "createDate", Value: -1}, {Key: "_id", Value: -1}}).
		SetLimit(limit)

	cursor, err := store.collection.Find(ctx, criteria, query)

	if err != nil {
		return Match{}, derp.Internal(location, "Searching error log", err.Error())
	}

	candidates, err := decodeAll(ctx, cursor)

	if err != nil {
		return Match{}, derp.Wrap(err, location, "Reading error log")
	}

	match := Match{
		Fingerprint: fingerprint,
		Scanned:     len(candidates),
		Records:     make([]Record, 0, len(candidates)),
	}

	for _, candidate := range candidates {
		if candidate.Fingerprint() == fingerprint {
			match.Records = append(match.Records, candidate)
		}
	}

	return match, nil
}

// Delete removes specific error records, and returns how many were actually deleted
func (store Store) Delete(ctx context.Context, recordIDs []primitive.ObjectID) (int64, error) {

	const location = "triage.Store.Delete"

	// RULE: An empty list must never become a filter that matches every record in the log
	if len(recordIDs) == 0 {
		return 0, nil
	}

	criteria := bson.M{"_id": bson.M{"$in": recordIDs}}

	result, err := store.collection.DeleteMany(ctx, criteria)

	if err != nil {
		return 0, derp.Internal(location, "Deleting error records", err.Error())
	}

	// The driver always returns a result alongside a nil error, but nothing proves it here
	if result == nil {
		return 0, nil
	}

	// Gone, but not forgotten -- the decision file remembers
	return result.DeletedCount, nil
}
