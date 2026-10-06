package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/store"
)

// usageDaemon serves GET /api/usage and fails the test on anything else, so
// a /usage that posted a message to the model would be caught.
func usageDaemon(t *testing.T) *client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/usage" || r.URL.Query().Get("session") != "s1" {
			t.Errorf("request = %s %s, want GET /api/usage?session=s1", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode(daemon.UsageJSON{
			Session: []store.UsageRow{{Model: "m", Turns: 3, TokensIn: 1200, TokensOut: 80, CostUSD: 0.25}},
			Days:    []store.DailyUsageRow{{Day: "2026-10-05", UsageRow: store.UsageRow{Model: "m", Turns: 40, TokensIn: 2_500_000, TokensOut: 9000, CostUSD: 4}}},
		})
	}))
	t.Cleanup(ts.Close)
	return &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}
}

func TestRunPlainSlashReportsUsage(t *testing.T) {
	c := usageDaemon(t)
	var out bytes.Buffer
	handled, err := runPlainSlash(context.Background(), c, "s1", "/usage", true, &out)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	for _, want := range []string{"turns: 3", "tokens in: 1,200", "cost: $0.2500", "last 30 days, all sessions: 40 turns, 2.5M in, 9k out, $4.00"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
}

func TestTUISlashUsageReadsTheUsageEndpoint(t *testing.T) {
	b := tuiBackend{c: usageDaemon(t), showCost: false}
	got, err := b.Slash(context.Background(), "s1", "usage")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "turns: 3") || !strings.Contains(got, "all sessions: 40 turns") || strings.Contains(got, "$") {
		t.Fatalf("got %q, want session and 30-day totals with no cost", got)
	}
}
