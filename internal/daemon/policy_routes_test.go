package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

// send makes one request with an optional JSON body and returns the status
// and the body text.
func send(t *testing.T, method, url string, body any, header ...string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rdr)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// captureLog routes slog's default logger into a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func attachPolicy(t *testing.T, s *Server) *policy.Reloader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[policy]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pc := s.cfg.Policy
	// config.Default has no baseline; config.Load adds it. Add it here so the
	// view's baseline rows are exercised.
	pc.Deny = append(config.BaselineDeny(), pc.Deny...)
	e, err := policy.NewEngine(pc)
	if err != nil {
		t.Fatal(err)
	}
	g := policy.NewGuard(nil, e, s.Approver(), s.Store(), nil)
	s.guard = g
	rl := policy.NewReloader(path, pc, g)
	s.AttachOperator(Operator{Policy: rl})
	return rl
}

func probeDecision(s *Server) policy.Decision {
	return s.guard.Engine().Evaluate(policy.Session{Profile: policy.ProfileLocal}, policy.Call{Tool: "probe_tool", Args: []byte(`{}`)}).Decision
}

func TestPolicyListsSourcesAndRevokeTakesEffect(t *testing.T) {
	s, ts := newTestServer(t)
	rl := attachPolicy(t, s)
	logs := captureLog(t)
	if err := rl.Learn(policy.DecisionAllow, "probe_tool"); err != nil {
		t.Fatal(err)
	}

	code, body := send(t, "GET", ts.URL+"/api/policy", nil)
	if code != 200 {
		t.Fatalf("GET = %d %s", code, body)
	}
	var got PolicyJSON
	_ = json.Unmarshal([]byte(body), &got)
	var learned, baseline bool
	for _, r := range got.Rules {
		learned = learned || (r.Profile == "local" && r.Rule == "probe_tool" && r.Source == "learned" && r.Decision == "allow")
		baseline = baseline || r.Source == "baseline"
	}
	if !learned || !baseline {
		t.Fatalf("rules missing learned=%v baseline=%v: %+v", learned, baseline, got.Rules)
	}

	code, body = send(t, "DELETE", ts.URL+"/api/policy/learned", map[string]string{"decision": "allow", "rule": "probe_tool"}, "X-Spore-Client", "tui")
	if code != 200 {
		t.Fatalf("DELETE = %d %s", code, body)
	}
	if probeDecision(s) != policy.DecisionAsk {
		t.Error("the next call must ask again after a revoke")
	}
	if l := logs.String(); !strings.Contains(l, "operator action") || !strings.Contains(l, "actor=tui") || !strings.Contains(l, "action=revoke") {
		t.Errorf("audit line missing:\n%s", l)
	}

	code, body = send(t, "DELETE", ts.URL+"/api/policy/learned", map[string]string{"decision": "allow", "rule": "probe_tool"})
	if code != 404 || !strings.Contains(body, "only rules added with p can be revoked here") {
		t.Errorf("second revoke = %d %s", code, body)
	}
	code, _ = send(t, "DELETE", ts.URL+"/api/policy/learned", map[string]string{"decision": "maybe", "rule": "x"})
	if code != 400 {
		t.Errorf("bad decision = %d, want 400", code)
	}
	code, _ = send(t, "DELETE", ts.URL+"/api/policy/learned", nil)
	if code != 400 {
		t.Errorf("no body = %d, want 400", code)
	}
}

func TestRevokeWithoutAReloaderIs503(t *testing.T) {
	_, ts := newTestServer(t)
	code, _ := send(t, "DELETE", ts.URL+"/api/policy/learned", map[string]string{"decision": "allow", "rule": "x"})
	if code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", code)
	}
	code, _ = send(t, "GET", ts.URL+"/api/policy", nil)
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET without a guard = %d, want 503", code)
	}
}

func TestRevokeRefusesBaselineAndConfigRules(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)

	// Get the policy list to find baseline and config rules
	code, body := send(t, "GET", ts.URL+"/api/policy", nil)
	if code != 200 {
		t.Fatalf("GET = %d %s", code, body)
	}
	var got PolicyJSON
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}

	var rulesToTest []*PolicyRuleJSON
	for i := range got.Rules {
		if got.Rules[i].Source == "baseline" || got.Rules[i].Source == "config" {
			rulesToTest = append(rulesToTest, &got.Rules[i])
			if len(rulesToTest) >= 2 {
				break
			}
		}
	}
	if len(rulesToTest) == 0 {
		t.Fatal("no baseline or config rules found in policy")
	}

	// Try to revoke each rule - should fail
	for _, rule := range rulesToTest {
		code, body = send(t, "DELETE", ts.URL+"/api/policy/learned",
			map[string]string{"decision": rule.Decision, "rule": rule.Rule})
		if code != 404 || !strings.Contains(body, "only rules added with p can be revoked here") {
			t.Errorf("revoke %s rule %s = %d %s, want 404 with message", rule.Source, rule.Rule, code, body)
		}

		// Verify the rule is still there
		code, body = send(t, "GET", ts.URL+"/api/policy", nil)
		if code != 200 {
			t.Fatalf("GET after revoke = %d %s", code, body)
		}
		var updatedPolicy PolicyJSON
		if err := json.Unmarshal([]byte(body), &updatedPolicy); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range updatedPolicy.Rules {
			if r.Rule == rule.Rule && r.Source == rule.Source {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s rule %s disappeared after failed revoke", rule.Source, rule.Rule)
		}
	}

	// Test oversized body
	oversized := map[string]string{"decision": "deny", "rule": strings.Repeat("a", 70*1024)}
	code, _ = send(t, "DELETE", ts.URL+"/api/policy/learned", oversized)
	if code != 400 {
		t.Errorf("oversized body = %d, want 400", code)
	}
}
