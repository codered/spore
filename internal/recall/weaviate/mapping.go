package weaviate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/google/uuid"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/filters"
	"github.com/weaviate/weaviate/entities/models"

	"github.com/codered/spore/internal/recall"
)

// excerptBytes bounds what a hit carries back into a prompt. Recall exists to
// point at content, not to inline it; the caller reads the source when it
// needs more.
const excerptBytes = 280

// idNamespace makes object ids deterministic. A re-sent batch must overwrite
// rather than duplicate, because the mirror re-sends whenever it crashed
// between accepting a batch and writing its cursor.
var idNamespace = uuid.MustParse("6f1d0f3c-9a1e-5c7b-8a2f-1f4b6d0c7e21")

func objectID(kind, refID string) strfmt.UUID {
	return strfmt.UUID(uuid.NewSHA1(idNamespace, []byte(kind+"\x00"+refID)).String())
}

func chunkObject(c recall.Chunk) *models.Object {
	return &models.Object{
		Class: Collection,
		ID:    objectID(c.Kind, c.ID),
		Properties: map[string]any{
			"text":       c.Text,
			"kind":       c.Kind,
			"ref_id":     c.ID,
			"session_id": c.SessionID,
			"created_at": c.CreatedAt.Format(time.RFC3339),
		},
	}
}

// whereFilter translates a Query's scope. It returns nil for an unscoped
// query so the caller can leave the filter off the request entirely rather
// than sending a filter that matches everything.
func whereFilter(q recall.Query) *filters.WhereBuilder {
	var operands []*filters.WhereBuilder
	if q.SessionID != "" {
		operands = append(operands, filters.Where().
			WithPath([]string{"session_id"}).WithOperator(filters.Equal).WithValueText(q.SessionID))
	}
	if len(q.Kinds) > 0 {
		var kinds []*filters.WhereBuilder
		for _, k := range q.Kinds {
			kinds = append(kinds, filters.Where().
				WithPath([]string{"kind"}).WithOperator(filters.Equal).WithValueText(k))
		}
		if len(kinds) == 1 {
			operands = append(operands, kinds[0])
		} else {
			operands = append(operands, filters.Where().WithOperator(filters.Or).WithOperands(kinds))
		}
	}
	switch len(operands) {
	case 0:
		return nil
	case 1:
		return operands[0]
	default:
		return filters.Where().WithOperator(filters.And).WithOperands(operands)
	}
}

// filterKind is the one-property filter Aggregate uses. It is separate from
// whereFilter because a count is never scoped by session.
func filterKind(kind string) *filters.WhereBuilder {
	return filters.Where().WithPath([]string{"kind"}).WithOperator(filters.Equal).WithValueText(kind)
}

// rawHit is the projection every search asks for. certainty is Weaviate's
// normalised similarity, which is the only score spore promises anything
// about -- the Recall contract makes the value itself backend-defined.
type rawHit struct {
	Text       string `json:"text"`
	Kind       string `json:"kind"`
	RefID      string `json:"ref_id"`
	SessionID  string `json:"session_id"`
	CreatedAt  string `json:"created_at"`
	Additional struct {
		Certainty float64 `json:"certainty"`
	} `json:"_additional"`
}

// decodeHits reads the GraphQL envelope. A GraphQL response carries errors in
// the body with a 200 status, so a decoder that only checks transport has
// silently returned "no results" for a broken query.
func decodeHits(resp *models.GraphQLResponse) ([]recall.Hit, error) {
	if resp == nil {
		return nil, fmt.Errorf("empty response")
	}
	if err := graphQLErrors(resp); err != nil {
		return nil, err
	}
	get, ok := resp.Data["Get"]
	if !ok {
		return nil, nil
	}
	blob, err := json.Marshal(get)
	if err != nil {
		return nil, err
	}
	var wrapper map[string][]rawHit
	if err := json.Unmarshal(blob, &wrapper); err != nil {
		return nil, fmt.Errorf("decode hits: %w", err)
	}
	var out []recall.Hit
	for _, r := range wrapper[Collection] {
		when, _ := time.Parse(time.RFC3339, r.CreatedAt)
		out = append(out, recall.Hit{
			Chunk: recall.Chunk{
				ID: r.RefID, Kind: r.Kind, Text: r.Text,
				SessionID: r.SessionID, CreatedAt: when,
			},
			Score:   r.Additional.Certainty,
			Excerpt: excerpt(r.Text),
		})
	}
	return out, nil
}

// decodeCount reads meta.count out of an Aggregate response. A missing count
// is zero rather than an error: an empty collection legitimately returns no
// aggregate group.
func decodeCount(resp *models.GraphQLResponse) (int, error) {
	if resp == nil {
		return 0, fmt.Errorf("empty response")
	}
	if err := graphQLErrors(resp); err != nil {
		return 0, err
	}
	agg, ok := resp.Data["Aggregate"]
	if !ok {
		return 0, nil
	}
	blob, err := json.Marshal(agg)
	if err != nil {
		return 0, err
	}
	var wrapper map[string][]struct {
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(blob, &wrapper); err != nil {
		return 0, fmt.Errorf("decode count: %w", err)
	}
	groups := wrapper[Collection]
	if len(groups) == 0 {
		return 0, nil
	}
	return groups[0].Meta.Count, nil
}

// graphQLErrors turns the errors carried in a 200 response body into a real
// error, so no caller has to remember that GraphQL fails with a success code.
func graphQLErrors(resp *models.GraphQLResponse) error {
	if len(resp.Errors) == 0 {
		return nil
	}
	var msg string
	for _, e := range resp.Errors {
		if e == nil || e.Message == "" {
			continue
		}
		if msg != "" {
			msg += "; "
		}
		msg += e.Message
	}
	if msg == "" {
		msg = "unspecified error"
	}
	return fmt.Errorf("weaviate: %s", msg)
}

// excerpt clips on a rune boundary so a multi-byte character is never cut in
// half on its way into a prompt.
func excerpt(text string) string {
	if len(text) <= excerptBytes {
		return text
	}
	out := make([]rune, 0, len(text))
	n := 0
	for _, r := range text {
		if n+len(string(r)) > excerptBytes {
			break
		}
		out = append(out, r)
		n += len(string(r))
	}
	return string(out) + "…"
}
