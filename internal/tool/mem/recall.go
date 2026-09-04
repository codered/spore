package mem

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/trace"
)

type recallSearch struct{ r recall.Recall }

// NewRecallSearch builds the read-only search tool over whichever backend is
// configured.
func NewRecallSearch(r recall.Recall) tool.Tool { return recallSearch{r: r} }

func (recallSearch) Name() string { return "recall_search" }

func (recallSearch) Description() string {
	return "Search earlier conversations, compaction summaries and memory facts by keyword. " +
		"Use it to recover something discussed in another session, or to read a memory fact " +
		"whose body did not fit in context."
}

func (recallSearch) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "query": {"type": "string", "description": "Keywords to search for."},
	    "k": {"type": "integer", "description": "Maximum hits to return (default 8, maximum 25)."}
	  },
	  "required": ["query"]
	}`)
}

// ReadOnly is true: search mutates nothing, so the loop may dispatch it
// concurrently with other read-only calls.
func (recallSearch) ReadOnly() bool { return true }

func (t recallSearch) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}

	q := recall.Query{Text: a.Query, K: recall.ClampK(a.K)}

	// The policy engine gates tool names and their arguments, not the scope of
	// a result, so this restriction has to live in the tool. A remote session
	// is an admitted chat user, not the operator: it may search its own
	// conversation and nothing else, and it may not read facts at all.
	sess := policy.SessionFrom(ctx)
	sessionID, profile := sess.ID, sess.Profile
	// This is deliberately a negative match on the one trusted profile, not a
	// positive match on the untrusted one. policy.Engine accepts arbitrary
	// profile names from config, so a future profile this tool has never
	// heard of (e.g. "guest") must land on the scoped branch by default.
	// Matching on == ProfileRemote would instead send it down the unscoped,
	// facts-included path -- fail-open exactly where every other trust
	// decision in this codebase fails closed.
	if profile != policy.ProfileLocal {
		// An empty session id is not "no filter" — the sqlitefts backend only
		// applies its session_id predicate when SessionID is non-empty, so
		// setting it to "" here would turn a remote call with no session
		// attached into an UNSCOPED search across every session's messages
		// and summaries: exactly the leak this tool exists to prevent. No
		// caller does this today (the daemon always attaches a session
		// before running a turn), but that is an operational fact, not a
		// guarantee this function can lean on. Refuse instead of searching:
		// return the same "no matches" a caller sees for a real empty
		// result, not an error the model might read as worth retrying.
		if sessionID == "" {
			return "no matches", nil
		}
		q.SessionID = sessionID
		q.Kinds = []string{recall.KindMessage, recall.KindSummary}
	}

	status, _ := t.r.Status(ctx)
	backend := status.Backend
	if backend == "" {
		backend = "unknown"
	}
	ctx, span := trace.StartRetriever(ctx, backend, a.Query, q.K)
	// EndRetriever closes the span on the success path below and records the
	// hit list on it first; this defer only covers the paths that skip that
	// call (the error return, or a panic unwinding through Search), so the
	// span is always closed exactly once and never left open for a crashed
	// call.
	ended := false
	defer func() {
		if !ended {
			span.End()
		}
	}()

	hits, err := t.r.Search(ctx, q)
	if err != nil {
		return "", fmt.Errorf("recall search failed: %w", err)
	}
	ids := make([]string, 0, len(hits))
	scores := make([]float64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.Kind+":"+h.ID)
		scores = append(scores, h.Score)
	}
	trace.EndRetriever(span, ids, scores)
	ended = true

	if len(hits) == 0 {
		return "no matches", nil
	}
	var b strings.Builder
	for i, h := range hits {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%s", h.Kind)
		if h.SessionID != "" {
			fmt.Fprintf(&b, " · session %s", h.SessionID)
		}
		if !h.CreatedAt.IsZero() {
			fmt.Fprintf(&b, " · %s", h.CreatedAt.Format("2006-01-02"))
		}
		if h.Kind == recall.KindFact {
			fmt.Fprintf(&b, " · %s", h.ID)
		}
		b.WriteString("]\n")
		// A fact is short and is the retrieval path for one that did not fit
		// the context budget, so it comes back whole. Everything else is a
		// snippet: the point is to locate the conversation, not replay it.
		if h.Kind == recall.KindFact {
			b.WriteString(h.Text)
		} else {
			b.WriteString(h.Excerpt)
		}
	}
	return b.String(), nil
}
