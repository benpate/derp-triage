package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestMatch_RecordIDs(t *testing.T) {

	first := primitive.NewObjectID()
	second := primitive.NewObjectID()

	match := Match{
		Records: []Record{{RecordID: first}, {RecordID: second}},
	}

	assert.Equal(t, []primitive.ObjectID{first, second}, match.RecordIDs())
}

func TestMatch_RecordIDsOfNothing(t *testing.T) {
	assert.Equal(t, []primitive.ObjectID{}, Match{}.RecordIDs())
}

func TestStore_DeleteNothing(t *testing.T) {

	// RULE: An empty list must return before it can become a filter that matches the whole
	// collection.  The nil collection below proves the database is never reached.
	store := Store{}

	deleted, err := store.Delete(t.Context(), nil)

	require.NoError(t, err)
	assert.Equal(t, int64(0), deleted)

	deleted, err = store.Delete(t.Context(), []primitive.ObjectID{})

	require.NoError(t, err)
	assert.Equal(t, int64(0), deleted)
}
