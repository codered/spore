package weaviate

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	client "github.com/weaviate/weaviate-go-client/v5/weaviate"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/graphql"

	"github.com/codered/spore/internal/recall"
)

// Name is the backend's name in config, in `recall status`, and in the
// watermark row. One constant so the three cannot drift.
const Name = "weaviate"

// batchLimit bounds one insert request. Weaviate accepts far larger batches;
// the limit is here so a backfill of a long history streams rather than
// building one request the size of the archive.
const batchLimit = 100

// statusTimeout bounds the health check. `recall status` must answer promptly
// even when the container is wedged rather than merely absent.
const statusTimeout = 5 * time.Second

// Backend implements recall.Recall against a Weaviate instance. It holds no
// state beyond the client: the watermark lives in SQLite, so restarting spore
// resumes rather than re-sending.
type Backend struct {
	c      *client.Client
	host   string
	scheme string
}

// New dials nothing. Construction must not fail because a container is down,
// or spore could not start on a machine whose sidecar has not come up yet.
func New(baseURL string) (*Backend, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("%s: no url configured", Name)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%s: parse url: %w", Name, err)
	}
	if u.Host == "" || u.Scheme == "" {
		return nil, fmt.Errorf("%s: url %q needs a scheme and a host", Name, baseURL)
	}
	// client.New rather than client.NewClient: NewClient probes the server for
	// its version during construction, which blocks for as long as the dial
	// takes. spore builds this backend while wiring the daemon, so a sidecar
	// that is down must cost nothing at startup -- it is the degraded case,
	// not a failure to start.
	c := client.New(client.Config{Host: u.Host, Scheme: u.Scheme})
	return &Backend{c: c, host: u.Host, scheme: u.Scheme}, nil
}

// Ready reports whether the instance is up and serving. Setup waits on it;
// Status asks it to decide whether search has degraded.
func (b *Backend) Ready(ctx context.Context) error {
	ok, err := b.c.Misc().ReadyChecker().Do(ctx)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", Name, b.host, err)
	}
	if !ok {
		return fmt.Errorf("%s at %s: not ready", Name, b.host)
	}
	return nil
}

// EnsureCollection creates the class when it is missing and leaves an
// existing one alone. It is safe to call on every start, which is what makes
// a half-finished setup recoverable by running setup again.
func (b *Backend) EnsureCollection(ctx context.Context) error {
	exists, err := b.c.Schema().ClassExistenceChecker().WithClassName(Collection).Do(ctx)
	if err != nil {
		return fmt.Errorf("%s: check collection: %w", Name, err)
	}
	if exists {
		return nil
	}
	if err := b.c.Schema().ClassCreator().WithClass(collectionClass()).Do(ctx); err != nil {
		return fmt.Errorf("%s: create collection: %w", Name, err)
	}
	return nil
}

// DropAll removes the collection. `recall reindex` uses it: a rebuild
// renumbers every FTS rowid, so the mirror's watermark is meaningless
// afterwards and the only honest move is to start the mirror over.
func (b *Backend) DropAll(ctx context.Context) error {
	exists, err := b.c.Schema().ClassExistenceChecker().WithClassName(Collection).Do(ctx)
	if err != nil {
		return fmt.Errorf("%s: check collection: %w", Name, err)
	}
	if !exists {
		return nil
	}
	if err := b.c.Schema().ClassDeleter().WithClassName(Collection).Do(ctx); err != nil {
		return fmt.Errorf("%s: delete collection: %w", Name, err)
	}
	return nil
}

func (b *Backend) Index(ctx context.Context, chunks []recall.Chunk) error {
	for start := 0; start < len(chunks); start += batchLimit {
		end := min(start+batchLimit, len(chunks))
		batcher := b.c.Batch().ObjectsBatcher()
		for _, c := range chunks[start:end] {
			batcher = batcher.WithObject(chunkObject(c))
		}
		res, err := batcher.Do(ctx)
		if err != nil {
			return fmt.Errorf("%s: index batch: %w", Name, err)
		}
		// A batch returns 200 with per-object errors inside it, so a caller
		// that checks only err has silently dropped objects.
		for _, r := range res {
			if r.Result == nil || r.Result.Errors == nil || len(r.Result.Errors.Error) == 0 {
				continue
			}
			if e := r.Result.Errors.Error[0]; e != nil {
				return fmt.Errorf("%s: index object: %s", Name, e.Message)
			}
		}
	}
	return nil
}

func (b *Backend) Search(ctx context.Context, q recall.Query) ([]recall.Hit, error) {
	// Tokenising already treats text with no word characters as "no hits";
	// sending it would be a round trip whose answer is known.
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	fields := []graphql.Field{
		{Name: "text"}, {Name: "kind"}, {Name: "ref_id"},
		{Name: "session_id"}, {Name: "created_at"},
		{Name: "_additional", Fields: []graphql.Field{{Name: "certainty"}}},
	}
	near := (&graphql.NearTextArgumentBuilder{}).WithConcepts([]string{q.Text})
	get := b.c.GraphQL().Get().
		WithClassName(Collection).
		WithFields(fields...).
		WithNearText(near).
		WithLimit(recall.ClampK(q.K))
	if where := whereFilter(q); where != nil {
		get = get.WithWhere(where)
	}
	resp, err := get.Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: search: %w", Name, err)
	}
	hits, err := decodeHits(resp)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Name, err)
	}
	return hits, nil
}

// Status never returns an error for an unreachable server. Being down is the
// condition it exists to report, and returning an error would make every
// caller re-implement the same "is this the degraded case" check.
func (b *Backend) Status(ctx context.Context) (recall.Status, error) {
	st := recall.Status{Backend: Name, Counts: map[string]int{}}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	if err := b.Ready(ctx); err != nil {
		st.Degraded = true
		st.Reason = err.Error()
		return st, nil
	}
	for _, kind := range []string{recall.KindMessage, recall.KindSummary, recall.KindFact} {
		n, err := b.countKind(ctx, kind)
		if err != nil {
			st.Degraded = true
			st.Reason = err.Error()
			return st, nil
		}
		st.Counts[kind] = n
	}
	return st, nil
}

func (b *Backend) countKind(ctx context.Context, kind string) (int, error) {
	resp, err := b.c.GraphQL().Aggregate().
		WithClassName(Collection).
		WithWhere(filterKind(kind)).
		WithFields(graphql.Field{Name: "meta", Fields: []graphql.Field{{Name: "count"}}}).
		Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: count %s: %w", Name, kind, err)
	}
	return decodeCount(resp)
}
