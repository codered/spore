package anthropic

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
	}{{"max_tokens", true}, {"end_turn", false}, {"tool_use", false}} {
		body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + tc.reason + "\"},\"usage\":{\"output_tokens\":7}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(body))
		}))
		ch, err := New(srv.URL, "sk-test", "", true, srv.Client()).Stream(context.Background(), provider.Request{Model: "m", MaxTokens: 10})
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
			t.Errorf("stop_reason %q: done = %+v, want HitMaxTokens %v", tc.reason, done, tc.want)
		}
	}
}
