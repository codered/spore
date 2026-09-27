package refine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/codered/spore/internal/policy"
)

type fakeRequester struct{ sid, instr string }

func (f *fakeRequester) Request(_ context.Context, sid, instr string) error {
	f.sid, f.instr = sid, instr
	return nil
}

func TestRefineToolSchedulesForTheCallingSession(t *testing.T) {
	fr := &fakeRequester{}
	tl := New(fr)
	ctx := policy.WithSession(context.Background(), policy.Session{ID: "s1", Workspace: "/w"})
	out, err := tl.Call(ctx, json.RawMessage(`{"instructions":"the lint rule"}`))
	if err != nil {
		t.Fatal(err)
	}
	if fr.sid != "s1" || fr.instr != "the lint rule" || out == "" {
		t.Fatalf("requester got %+v, out %q", fr, out)
	}
	if _, err := tl.Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("a call with no session attached must fail")
	}
}
