package openaicompat

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

func streamText(t *testing.T, c *Client) (string, error) {
	t.Helper()
	ch, err := c.Stream(context.Background(), provider.Request{
		Model:    "test-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text.WriteString(ev.Text)
		case provider.EventError:
			return text.String(), ev.Err
		}
	}
	return text.String(), nil
}

// A reply that keeps streaming is not cut off however long it runs. A
// whole-request timeout once killed a healthy local stream at ten minutes.
func TestStreamOutlivesTheStallLimitWhileItSends(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 10; i++ { // 10 x 30ms = 300ms against a 100ms limit
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%d\"}}]}\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL, "", srv.Client())
	c.stall = 100 * time.Millisecond
	text, err := streamText(t, c)
	if err != nil {
		t.Fatalf("stream failed after %q: %v", text, err)
	}
	if text != "0123456789" {
		t.Errorf("text = %q, want 0123456789", text)
	}
}

func TestStreamFailsWhenTheServerGoesSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL, "", srv.Client())
	c.stall = 100 * time.Millisecond
	text, err := streamText(t, c)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("err = %v after %q, want a stall", err, text)
	}
}
