package weaviate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/codered/spore/internal/recall"
)

func TestObjectIDIsStableAndDistinct(t *testing.T) {
	a := objectID(recall.KindMessage, "17")
	if a == "" {
		t.Fatal("empty id")
	}
	if b := objectID(recall.KindMessage, "17"); a != b {
		t.Errorf("the same chunk produced two ids: %s and %s", a, b)
	}
	// A stable id is what makes a re-sent batch an overwrite rather than a
	// duplicate, so this property carries the whole sync design.
	if objectID(recall.KindFact, "17") == a {
		t.Error("a fact and a message with the same ref share an id")
	}
	if objectID(recall.KindMessage, "18") == a {
		t.Error("two messages share an id")
	}
}

func TestChunkObjectCarriesEveryFilterableField(t *testing.T) {
	when := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	obj := chunkObject(recall.Chunk{
		ID: "42", Kind: recall.KindMessage, Text: "hello", SessionID: "sess-1", CreatedAt: when,
	})
	if obj.Class != Collection {
		t.Errorf("class %q, want %q", obj.Class, Collection)
	}
	props, ok := obj.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties are %T, want a map", obj.Properties)
	}
	for field, want := range map[string]any{
		"text": "hello", "kind": recall.KindMessage, "ref_id": "42", "session_id": "sess-1",
	} {
		if props[field] != want {
			t.Errorf("property %s = %v, want %v", field, props[field], want)
		}
	}
	if props["created_at"] != when.Format(time.RFC3339) {
		t.Errorf("created_at = %v, want an RFC3339 string", props["created_at"])
	}
}

func TestCollectionVectorizesTextAndNothingElse(t *testing.T) {
	class := collectionClass()
	if class.Vectorizer != vectorizer {
		t.Errorf("vectorizer %q, want %q", class.Vectorizer, vectorizer)
	}
	// Metadata in the vector is noise: a session id has no meaning in
	// embedding space and would drag every hit towards whichever session
	// happened to be long.
	seen := 0
	for _, p := range class.Properties {
		seen++
		cfg, _ := p.ModuleConfig.(map[string]any)
		mod, _ := cfg[vectorizer].(map[string]any)
		skip, _ := mod["skip"].(bool)
		if p.Name == "text" && skip {
			t.Error("the text property is skipped, so nothing is vectorized")
		}
		if p.Name != "text" && !skip {
			t.Errorf("property %q is vectorized, want it skipped", p.Name)
		}
	}
	if seen != 5 {
		t.Errorf("collection has %d properties, want 5", seen)
	}
}

func TestWhereFilterMatchesTheQueryScope(t *testing.T) {
	if whereFilter(recall.Query{Text: "x"}) != nil {
		t.Error("an unscoped query built a filter")
	}
	// The remote profile narrows to one session and drops facts; that scoping
	// is the tool's, and this is the half that must survive the translation.
	f := whereFilter(recall.Query{Text: "x", SessionID: "sess-1", Kinds: []string{recall.KindMessage}})
	if f == nil {
		t.Fatal("a scoped query built no filter")
	}
	built := f.String()
	if !strings.Contains(built, "sess-1") {
		t.Errorf("filter does not narrow by session: %s", built)
	}
	if !strings.Contains(built, recall.KindMessage) {
		t.Errorf("filter does not narrow by kind: %s", built)
	}

	two := whereFilter(recall.Query{Text: "x", Kinds: []string{recall.KindMessage, recall.KindSummary}})
	if two == nil {
		t.Fatal("two kinds built no filter")
	}
	if b := two.String(); !strings.Contains(b, recall.KindSummary) {
		t.Errorf("the second kind was dropped: %s", b)
	}
}

func TestDecodeHitsReadsAWeaviateResponse(t *testing.T) {
	raw := `{"Get":{"SporeChunk":[
	  {"text":"the retry logic lives in provider","kind":"message","ref_id":"17",
	   "session_id":"sess-1","created_at":"2026-09-03T10:00:00Z",
	   "_additional":{"certainty":0.87}},
	  {"text":"black coffee","kind":"fact","ref_id":"coffee",
	   "session_id":"","created_at":"2026-09-02T09:00:00Z",
	   "_additional":{"certainty":0.61}}]}}`
	var data map[string]models.JSONObject
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	hits, err := decodeHits(&models.GraphQLResponse{Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].ID != "17" || hits[0].Kind != recall.KindMessage || hits[0].SessionID != "sess-1" {
		t.Errorf("first hit = %+v", hits[0].Chunk)
	}
	if hits[0].Score <= hits[1].Score {
		t.Errorf("scores %v and %v are not in the order the server returned", hits[0].Score, hits[1].Score)
	}
	if !strings.Contains(hits[0].Excerpt, "retry logic") {
		t.Errorf("excerpt %q does not carry the text", hits[0].Excerpt)
	}
	if hits[0].CreatedAt.IsZero() {
		t.Error("created_at did not parse")
	}
}

func TestDecodeHitsSurvivesAnErrorResponse(t *testing.T) {
	// GraphQL reports failure inside a 200 body, so a decoder that checks only
	// transport turns a broken query into "no results".
	resp := &models.GraphQLResponse{Errors: []*models.GraphQLError{{Message: "collection not found"}}}
	if _, err := decodeHits(resp); err == nil {
		t.Fatal("a GraphQL error decoded as success")
	} else if !strings.Contains(err.Error(), "collection not found") {
		t.Errorf("error %q drops the server's message", err)
	}
}

func TestDecodeCountReadsAnAggregate(t *testing.T) {
	raw := `{"Aggregate":{"SporeChunk":[{"meta":{"count":7}}]}}`
	var data map[string]models.JSONObject
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	n, err := decodeCount(&models.GraphQLResponse{Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("count = %d, want 7", n)
	}
	empty, err := decodeCount(&models.GraphQLResponse{Data: map[string]models.JSONObject{}})
	if err != nil || empty != 0 {
		t.Errorf("an empty aggregate gave (%d, %v), want (0, nil)", empty, err)
	}
}

func TestExcerptIsBoundedAndWholeWhenShort(t *testing.T) {
	if got := excerpt("short text"); got != "short text" {
		t.Errorf("excerpt(%q) = %q, want it whole", "short text", got)
	}
	long := strings.Repeat("word ", 200)
	got := excerpt(long)
	if len(got) > excerptBytes+len("…") {
		t.Errorf("excerpt is %d bytes, want at most %d", len(got), excerptBytes+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a clipped excerpt should say so: %q", got)
	}
	// A clip that lands mid-rune would corrupt the text on its way into a
	// prompt, so the boundary matters, not just the length.
	multi := strings.Repeat("é", 400)
	if clipped := excerpt(multi); !utf8ValidString(clipped) {
		t.Errorf("excerpt cut a rune in half: %q", clipped)
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
