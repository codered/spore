package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func TestStreamReportsTheOutputLimit(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   bool
	}{{"length", true}, {"stop", false}, {"tool_calls", false}} {
		body := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"" + tc.reason + "\"}]}\n\ndata: [DONE]\n\n"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(body))
		}))
		ch, err := New(srv.URL, "sk-test", srv.Client()).Stream(context.Background(), provider.Request{Model: "m"})
		if err != nil {
			srv.Close()
			t.Fatalf("Stream: %v", err)
		}
		var done *provider.Event
		for ev := range ch {
			if ev.Type == provider.EventDone {
				ev := ev
				done = &ev
			}
		}
		srv.Close()
		if done == nil || done.HitMaxTokens != tc.want {
			t.Errorf("finish_reason %q: done = %+v, want HitMaxTokens %v", tc.reason, done, tc.want)
		}
	}
}
