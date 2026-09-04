// Package weaviate is recall's semantic backend. It mirrors the corpus the
// keyword index already holds; it is never the only copy, which is what lets
// a caller fall back to sqlitefts without reconciling anything afterwards.
package weaviate

import "github.com/weaviate/weaviate/entities/models"

// Collection is the one class spore owns. Everything indexed is a chunk, and
// the kind property tells them apart, because a filter over one class is
// cheaper than a query that has to know which of three classes to ask.
const Collection = "SporeChunk"

// vectorizer names the module the compose file starts. It runs as a second
// container: no Weaviate vectorizer runs in-process, and the module-free path
// would mean spore holding an embedding API key.
const vectorizer = "text2vec-model2vec"

// skipVectorizing is the per-property module config for everything that is
// metadata. A session id has no meaning in embedding space, and including it
// would pull hits towards whichever session had the most text.
func skipVectorizing() map[string]any {
	return map[string]any{vectorizer: map[string]any{"skip": true, "vectorizePropertyName": false}}
}

func vectorizeProperty() map[string]any {
	return map[string]any{vectorizer: map[string]any{"skip": false, "vectorizePropertyName": false}}
}

// collectionClass is the schema spore creates on setup. It is written here
// rather than in a JSON fixture so a property added to Chunk fails to compile
// until it is given a vectorization decision.
func collectionClass() *models.Class {
	return &models.Class{
		Class:       Collection,
		Description: "One indexed chunk of spore's corpus: a message, a summary, or a fact.",
		Vectorizer:  vectorizer,
		ModuleConfig: map[string]any{
			vectorizer: map[string]any{"vectorizeClassName": false},
		},
		Properties: []*models.Property{
			{Name: "text", DataType: []string{"text"}, ModuleConfig: vectorizeProperty()},
			{Name: "kind", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
			{Name: "ref_id", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
			{Name: "session_id", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
			{Name: "created_at", DataType: []string{"text"}, ModuleConfig: skipVectorizing()},
		},
	}
}
