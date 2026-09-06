package store

import (
	"context"
	"testing"
)

func TestSessionUsageGroupsByModel(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, err := st.CreateSession(ctx, "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		model   string
		in, out int
		cost    float64
	}{
		{"anthropic/claude-opus-5", 100, 20, 0.10},
		{"anthropic/claude-opus-5", 300, 40, 0.30},
		{"ollama/qwen3:8b", 50, 10, 0},
	} {
		if _, err := st.AppendMessage(ctx, Message{
			SessionID: id, Role: "assistant", BlocksJSON: []byte(`[]`),
			Model: m.model, TokensIn: m.in, TokensOut: m.out, CostUSD: m.cost,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.SessionUsage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want one row per model, got %+v", rows)
	}
	var opus UsageRow
	for _, r := range rows {
		if r.Model == "anthropic/claude-opus-5" {
			opus = r
		}
	}
	if opus.Turns != 2 || opus.TokensIn != 400 || opus.TokensOut != 60 {
		t.Fatalf("wrong sums: %+v", opus)
	}
	if opus.CostUSD < 0.399 || opus.CostUSD > 0.401 {
		t.Fatalf("wrong cost: %+v", opus)
	}
}

func TestTotalUsageSpansSessions(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		id, err := st.CreateSession(ctx, "t", "/ws")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendMessage(ctx, Message{
			SessionID: id, Role: "assistant", BlocksJSON: []byte(`[]`),
			Model: "m", TokensIn: 10, TokensOut: 5, CostUSD: 0.01,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.TotalUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Turns != 2 || rows[0].TokensIn != 20 {
		t.Fatalf("totals must span sessions: %+v", rows)
	}
}

func TestUsageOfAnEmptySessionIsEmpty(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, err := st.CreateSession(ctx, "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.SessionUsage(ctx, id)
	if err != nil || len(rows) != 0 {
		t.Fatalf("want no rows and no error, got %+v %v", rows, err)
	}
}

func TestLastSeqReportsTheNewestMessage(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateSession(ctx, "t", "/ws")
	seq, err := st.LastSeq(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("an empty session has no last seq: %d", seq)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.AppendMessage(ctx, Message{
			SessionID: id, Role: "user", BlocksJSON: []byte(`[]`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seq, err = st.LastSeq(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 3 {
		t.Fatalf("want seq 3, got %d", seq)
	}
}

func TestSetSummaryWithEmptyTextIndexesNothing(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateSession(ctx, "t", "/ws")
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: id, Role: "user", BlocksJSON: []byte(`[]`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, id, "a real summary", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, id, "", 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM recall_fts WHERE kind = 'summary' AND ref_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("clearing a summary must leave no indexed document, got %d", n)
	}
}
