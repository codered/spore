package daemon

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func getUsage(t *testing.T, url string) UsageJSON {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, res.Status)
	}
	var out UsageJSON
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUsageReportsTheSessionAndTheDays(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{
		Text: "hi", Usage: provider.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 30},
	})
	id := createTestSession(t, ts.URL)
	body := attachStream(t, ts, id)
	res := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "go"})
	res.Body.Close()
	readSSE(t, body, 3) // turn_started, text, turn_done

	u := getUsage(t, ts.URL+"/api/usage?session="+id)
	if len(u.Session) != 1 || u.Session[0].Turns != 1 || u.Session[0].TokensIn != 10 || u.Session[0].TokensCacheRead != 30 {
		t.Fatalf("session = %+v, want one model row with 1 turn, 10 in, 30 cache read", u.Session)
	}
	if len(u.Days) != 1 || u.Days[0].Turns != 1 || u.Days[0].Day == "" {
		t.Fatalf("days = %+v, want one row for today", u.Days)
	}
}

func TestUsageWithoutASessionStillReportsTheDays(t *testing.T) {
	_, ts := newTestServer(t)
	u := getUsage(t, ts.URL+"/api/usage")
	if u.Session == nil || u.Days == nil {
		t.Fatalf("usage = %+v, want empty arrays, not null", u)
	}
}

func TestUsageForAnUnknownSessionIsNotFound(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/usage?session=nope")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}
