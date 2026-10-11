package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
)

func TestStreamFailsWhenTheServerGoesSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "wrkspc_test", false, srv.Client())
	c.stall = 100 * time.Millisecond
	ch, err := c.Stream(context.Background(), provider.Request{Model: "m", MaxTokens: 16})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var last provider.Event
	for ev := range ch {
		last = ev
	}
	if last.Type != provider.EventError || !strings.Contains(last.Err.Error(), "stalled") {
		t.Fatalf("last event = %+v, want a stall error", last)
	}
}
