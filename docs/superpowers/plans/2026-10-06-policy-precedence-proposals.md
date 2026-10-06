# Policy precedence, pattern proposals and daemon route auth — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a learned path rule outrank a bare tool ask, turn the `p` answer into a reviewed proposal, and put every daemon `/api` route behind a token.

**Architecture:** The policy engine ranks matching allow/ask rules by tier (argument predicate > exact tool name > tool glob) with ask winning ties, so order stops mattering. The guard writes a `policy.allow`/`policy.deny` refinement row instead of editing `config.toml`; `Refiner.Accept` applies it through the existing `Reloader.Learn`. The daemon loads a persistent token from `<data dir>/daemon.token` and refuses `/api/*` without it (bearer header or cookie), plus a Host check.

**Tech Stack:** Go 1.x stdlib (`net/http`, `crypto/rand`, `crypto/subtle`), SQLite via the existing `internal/store`, vanilla JS in `web/app.js`, Bubble Tea TUI.

**Spec:** `docs/superpowers/specs/2026-10-06-policy-precedence-proposals-design.md`

## Global Constraints

- Deny (config, profile, learned, baseline) is evaluated first and always wins. Never change that.
- Learned allow/ask rules are built into the base ruleset only, never into a profile's ruleset.
- Tier 1 = rule has an argument predicate; tier 2 = no predicate, tool glob has no `*`; tier 3 = no predicate, tool glob has `*`. Within the deciding tier, ask beats allow.
- Refinement kinds: `policy.allow`, `policy.deny`. Trigger: `approval`. Round id: `approval-<pendingID>`. `target` and `after` are both the rule text.
- Token file: `<cfg.DataDir>/daemon.token`, 64 hex chars (32 random bytes), mode 0600, persists across restarts.
- Cookie: name `spore_token`, `HttpOnly`, `SameSite=Strict`, `Path=/`.
- Open routes: `GET /healthz`, `GET /`, `GET /static/{file}`. Everything under `/api/` requires the token. An empty server token refuses every `/api` request (fail closed).
- Host check: hostname must be `localhost`, `127.0.0.1`, `::1`, or the configured daemon address's host; port not compared; else 403.
- Comment style: match surrounding code — full-sentence `//` comments that explain *why*.
- The CI gate is: `go vet ./...`, `make fmtcheck`, `make lint`, `make vulncheck`, `make tidy` (or `go mod tidy` + clean diff), `go test ./...`. Run all of them before claiming a task done; `make lint` does not check gofmt.
- Do not kill or restart any `spore` daemon already running on this machine; use temp data dirs and ports in tests and manual checks.

## Review Focus

1. **A `p` answer from a `remote` (Discord) session** — must not be offered and must not create a proposal row that would loosen local policy. Test in Task 3.
2. **A refinement planner reply containing `"kind":"policy.allow"`** — the model's refinement round must not be able to mint a policy proposal; `apply.go` must reject the kind. Test in Task 2.
3. **Two clicks on Accept for the same policy row** — the rule must be written once and the second accept must fail cleanly. Test in Task 2.
4. **A browser tab opened before the upgrade (no cookie)** — `/` must show the "run `spore web`" page rather than a broken UI, and `api()` must show the same instruction on 401. Test in Task 5 (server side) and Task 6 (JS string check).
5. **A route added later without the token wrapper** — a source-level test must fail when any `/api/` pattern in `server.go` is registered outside `api(...)`. Test in Task 5.

---

### Task 1: Tiered precedence in the policy engine

**Files:**
- Modify: `internal/policy/rule.go` (Rule struct ~line 70-85, `ParseRule` ~line 98-128)
- Modify: `internal/policy/engine.go` (`buildRuleset` ~line 102-150, `Evaluate` ~line 236-278)
- Test: `internal/policy/engine_test.go`

**Interfaces:**
- Produces: `func (r Rule) Tier() int` (1, 2 or 3; 0 is never returned for a parsed rule); `func (e *Engine) WouldAllow(s Session, c Call, rule string) bool`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/policy/engine_test.go`:

```go
func evalDecision(t *testing.T, e *Engine, p Profile, tool, args string) Decision {
	t.Helper()
	return e.Evaluate(Session{Profile: p}, Call{Tool: tool, Args: json.RawMessage(args)}).Decision
}

// The bug this design fixes: a learned rule with a path condition never
// applied because a bare ask for the same tool was listed before it.
func TestALearnedPathAllowOutranksABareAsk(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Ask:     []string{"fs_write"},
		Learned: config.LearnedPolicy{Allow: []string{"fs_write(path matches /ws/uuid-server/**)"}},
	})
	if got := evalDecision(t, e, ProfileLocal, "fs_write", `{"path":"/ws/uuid-server/a.go"}`); got != DecisionAllow {
		t.Errorf("inside the pattern = %s, want allow", got)
	}
	if got := evalDecision(t, e, ProfileLocal, "fs_write", `{"path":"/ws/other/a.go"}`); got != DecisionAsk {
		t.Errorf("outside the pattern = %s, want ask", got)
	}
}

// A narrow ask is never overridden by a broader allow in the same tier,
// however the globs are spelled. A depth ranking would get this wrong.
func TestASameTierAskBeatsAnAllow(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Ask:     []string{"fs_read(path matches **/secrets/**)"},
		Learned: config.LearnedPolicy{Allow: []string{"fs_read(path matches /ws/repo/**)"}},
	})
	if got := evalDecision(t, e, ProfileLocal, "fs_read", `{"path":"/ws/repo/secrets/key"}`); got != DecisionAsk {
		t.Errorf("secrets = %s, want ask", got)
	}
	if got := evalDecision(t, e, ProfileLocal, "fs_read", `{"path":"/ws/repo/main.go"}`); got != DecisionAllow {
		t.Errorf("repo file = %s, want allow", got)
	}
}

func TestAnExactToolNameOutranksAToolGlob(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Allow: []string{"mcp__time__now"},
		Ask:   []string{"mcp__*"},
	})
	if got := evalDecision(t, e, ProfileLocal, "mcp__time__now", `{}`); got != DecisionAllow {
		t.Errorf("mcp__time__now = %s, want allow", got)
	}
	if got := evalDecision(t, e, ProfileLocal, "mcp__other__x", `{}`); got != DecisionAsk {
		t.Errorf("mcp__other__x = %s, want ask", got)
	}
}

func TestATierOneAskInsideABareAllowStillAsks(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Allow: []string{"fs_read"},
		Ask:   []string{"fs_read(path matches **/secrets/**)"},
	})
	if got := evalDecision(t, e, ProfileLocal, "fs_read", `{"path":"/ws/secrets/k"}`); got != DecisionAsk {
		t.Errorf("got %s, want ask", got)
	}
}

func TestRuleTiers(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{"fs_write(path matches /a/**)", 1},
		{"shell_exec(matches sudo)", 1},
		{"fs_write", 2},
		{"mcp__time__now", 2},
		{"mcp__*", 3},
		{"web_*", 3},
	} {
		r, err := ParseRule(DecisionAllow, tc.src)
		if err != nil {
			t.Fatal(err)
		}
		if r.Tier() != tc.want {
			t.Errorf("%s: tier %d, want %d", tc.src, r.Tier(), tc.want)
		}
	}
}

// The whole design rests on this: where a rule sits in the file does not
// change any decision.
func TestDecisionsDoNotDependOnRuleOrder(t *testing.T) {
	allow := []string{"fs_read", "web_*", "fs_write(path matches /ws/build/**)", "mcp__time__now"}
	ask := []string{"fs_write", "mcp__*", "fs_read(path matches **/secrets/**)", "shell_exec"}
	learned := []string{"fs_write(path matches /ws/uuid-server/**)", "shell_exec(word matches ls)"}
	calls := []Call{
		{Tool: "fs_read", Args: json.RawMessage(`{"path":"/ws/a"}`)},
		{Tool: "fs_read", Args: json.RawMessage(`{"path":"/ws/secrets/a"}`)},
		{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/build/a"}`)},
		{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/uuid-server/a"}`)},
		{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/x"}`)},
		{Tool: "mcp__time__now", Args: json.RawMessage(`{}`)},
		{Tool: "mcp__gh__x", Args: json.RawMessage(`{}`)},
		{Tool: "web_fetch", Args: json.RawMessage(`{}`)},
		{Tool: "shell_exec", Args: json.RawMessage(`{"command":"ls"}`)},
		{Tool: "shell_exec", Args: json.RawMessage(`{"command":"rm x"}`)},
		{Tool: "agent_note", Args: json.RawMessage(`{}`)},
	}
	decide := func(al, as, le []string) []Decision {
		e := engine(t, config.PolicyConfig{Allow: al, Ask: as, Learned: config.LearnedPolicy{Allow: le}})
		var out []Decision
		for _, c := range calls {
			out = append(out, e.Evaluate(Session{Profile: ProfileLocal}, c).Decision)
		}
		return out
	}
	want := decide(allow, ask, learned)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200; i++ {
		al, as, le := slices.Clone(allow), slices.Clone(ask), slices.Clone(learned)
		rng.Shuffle(len(al), func(a, b int) { al[a], al[b] = al[b], al[a] })
		rng.Shuffle(len(as), func(a, b int) { as[a], as[b] = as[b], as[a] })
		rng.Shuffle(len(le), func(a, b int) { le[a], le[b] = le[b], le[a] })
		if got := decide(al, as, le); !slices.Equal(got, want) {
			t.Fatalf("permutation %d changed decisions:\n got %v\nwant %v\nallow=%v ask=%v learned=%v", i, got, want, al, as, le)
		}
	}
}

func TestWouldAllow(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Ask: []string{"fs_write", "fs_edit(path matches **/secrets/**)"},
		Profiles: map[string]config.ProfilePolicy{
			"remote": {Deny: []string{"memory"}},
		},
	})
	local := Session{Profile: ProfileLocal, Workspace: "/ws"}
	w := Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/uuid-server/a.go"}`)}
	if !e.WouldAllow(local, w, "fs_write(path matches /ws/uuid-server/**)") {
		t.Error("the original bug's case: want true")
	}
	s := Call{Tool: "fs_edit", Args: json.RawMessage(`{"path":"/ws/secrets/a"}`)}
	if e.WouldAllow(local, s, "fs_edit(path matches /ws/secrets/**)") {
		t.Error("under a narrow written ask: want false")
	}
	if e.WouldAllow(Session{Profile: ProfileRemote, Workspace: "/ws"}, w, "fs_write(path matches /ws/uuid-server/**)") {
		t.Error("remote never sees learned rules: want false")
	}
	if e.WouldAllow(local, w, "fs_write(") {
		t.Error("an unparseable rule: want false")
	}
	// WouldAllow must not change the engine.
	if got := e.Evaluate(local, w).Decision; got != DecisionAsk {
		t.Errorf("after WouldAllow, Evaluate = %s, want ask", got)
	}
}
```

Add `"math/rand/v2"` and `"slices"` to the test file's imports.

Also update two existing tests whose names describe the old order rule (their assertions stay valid under tiers):
- Rename `TestFirstMatchWinsBetweenAllowAndAsk` to `TestAnExactAllowBeatsAGlobAsk` and change its error message from `"(the earlier list wins)"` to `"(tier 2 beats tier 3)"`.
- Rename `TestLearnedRulesApplyAfterConfiguredOnes` to `TestABareLearnedAllowTiesABareAskAndAskWins` and replace its comment with: `// Both rules are bare tool names, so they share a tier and ask wins the tie.`

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/policy/ -run 'TestALearnedPathAllowOutranksABareAsk|TestASameTierAskBeatsAnAllow|TestAnExactToolNameOutranksAToolGlob|TestATierOneAskInsideABareAllowStillAsks|TestRuleTiers|TestDecisionsDoNotDependOnRuleOrder|TestWouldAllow'`
Expected: build failure (`r.Tier undefined`, `e.WouldAllow undefined`).

- [ ] **Step 3: Add the tier to Rule**

In `internal/policy/rule.go`, add a field to `Rule` after `pred predicate`:

```go
	// tier ranks how narrow the rule is: 1 has an argument predicate, 2 is
	// a bare exact tool name, 3 is a bare tool glob. Evaluate decides
	// allow and ask by the narrowest tier that matched.
	tier int
```

In `ParseRule`, after `r := Rule{Decision: d, Raw: raw, tool: re}` and the predicate block, set the tier just before `return r, nil`:

```go
	switch {
	case r.pred != nil:
		r.tier = 1
	case strings.Contains(toolSrc, "*"):
		r.tier = 3
	default:
		r.tier = 2
	}
```

Add the accessor below `Match`:

```go
// Tier is how narrow the rule is: 1 (argument predicate), 2 (exact tool
// name) or 3 (tool glob).
func (r Rule) Tier() int { return r.tier }
```

- [ ] **Step 4: Evaluate by tier and add WouldAllow**

In `internal/policy/engine.go`:

1. In `buildRuleset`, replace the comment and the four `append` lines that build `rs.allowAndAsk` with:

```go
	// Source plays no part in a decision: Evaluate ranks by tier, and ask
	// wins within a tier. The list is kept in tier order so the policy view
	// reads in the order decisions are made.
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(allowRules, SourceConfig)...)
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(askRules, SourceConfig)...)
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(learnedAllow, SourceLearned)...)
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(learnedAsk, SourceLearned)...)
	slices.SortStableFunc(rs.allowAndAsk, func(a, b Rule) int { return a.tier - b.tier })
```

2. Split `Evaluate` so the ruleset can be supplied. Replace the body from `rs, ok := e.profiles[s.Profile]` to the end of the function with:

```go
	rs, ok := e.profiles[s.Profile]
	if !ok {
		rs = e.base
	}
	return decide(rs, env, c)
}

// decide runs one call through one ruleset. Deny wins outright. Otherwise
// the narrowest tier with a matching allow or ask rule decides, and within
// it ask beats allow, so neither a rule's position in the file nor whether
// it was learned changes the outcome. List order only picks which rule is
// named in the result.
func decide(rs ruleset, env Env, c Call) Result {
	for _, r := range rs.deny {
		if r.Match(c, env) {
			return Result{Decision: DecisionDeny, Rule: r.Raw, Detail: r.explain(c, env)}
		}
	}
	var firstAllow, firstAsk [4]*Rule
	for i := range rs.allowAndAsk {
		r := &rs.allowAndAsk[i]
		if !r.Match(c, env) {
			continue
		}
		switch r.Decision {
		case DecisionAsk:
			if firstAsk[r.tier] == nil {
				firstAsk[r.tier] = r
			}
		case DecisionAllow:
			if firstAllow[r.tier] == nil {
				firstAllow[r.tier] = r
			}
		}
	}
	for t := 1; t <= 3; t++ {
		if firstAsk[t] != nil {
			return Result{Decision: DecisionAsk, Rule: firstAsk[t].Raw}
		}
		if firstAllow[t] != nil {
			return Result{Decision: DecisionAllow, Rule: firstAllow[t].Raw}
		}
	}
	// go_run has no effect of its own: everything a program does arrives
	// as a separate call through the guard and is judged there. Falling
	// back to the profile default would put an approval on every program
	// under any profile that lists its own allow rules.
	if c.Tool == KernelTool {
		return Result{Decision: DecisionAllow, Rule: "policy.kernel"}
	}
	return Result{Decision: rs.fallback, Rule: "policy.default"}
}

// WouldAllow reports whether a learned allow rule, if it were added, would
// decide this call as allow. The guard offers "propose this pattern" only
// when it would: a proposal that cannot take effect is noise in review.
// Learned allow rules are built into the base ruleset only, so a session
// on a configured profile (remote, by default) always gets false.
func (e *Engine) WouldAllow(s Session, c Call, rule string) bool {
	if _, ok := e.profiles[s.Profile]; ok {
		return false
	}
	r, err := ParseRule(DecisionAllow, rule)
	if err != nil {
		return false
	}
	r.Source = SourceLearned
	env := e.env
	if s.Workspace != "" {
		env.Workspace = s.Workspace
	}
	var argObj map[string]json.RawMessage
	if err := json.Unmarshal(c.Args, &argObj); err != nil || argObj == nil {
		return false
	}
	rs := e.base
	rs.allowAndAsk = append(slices.Clone(e.base.allowAndAsk), r)
	return decide(rs, env, c).Decision == DecisionAllow
}
```

The existing deny loop, allow/ask loop and `go_run` block that were in `Evaluate` are now in `decide`; make sure they are not left duplicated in `Evaluate`. Keep the malformed-arguments gate and the workspace setup at the top of `Evaluate` unchanged. Update `Evaluate`'s doc comment from "then allow and ask rules in configured order" to "then the narrowest matching tier of allow and ask rules, ask winning ties".

3. Update the package doc in `rule.go` line 1-4: replace "by matching ordered rules against the tool name AND its arguments" with "by matching rules against the tool name AND its arguments".

- [ ] **Step 5: Run the policy tests**

Run: `go test ./internal/policy/`
Expected: PASS. If `TestRulesTagSourcesInEvaluationOrder` fails, check that its expected rows are in tier order (they are: every allow/ask rule in it is tier 2).

- [ ] **Step 6: Mutation check**

Temporarily change `if firstAsk[t] != nil` / `if firstAllow[t] != nil` order (check allow first). Run `go test ./internal/policy/ -run 'TestASameTierAskBeatsAnAllow|TestDecisionsDoNotDependOnRuleOrder'`. Expected: FAIL. Revert and re-run: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/policy/rule.go internal/policy/engine.go internal/policy/engine_test.go
git commit -m "policy: decide allow and ask by tier, not by list order"
```

---

### Task 2: Policy proposals in the refinement ledger

**Files:**
- Modify: `internal/store/refine.go` (constants ~line 11-20, `LatestAppliedRound` ~line 132-146; add `ProposedPolicyExists`)
- Modify: `internal/refine/refine.go` (Kind/Trigger constants ~line 22-40, `Refiner` struct ~line 42-72)
- Modify: `internal/refine/review.go` (`Accept`, `Rollback`)
- Test: `internal/store/refine_test.go` (create if absent; else append), `internal/refine/round_test.go` (append)

**Interfaces:**
- Produces:
  - `store.KindPolicyAllow = "policy.allow"`, `store.KindPolicyDeny = "policy.deny"`, `store.RefineTriggerApproval = "approval"`
  - `func (s *Store) ProposedPolicyExists(ctx context.Context, kind, rule string) (bool, error)`
  - `refine.KindPolicyAllow`, `refine.KindPolicyDeny` (= the store constants), `refine.TriggerApproval Trigger = store.RefineTriggerApproval`
  - `Refiner.ApplyPolicy func(decision, rule string) error` (exported field)

- [ ] **Step 1: Write the failing tests**

Append to `internal/refine/round_test.go`:

```go
func addPolicyRow(t *testing.T, f *fix, kind, rule string) int64 {
	t.Helper()
	after := rule
	id, err := f.st.AddRefinement(context.Background(), store.Refinement{
		RoundID: "approval-1", SessionID: f.sid, Trigger: store.RefineTriggerApproval,
		Kind: kind, Target: rule, After: &after, Rationale: "fs_write on /ws/a/x", Status: store.RefineProposed,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAcceptingAPolicyRowAppliesTheRuleOnce(t *testing.T) {
	f := newFix(t, store.SourceChat)
	var applied []string
	f.r.ApplyPolicy = func(d, rule string) error {
		applied = append(applied, d+" "+rule)
		return nil
	}
	id := addPolicyRow(t, f, store.KindPolicyAllow, "fs_write(path matches /ws/a/**)")
	row, err := f.r.Accept(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RefineApplied {
		t.Errorf("status = %s, want applied", row.Status)
	}
	if len(applied) != 1 || applied[0] != "allow fs_write(path matches /ws/a/**)" {
		t.Fatalf("applied = %v", applied)
	}
	if _, err := f.r.Accept(context.Background(), id); err == nil {
		t.Error("a second accept succeeded; want an error")
	}
	if len(applied) != 1 {
		t.Errorf("the rule was applied %d times, want once", len(applied))
	}
}

func TestAcceptingAPolicyDenyPassesDeny(t *testing.T) {
	f := newFix(t, store.SourceChat)
	var got string
	f.r.ApplyPolicy = func(d, rule string) error { got = d; return nil }
	id := addPolicyRow(t, f, store.KindPolicyDeny, "fs_write(path matches /ws/a/**)")
	if _, err := f.r.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got != "deny" {
		t.Errorf("decision = %q, want deny", got)
	}
}

func TestAcceptingAPolicyRowWithoutApplyPolicyFails(t *testing.T) {
	f := newFix(t, store.SourceChat)
	id := addPolicyRow(t, f, store.KindPolicyAllow, "fs_write(path matches /ws/a/**)")
	if _, err := f.r.Accept(context.Background(), id); err == nil {
		t.Fatal("accept with no ApplyPolicy succeeded")
	}
	row, _, _ := f.st.Refinement(context.Background(), id)
	if row.Status != store.RefineFailed {
		t.Errorf("status = %s, want failed", row.Status)
	}
}

func TestAFailedPolicyWriteMarksTheRowFailed(t *testing.T) {
	f := newFix(t, store.SourceChat)
	f.r.ApplyPolicy = func(string, string) error { return errors.New("disk full") }
	id := addPolicyRow(t, f, store.KindPolicyAllow, "fs_write(path matches /ws/a/**)")
	if _, err := f.r.Accept(context.Background(), id); err == nil {
		t.Fatal("want the write error")
	}
	row, _, _ := f.st.Refinement(context.Background(), id)
	if row.Status != store.RefineFailed {
		t.Errorf("status = %s, want failed", row.Status)
	}
}

func TestRollbackAndLatestRoundSkipPolicyRows(t *testing.T) {
	f := newFix(t, store.SourceChat)
	f.r.ApplyPolicy = func(string, string) error { return nil }
	id := addPolicyRow(t, f, store.KindPolicyAllow, "fs_write(path matches /ws/a/**)")
	if _, err := f.r.Accept(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.st.LatestAppliedRound(context.Background(), f.sid); err != nil || ok {
		t.Fatalf("LatestAppliedRound found a round (ok=%v err=%v); policy rows must not count", ok, err)
	}
	res, err := f.r.Rollback(context.Background(), f.sid, "approval-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RolledBack)+len(res.Stale)+len(res.Failed) != 0 {
		t.Errorf("rollback touched a policy row: %+v", res)
	}
	row, _, _ := f.st.Refinement(context.Background(), id)
	if row.Status != store.RefineApplied {
		t.Errorf("status = %s, want applied", row.Status)
	}
}

// A refinement round is driven by the model. It must not be able to mint
// a policy proposal: only the guard writes those.
func TestARoundCannotProposeAPolicyEdit(t *testing.T) {
	f := newFix(t, store.SourceChat,
		`{"edits":[{"kind":"policy.allow","name":"x","body":"fs_write","rationale":"r"}]}`)
	f.say(t, "user", text("hello"))
	_, _ = f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	rows, err := f.st.Refinements(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Kind, "policy.") {
			t.Fatalf("a round wrote a policy row: %+v", r)
		}
	}
}
```

`Refinements(ctx, "", n)` lists every status. `f.r.Round(ctx, sid, TriggerManual, "", 0)` runs one round, as the existing tests in this file do.

Ensure `"errors"` and `"strings"` are imported in `round_test.go`.

Append to `internal/store/refine_test.go` (create the file with `package store` and imports `context`, `path/filepath`, `testing` if it does not exist):

```go
func TestProposedPolicyExists(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sid, err := st.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	rule := "fs_write(path matches /ws/a/**)"
	if ok, err := st.ProposedPolicyExists(ctx, KindPolicyAllow, rule); err != nil || ok {
		t.Fatalf("empty table: ok=%v err=%v", ok, err)
	}
	after := rule
	id, err := st.AddRefinement(ctx, Refinement{RoundID: "approval-1", SessionID: sid, Trigger: RefineTriggerApproval,
		Kind: KindPolicyAllow, Target: rule, After: &after, Rationale: "r", Status: RefineProposed})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ProposedPolicyExists(ctx, KindPolicyAllow, rule); !ok {
		t.Error("want true for a proposed row")
	}
	if ok, _ := st.ProposedPolicyExists(ctx, KindPolicyDeny, rule); ok {
		t.Error("the other kind must not match")
	}
	if _, err := st.SetRefinementStatus(ctx, id, RefineProposed, RefineRejected); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ProposedPolicyExists(ctx, KindPolicyAllow, rule); ok {
		t.Error("a rejected row must not count")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ ./internal/refine/`
Expected: build failure (undefined `KindPolicyAllow`, `ProposedPolicyExists`, `ApplyPolicy`).

- [ ] **Step 3: Store constants and queries**

In `internal/store/refine.go`, below the status constants add:

```go
// Policy proposals share the refinement ledger. The guard writes them when
// a human answers "propose this pattern"; Accept applies them through the
// policy reloader. Target and After both hold the rule text.
const (
	KindPolicyAllow       = "policy.allow"
	KindPolicyDeny        = "policy.deny"
	RefineTriggerApproval = "approval"
)

// ProposedPolicyExists reports whether a proposal for this exact rule is
// already waiting, so a second "propose" answer adds nothing.
func (s *Store) ProposedPolicyExists(ctx context.Context, kind, rule string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM refinements WHERE kind = ? AND after = ? AND status = ?`,
		kind, rule, RefineProposed).Scan(&n)
	return n > 0, err
}
```

In `LatestAppliedRound`, change the query to exclude policy rows and update its comment:

```go
// LatestAppliedRound is the most recent round in the session that still has
// an applied row: what "/refine rollback" undoes. Policy rows are not
// rounds anyone rolls back -- the policy view revokes a rule -- so they are
// skipped.
```

```go
		`SELECT round_id FROM refinements WHERE session_id = ? AND status = ? AND kind NOT LIKE 'policy.%' ORDER BY id DESC LIMIT 1`,
```

- [ ] **Step 4: Refine constants, field, Accept and Rollback**

In `internal/refine/refine.go`, add to the Trigger constants:

```go
	// TriggerApproval marks a policy proposal written from an approval
	// answer, not by a round.
	TriggerApproval Trigger = store.RefineTriggerApproval
```

and to the Kind constants:

```go
	KindPolicyAllow = store.KindPolicyAllow
	KindPolicyDeny  = store.KindPolicyDeny
```

(If `refine.go` does not import `store`, add `"github.com/codered/spore/internal/store"`.)

Add to the `Refiner` struct after `Notify`:

```go
	// ApplyPolicy writes an accepted policy proposal into the managed block
	// and makes it live. The daemon sets it to the policy reloader. Nil
	// makes accepting a policy row fail rather than mark it applied with
	// nothing written.
	ApplyPolicy func(decision, rule string) error
```

In `internal/refine/review.go` `Accept`, immediately after the `if row.Status != store.RefineProposed { ... }` block and before `path, err := r.pathFor(row)`, add:

```go
	if strings.HasPrefix(row.Kind, "policy.") {
		return r.acceptPolicy(ctx, row)
	}
```

and add the method below `Accept`:

```go
// acceptPolicy applies a policy proposal. The row is claimed first, so two
// accepts racing cannot both write the rule. There is no before-content to
// compare: config.LearnRule deduplicates, so a rule already in the block is
// applied without a second line.
func (r *Refiner) acceptPolicy(ctx context.Context, row store.Refinement) (store.Refinement, error) {
	moved, err := r.Store.SetRefinementStatus(ctx, row.ID, store.RefineProposed, store.RefineApplied)
	if err != nil {
		return row, err
	}
	if !moved {
		return row, fmt.Errorf("refinement %d changed while it was being accepted", row.ID)
	}
	decision := "allow"
	if row.Kind == KindPolicyDeny {
		decision = "deny"
	}
	var werr error
	switch {
	case r.ApplyPolicy == nil:
		werr = errors.New("policy proposals cannot be applied: no policy writer is attached")
	case row.After == nil:
		werr = fmt.Errorf("refinement %d has no rule", row.ID)
	default:
		werr = r.ApplyPolicy(decision, *row.After)
	}
	if werr != nil {
		_, _ = r.Store.SetRefinementStatus(ctx, row.ID, store.RefineApplied, store.RefineFailed)
		row.Status = store.RefineFailed
		return row, werr
	}
	row.Status = store.RefineApplied
	return row, nil
}
```

Note: `Accept` holds `r.fileMu` for its whole body (it locks at the top); `acceptPolicy` runs under it, which is fine.

In `Rollback`'s loop, after `if row.Status != store.RefineApplied { continue }`, add:

```go
		// A policy rule is removed by revoking it in the policy view, not by
		// rolling back the approval that proposed it.
		if strings.HasPrefix(row.Kind, "policy.") {
			continue
		}
```

Add `"strings"` to `review.go` imports if missing.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/store/ ./internal/refine/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/refine.go internal/store/refine_test.go internal/refine/
git commit -m "refine: carry policy proposals in the ledger and apply them on accept"
```

---

### Task 3: The guard proposes instead of learning

**Files:**
- Modify: `internal/policy/guard.go` (Guard struct, `NewGuard`, remove `SetLearn`, `Run` pattern handling ~line 228-330, `Resolve` ~line 455-500)
- Modify: `cmd/spore/wire.go` (~line 79-84 and ~line 226-238)
- Modify every `NewGuard(` call site (drop the last argument): `internal/policy/*_test.go`, `internal/kernel/policy_test.go`, `internal/mcp/e2e_test.go`, `internal/daemon/policy_routes_test.go`, `internal/daemon/e2e_test.go`, `internal/bridge/discord/helpers_test.go`
- Modify: `internal/policy/reloader_test.go`, `internal/policy/guard_test.go`, `cmd/spore/operator_wire_test.go`
- Test: `internal/policy/guard_test.go`

**Interfaces:**
- Consumes: `Engine.WouldAllow` (Task 1); `store.KindPolicyAllow`, `store.KindPolicyDeny`, `store.RefineTriggerApproval`, `store.ProposedPolicyExists`, `store.RefineProposed` (Task 2); `refine.Refiner.ApplyPolicy` (Task 2).
- Produces: `func NewGuard(inner Runner, e *Engine, ap Approver, st *store.Store) *Guard` (no learn parameter). `Guard.SetLearn` no longer exists.

- [ ] **Step 1: Write the failing tests**

In `internal/policy/guard_test.go`, replace `TestPatternScopeLearnsARule` (whole function) with:

```go
func proposals(t *testing.T, st *store.Store) []store.Refinement {
	t.Helper()
	rows, err := st.Refinements(context.Background(), store.RefineProposed, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// "p" answers this call once and queues the rule for review. Nothing is
// written to the config from the ask window.
func TestPatternAnswerProposesARule(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
	g.Run(ctx, toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
	if len(inner.calls) != 1 {
		t.Fatalf("the call ran %d times, want once", len(inner.calls))
	}
	rows := proposals(t, st)
	if len(rows) != 1 {
		t.Fatalf("proposals = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Kind != store.KindPolicyAllow || r.Trigger != store.RefineTriggerApproval || r.SessionID != sid {
		t.Errorf("row = %+v", r)
	}
	if r.After == nil || *r.After != "fs_write(path matches /ws/src/**)" || r.Target != *r.After {
		t.Errorf("rule: target=%q after=%v", r.Target, r.After)
	}
	if !strings.HasPrefix(r.RoundID, "approval-") {
		t.Errorf("round = %q", r.RoundID)
	}
	if !strings.Contains(r.Rationale, "/ws/src/a.go") {
		t.Errorf("rationale %q should name the path", r.Rationale)
	}
	// The next call in the pattern still asks: nothing changed policy.
	g.Run(ctx, toolCall("fs_write", "c2", `{"path":"/ws/src/b.go"}`))
	if ap.count() != 2 {
		t.Errorf("asked %d times, want 2", ap.count())
	}
	// A second "p" for the same rule adds no row.
	if n := len(proposals(t, st)); n != 1 {
		t.Errorf("proposals after a repeat = %d, want 1", n)
	}
}

// The offer is withheld when the proposed rule could not decide this call.
func TestPatternIsNotOfferedWhenItCouldNotApply(t *testing.T) {
	cases := []struct {
		name string
		pc   config.PolicyConfig
		prof Profile
	}{
		{"narrow written ask", config.PolicyConfig{Ask: []string{"fs_write(path matches **/src/**)"}}, ProfileLocal},
		{"remote profile", config.PolicyConfig{Ask: []string{"fs_write"},
			Profiles: map[string]config.ProfilePolicy{"remote": {Deny: []string{"memory"}}}}, ProfileRemote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
			g, _, st, sid := guardFixture(t, tc.pc, ap)
			ctx := WithSession(context.Background(), Session{ID: sid, Profile: tc.prof, Workspace: "/ws"})
			g.Run(ctx, toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
			if ap.count() != 1 {
				t.Fatalf("asked %d times", ap.count())
			}
			if p := ap.asked[0].Pattern; p != "" {
				t.Errorf("pattern offered: %q", p)
			}
			if n := len(proposals(t, st)); n != 0 {
				t.Errorf("proposals = %d, want 0", n)
			}
		})
	}
}
```

Check `scriptedApprover` in `guard_test.go`: confirm it records each `Ask` in a field (the code above assumes `ap.asked []Ask`); if the field has a different name, use it. Check `recordingRunner` records calls in `inner.calls`; if the field has a different name, use it.

Replace `TestResolveLearnsTheStoredPatternNotARederivedOne` with:

```go
func TestResolveProposesTheStoredPatternNotARederivedOne(t *testing.T) {
	ctx := context.Background()
	g, _, st, sid := guardFixture(t, config.PolicyConfig{}, nil)
	const shown = "fs_write(path matches /ws/notes/**)"
	id, err := st.AddPendingCall(ctx, store.PendingCall{
		SessionID: sid, ToolUseID: "tu1", Tool: "fs_write", Profile: "local", Rule: "fs_write",
		ArgsJSON: []byte(`{"path":"/elsewhere/x/a.go"}`), Pattern: shown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Resolve(ctx, sid, id, Answer{Allow: true, Scope: ScopePattern}); err != nil {
		t.Fatal(err)
	}
	rows := proposals(t, st)
	if len(rows) != 1 || rows[0].After == nil || *rows[0].After != shown {
		t.Fatalf("proposals = %+v, want exactly the pattern that was shown", rows)
	}
}
```

In `TestResolveDowngradesADegradedPatternAnswer`, replace the `learned` slice and the custom `NewGuard(...)` with `g, _, st, sid := guardFixture(t, config.PolicyConfig{}, nil)` (remove the now-duplicate `st`/`sid` setup lines above it), and replace the `if len(learned) != 0` assertion with:

```go
	if n := len(proposals(t, st)); n != 0 {
		t.Fatalf("a rule was proposed for a call with no pattern: %d rows", n)
	}
```

Add a test that `p` never writes the config file. Append:

```go
func TestPatternAnswerLeavesTheConfigUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := []byte("[policy]\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	pc := config.PolicyConfig{Default: "ask", ApprovalTimeout: "5m", Workspace: "/ws", Ask: []string{"fs_write"}}
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
	g, _, _, sid := guardFixture(t, pc, ap)
	_ = NewReloader(path, pc, g) // wired exactly as the daemon wires it
	g.Run(WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"}),
		toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatalf("config changed:\n%s", got)
	}
}
```

Add `"bytes"` and `"os"` to the imports if missing.

In `internal/policy/reloader_test.go`, change the fixture's guard construction to `g := NewGuard(&recordingRunner{}, engine(t, pc), ap, st)` and remove the `var rl *Reloader` forward declaration (`rl = NewReloader(path, pc, g)` stays). Then rewrite `TestAPatternAnswerAppliesToTheVeryNextCall` so it drives the new path: the `p` answer proposes; accepting through `rl.Learn` (what `ApplyPolicy` calls) makes the very next call allowed. Replace that test's body after the first `f.g.Run(f.ctx, call)` and its ask-count check with:

```go
	rows, err := f.g.store.Refinements(context.Background(), store.RefineProposed, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("proposals = %v (err %v), want 1", rows, err)
	}
	// Accepting in review calls the reloader, exactly as Refiner.ApplyPolicy does.
	if err := f.rl.Learn(DecisionAllow, *rows[0].After); err != nil {
		t.Fatal(err)
	}
	f.g.Run(f.ctx, toolCall("fs_write", "c2", `{"path":"/ws/src/b.go"}`))
	if f.ap.count() != 1 {
		t.Errorf("asked %d times; the accepted rule should have allowed the second call", f.ap.count())
	}
```

Read the rest of `reloader_test.go`; any other test there that relied on the guard writing a rule on a `p` answer must instead call `f.rl.Learn(...)` directly. Tests that call `f.rl.Learn`/`f.rl.Unlearn` directly already work.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/policy/`
Expected: build failure (`NewGuard` arity, `proposals` sees no rows).

- [ ] **Step 3: Implement in the guard**

In `internal/policy/guard.go`:

1. Remove the `learn` field and its comment from `Guard`. Change the constructor:

```go
func NewGuard(inner Runner, e *Engine, ap Approver, st *store.Store) *Guard {
	g := &Guard{inner: inner, approver: ap, store: st}
	g.engine.Store(e)
	return g
}
```

2. Delete `SetLearn` and its comment.

3. Update the `Ask.Pattern` field comment: replace `Pattern is the rule "always allow this pattern" would write.` with `Pattern is the rule a "propose this pattern" answer would queue for review.`

4. In `Run`, replace

```go
	pattern, patternOK := PatternFor(c, sess.Workspace)
```

with

```go
	pattern, patternOK := PatternFor(c, sess.Workspace)
	// A proposal that could not decide this call -- a narrower ask the
	// operator wrote, or a profile learned rules never reach -- would only
	// clutter review, so it is not offered.
	if patternOK && !eng.WouldAllow(sess, c, pattern) {
		pattern, patternOK = "", false
	}
```

Update the comment above it ("An empty pattern is the wire signal ...") to say "the "propose this pattern" option".

5. In `Run`, replace the block

```go
		if scope == ScopePattern && g.learn != nil {
			if err := g.learn(decision, pattern); err != nil {
				...
			}
		}
```

with

```go
		if scope == ScopePattern {
			if err := g.propose(book, sess.ID, pendingID, decision, pattern, c); err != nil {
				// Failing to queue the proposal must not change this call's
				// outcome; the user is simply offered it again next time.
				sporetrace.RecordPolicy(ctx, string(decision), "rule proposal not recorded: "+err.Error())
			}
		}
```

6. In `Resolve`, replace the block

```go
	if ans.Scope == ScopePattern && g.learn != nil {
		if pattern := claimed.Pattern; pattern != "" {
			if err := g.learn(decision, pattern); err != nil {
				...
			}
		}
	}
```

with

```go
	if ans.Scope == ScopePattern {
		if pattern := claimed.Pattern; pattern != "" {
			c := Call{Tool: claimed.Tool, Args: claimed.ArgsJSON}
			if err := g.propose(ctx, claimed.SessionID, pendingID, decision, pattern, c); err != nil {
				// Same invariant as Run: failing to queue the proposal must
				// not undo an answer already recorded.
				sporetrace.RecordPolicy(ctx, string(decision), "rule proposal not recorded: "+err.Error())
			}
		}
	}
```

(Check `claimed`'s field names in `store.PendingCall` — `Tool`, `ArgsJSON`, `SessionID`, `Pattern` — and adjust if they differ.)

7. Add the method after `PatternFor`:

```go
// propose queues a rule for the operator to review. It never edits the
// config: a policy change made inside a session's ask window is the lever a
// tired human or a prompt injection reaches for, so the change waits for
// the refinements view. A rule already waiting is not queued twice.
func (g *Guard) propose(ctx context.Context, sessionID string, pendingID int64, d Decision, rule string, c Call) error {
	kind := store.KindPolicyAllow
	if d == DecisionDeny {
		kind = store.KindPolicyDeny
	}
	exists, err := g.store.ProposedPolicyExists(ctx, kind, rule)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	where := ""
	if paths := argPaths(c); len(paths) > 0 {
		where = " on " + paths[0]
	}
	after := rule
	_, err = g.store.AddRefinement(ctx, store.Refinement{
		RoundID:   fmt.Sprintf("approval-%d", pendingID),
		SessionID: sessionID,
		Trigger:   store.RefineTriggerApproval,
		Kind:      kind,
		Target:    rule,
		After:     &after,
		Rationale: fmt.Sprintf("%s %s%s, answered in session %s", d, c.Tool, where, sessionID),
		Status:    store.RefineProposed,
	})
	return err
}
```

- [ ] **Step 4: Update every NewGuard call site and the wiring**

Run `grep -rn "NewGuard(" --include=*.go .` and remove the last argument from each call (it is `nil`, a `learn` variable, or an inline `func(...)`). For inline funcs that recorded learned rules in a test, delete the recording variable and any assertion on it only if the test is not one of those rewritten above; if such a test asserted learning behaviour, convert the assertion to read `store.Refinements(ctx, store.RefineProposed, 10)` the same way as `TestPatternAnswerProposesARule`.

In `cmd/spore/wire.go`:
- Delete the `learn := func(...)` closure and change the call to `guard := policy.NewGuard(reg, engine, approver, st)`.
- In `buildServer`, replace

```go
	// A learned rule is live the moment it is written: the reloader rewrites
	// the managed block and swaps a rebuilt engine into this guard. Set
	// before any turn can run.
	reloader := policy.NewReloader(cfg.Path, cfg.Policy, guard)
	guard.SetLearn(reloader.Learn)
```

with

```go
	// An accepted policy proposal is live the moment it is written: the
	// reloader rewrites the managed block and swaps a rebuilt engine into
	// this guard.
	reloader := policy.NewReloader(cfg.Path, cfg.Policy, guard)
```

and after the `ref, ok := a.Refine.(*refine.Refiner)` check add (next to `ref.Notify = ...`):

```go
	ref.ApplyPolicy = func(d, rule string) error { return reloader.Learn(policy.Decision(d), rule) } // set before any accept can run
```

Check whether `reloader` is used elsewhere in `buildServer` (e.g. passed to `srv.AttachOperator` for revoke); keep those uses.

- [ ] **Step 5: Update cmd/spore/operator_wire_test.go**

This test answers `p` and expects the rule to be live. Read it. Change it so that after the `p` answer it (a) asserts one `proposed` refinement row exists, (b) accepts it through the HTTP route `POST /api/refinements/{id}/accept` on `ts` (use the same request helper the test already uses for other routes, or `http.Post`), and (c) asserts the next call is allowed without asking and that `config.toml` now contains the rule. Remove the line that deletes `fs_write` from `cfg.Policy.Ask` and its comment: with tiered precedence the learned path rule must win over the default bare `fs_write` ask, and this test now proves that end to end.

- [ ] **Step 6: Run everything**

Run: `go build ./... && go test ./internal/policy/ ./internal/refine/ ./internal/kernel/ ./internal/mcp/ ./internal/daemon/ ./internal/bridge/... ./cmd/...`
Expected: PASS.

- [ ] **Step 7: Mutation check**

Temporarily replace the body of `propose` with a call to `config.LearnRule` against a temp path, or simply make `Run` skip the `WouldAllow` gate; run `go test ./internal/policy/ -run 'TestPatternAnswerProposesARule|TestPatternIsNotOfferedWhenItCouldNotApply'` and confirm at least one FAILS for each mutation. Revert.

- [ ] **Step 8: Commit**

```bash
git add -A internal/ cmd/
git commit -m "policy: a pattern answer proposes a rule for review instead of writing it"
```

Use `git add -A internal/ cmd/` only after `git status` shows no stray files (binaries, coverage files); otherwise add paths explicitly.

---

### Task 4: Approval labels and the refinements views

**Files:**
- Modify: `cmd/spore/approve.go` (~line 32-40)
- Modify: `internal/tui/approval.go` (~line 72-73), `internal/tui/app.go` (~line 1501-1502), `internal/tui/header.go` (~line 161-162)
- Modify: `internal/tui/res_refine.go` (`refineTarget`, `Detail`)
- Modify: `internal/bridge/discord/approve.go` (~line 119), `internal/bridge/discord/render.go` (~line 199-205)
- Modify: `web/app.js` (~line 754-766 approval card; ~line 1161-1208 refinements view; ~line 1262 help row)
- Test: the existing golden/unit tests for these files; update goldens deliberately.

**Interfaces:**
- Consumes: kind strings `policy.allow` / `policy.deny` (Task 2).

- [ ] **Step 1: Change the labels**

Exact replacements:

`cmd/spore/approve.go`: `[p]attern (always %s)` → `[p]ropose (allow once, queue %s for review)`.

`internal/tui/approval.go`: `styMuted.Render(" always allow ")` → `styMuted.Render(" allow once + propose ")`.

`internal/tui/app.go`: `return "Always allow " + ev.Pattern + "?"` → `return "Allow once and propose " + ev.Pattern + " for review?"`.

`internal/tui/header.go`: `hint("p", "pattern")` → `hint("p", "propose")`.

`internal/bridge/discord/approve.go`: `truncateLabel("always allow " + ev.Pattern)` → `truncateLabel("allow once + propose " + ev.Pattern)`.

`internal/bridge/discord/render.go` lines ~199-205: read them; wherever the text says "always" next to the pattern, change it to "propose".

`web/app.js`:
- `options.push(["Always " + a.pattern, "p", true, "pattern", ""]);` → `options.push(["Allow once + propose " + a.pattern, "p", true, "pattern", ""]);`
- `"Always allow writes the pattern to the learned block of your policy."` → `"Propose queues the pattern in Refinements for you to accept; nothing changes until you do."`
- help row `["y n s p", "approval: allow once, deny, this session, always pattern"]` → `["y n s p", "approval: allow once, deny, this session, propose pattern"]`

- [ ] **Step 2: Render policy rows in the refinements views**

`internal/tui/res_refine.go`, change `Detail` so policy rows show no before/after diff:

```go
func (refinementsRes) Detail(r Row) string {
	x, ok := r.Data.(daemon.RefinementJSON)
	if !ok {
		return ""
	}
	if strings.HasPrefix(x.Kind, "policy.") {
		return fmt.Sprintf("refinement %d — %s (%s)\nrule: %s\nsession: %s  trigger: %s\nwhy: %s\n\naccepting writes this rule to the managed block of config.toml and applies it at once\n",
			x.ID, x.Kind, x.Status, x.Target, x.SessionID, x.Trigger, x.Rationale)
	}
	return fmt.Sprintf("refinement %d — %s %s (%s)\nsession: %s  round: %s  trigger: %s\nwhy: %s\n\n--- before\n%s\n\n+++ after\n%s\n",
		x.ID, x.Kind, x.Target, x.Status, x.SessionID, x.RoundID, x.Trigger, x.Rationale,
		refineContent(x.Before), refineContent(x.After))
}
```

Read `Actions()` in the same file: if there is a roll-back action that applies to `applied` rows, make it not apply when `strings.HasPrefix(x.Kind, "policy.")`.

`web/app.js` refinements view:
- On the "Roll back round" action, change `applies: (r) => r.status === "applied",` to `applies: (r) => r.status === "applied" && !String(r.kind).startsWith("policy."),`.
- Change `detail:` to show policy rows without the before/after columns:

```js
        detail: (r) => String(r.kind).startsWith("policy.")
          ? h("div", {},
              h("h3", { text: "#" + r.id + " · " + r.kind }),
              h("pre", { text: r.target }),
              h("div", { class: "prose", text: r.rationale || "" }),
              h("div", { class: "fine", text: "Accepting writes this rule to the managed block of config.toml and applies it at once." }))
          : h("div", {},
              h("h3", { text: "#" + r.id + " · " + r.kind + " · " + r.target + " · round " + r.round_id }),
              h("div", { class: "prose", text: r.rationale || "" }),
              h("div", { class: "cols" },
                h("div", {}, h("div", { class: "view-sub", text: "before" }), h("pre", { text: r.before === null ? "(none)" : r.before })),
                h("div", {}, h("div", { class: "view-sub", text: "after" }), h("pre", { text: r.after === null ? "(none)" : r.after })))),
```

- [ ] **Step 3: Add a TUI test for the policy detail**

Append to the TUI test file that covers `res_refine.go` (find it with `grep -ln "refinementsRes" internal/tui/*_test.go`; if none, add to `internal/tui/res_test.go`):

```go
func TestRefinementDetailShowsAPolicyRuleNotADiff(t *testing.T) {
	after := "fs_write(path matches /ws/a/**)"
	row := Row{Data: daemon.RefinementJSON{ID: 7, Kind: "policy.allow", Target: after, After: &after, Status: "proposed", Rationale: "allow fs_write on /ws/a/x"}}
	got := refinementsRes{}.Detail(row)
	if !strings.Contains(got, "rule: "+after) || strings.Contains(got, "--- before") {
		t.Errorf("detail:\n%s", got)
	}
}
```

- [ ] **Step 4: Run tests and refresh goldens deliberately**

Run: `go test ./internal/tui/ ./internal/bridge/... ./cmd/... ./internal/daemon/`
If golden tests fail only because the label text changed, regenerate them with the package's update flag (find it: `grep -rn "flag.Bool(\"update" internal/tui`), then `git diff internal/tui/testdata` and confirm the only differences are the `p` label text. Any other golden difference is a bug: fix it, do not accept it.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/spore/approve.go internal/tui internal/bridge/discord web/app.js
git commit -m "ui: p proposes a rule; refinements views show policy rows as rules"
```

---

### Task 5: Daemon token, route guard and Host check

**Files:**
- Create: `internal/daemon/auth.go`
- Create: `internal/daemon/auth_test.go`
- Modify: `internal/daemon/server.go` (`Options`, `Server`, `New`, `Handler`)
- Modify: `internal/daemon/web.go` (`handleIndex`)
- Modify: `cmd/spore/wire.go` (`buildServer`)
- Modify: daemon and cmd tests that build a server (see Step 6)

**Interfaces:**
- Produces:
  - `const daemon.TokenFile = "daemon.token"`
  - `func daemon.LoadOrCreateToken(dataDir string) (string, error)`
  - `func daemon.ReadToken(dataDir string) (string, error)`
  - `daemon.Options.Token string`
  - `func (s *Server) Token() string`
  - Cookie name `spore_token`; query parameter `token` on `GET /`.

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/auth_test.go`:

```go
package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func bareServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	return New(Options{Cfg: cfg, Token: testToken})
}

func TestTokenFileIsCreatedPrivateAndReused(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 {
		t.Errorf("token length %d, want 64 hex chars", len(a))
	}
	fi, err := os.Stat(filepath.Join(dir, TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	b, _ := LoadOrCreateToken(dir)
	if a != b {
		t.Error("a second load made a new token; it must persist")
	}
	got, err := ReadToken(dir)
	if err != nil || got != a {
		t.Errorf("ReadToken = %q, %v", got, err)
	}
	_ = os.Remove(filepath.Join(dir, TokenFile))
	c, _ := LoadOrCreateToken(dir)
	if c == a || len(c) != 64 {
		t.Error("deleting the file must rotate the token")
	}
}

// Every /api route registered in Handler refuses a request without the
// token and accepts one with it, by header or by cookie.
func TestEveryAPIRouteNeedsTheToken(t *testing.T) {
	s := bareServer(t)
	h := s.Handler()
	if len(s.apiPatterns) < 30 {
		t.Fatalf("only %d api patterns recorded; Handler must register /api routes through api()", len(s.apiPatterns))
	}
	for _, p := range s.apiPatterns {
		method, path, _ := strings.Cut(p, " ")
		path = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "1")
		for _, auth := range []string{"none", "header", "cookie", "wrong"} {
			// A short deadline: the event-stream routes hold the request open
			// until the client goes away.
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			req := httptest.NewRequest(method, "http://127.0.0.1:7777"+path, nil).WithContext(ctx)
			switch auth {
			case "header":
				req.Header.Set("Authorization", "Bearer "+testToken)
			case "cookie":
				req.AddCookie(&http.Cookie{Name: "spore_token", Value: testToken})
			case "wrong":
				req.Header.Set("Authorization", "Bearer nope")
			}
			rec := httptest.NewRecorder()
			func() {
				// Handlers behind the guard may panic on a bare server with
				// no store; only the guard's answer matters here.
				defer func() { _ = recover() }()
				h.ServeHTTP(rec, req)
			}()
			cancel()
			unauth := rec.Code == http.StatusUnauthorized
			want := auth == "none" || auth == "wrong"
			if unauth != want {
				t.Errorf("%s with %s: status %d", p, auth, rec.Code)
			}
		}
	}
}

// A route added later with mux.HandleFunc directly would skip the guard.
func TestNoAPIRouteIsRegisteredOutsideTheGuard(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, " /api/") && strings.Contains(line, "mux.Handle") {
			t.Errorf("server.go:%d registers an /api route outside api(): %s", i+1, strings.TrimSpace(line))
		}
	}
}

func TestOpenRoutesStayOpen(t *testing.T) {
	h := bareServer(t).Handler()
	for _, path := range []string{"/healthz", "/static/app.js", "/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777"+path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200", path, rec.Code)
		}
	}
}

func TestIndexWithoutACookieSaysRunSporeWeb(t *testing.T) {
	h := bareServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777/", nil))
	if !strings.Contains(rec.Body.String(), "spore web") || strings.Contains(rec.Body.String(), `id="transcript"`) {
		t.Errorf("body:\n%s", rec.Body.String())
	}
}

func TestATokenQuerySetsTheCookieAndRedirects(t *testing.T) {
	h := bareServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777/?token="+testToken, nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != "spore_token" || c[0].Value != testToken || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Path != "/" {
		t.Errorf("cookie = %+v", c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777/?token=wrong", nil))
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a wrong token set a cookie")
	}
}

func TestAForeignHostIsRefused(t *testing.T) {
	h := bareServer(t).Handler()
	for host, want := range map[string]int{
		"evil.example:7777": http.StatusForbidden,
		"evil.example":      http.StatusForbidden,
		"localhost:7777":    http.StatusOK,
		"127.0.0.1:9999":    http.StatusOK,
		"[::1]:7777":        http.StatusOK,
	} {
		req := httptest.NewRequest("GET", "http://x/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.Code, want)
		}
	}
}

func TestAnEmptyServerTokenRefusesEverything(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	h := New(Options{Cfg: cfg}).Handler()
	req := httptest.NewRequest("GET", "http://127.0.0.1:7777/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rec.Code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/daemon/ -run 'Token|APIRoute|OpenRoutes|IndexWithout|TokenQuery|ForeignHost|EmptyServerToken'`
Expected: build failure (undefined `LoadOrCreateToken`, `Options.Token`, `apiPatterns`).

- [ ] **Step 3: Write auth.go**

Create `internal/daemon/auth.go`:

```go
package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// TokenFile is the daemon's credential, in the data directory. Every /api
// route requires it: without one, anything that can reach the loopback
// address -- including a model's approved shell, or web_fetch -- could
// answer approvals, accept policy proposals or revoke learned rules.
const TokenFile = "daemon.token"

const tokenCookie = "spore_token"

// LoadOrCreateToken returns the daemon token, creating it on first start.
// It persists across restarts so a signed-in browser tab survives one;
// deleting the file rotates it.
func LoadOrCreateToken(dataDir string) (string, error) {
	tok, err := ReadToken(dataDir)
	if err == nil {
		return tok, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok = hex.EncodeToString(buf)
	path := filepath.Join(dataDir, TokenFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: dataDir is from the validated config
	if errors.Is(err, os.ErrExist) {
		// Another process created it between the read and the open.
		return ReadToken(dataDir)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	return tok, f.Close()
}

// ReadToken reads the token a client sends. An empty or malformed file is
// an error rather than an empty token, so a client never sends "Bearer ".
func ReadToken(dataDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, TokenFile)) //nolint:gosec // G304: dataDir is from the validated config
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if len(tok) != 64 {
		return "", fmt.Errorf("%s is malformed; delete it and restart spore", filepath.Join(dataDir, TokenFile))
	}
	return tok, nil
}

// authorized reports whether the request carries the token, by bearer
// header or by the cookie spore web sets. An empty server token authorises
// nothing: a daemon wired without one fails closed.
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(tokenCookie); err == nil {
		got = c.Value
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// requireToken wraps an /api handler.
func (s *Server) requireToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "missing or wrong daemon token: run `spore web` to sign in a browser")
			return
		}
		h(w, r)
	}
}

// checkHost refuses a request addressed to any host but this machine's
// loopback names or the configured address. DNS rebinding reaches a local
// daemon through a hostname the attacker controls; the port does not
// matter, the hostname does.
func (s *Server) checkHost(next http.Handler) http.Handler {
	configured := ""
	if s.cfg != nil {
		if h, _, err := net.SplitHostPort(s.cfg.Daemon.Addr); err == nil {
			configured = h
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		switch {
		case host == "localhost", host == "127.0.0.1", host == "::1":
		case configured != "" && host == configured:
		default:
			writeError(w, http.StatusForbidden, "host %q is not this daemon", r.Host)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 4: Wire it into the server**

In `internal/daemon/server.go`:
- Add `Token string` to `Options` with comment `// Token is the daemon credential every /api route requires. Empty refuses every /api request.`
- Add fields to `Server`: `token string` and `apiPatterns []string` (comment: `// apiPatterns records every route registered through api(), for the test that walks them.`).
- In `New`, set `token: o.Token`.
- Add `func (s *Server) Token() string { return s.token }` with comment `// Token is the credential in-process tests hand to a client.`
- In `Handler()`, add at the top after `mux := http.NewServeMux()`:

```go
	s.apiPatterns = nil
	// api registers a route behind the token. Every /api route goes through
	// it; a test fails if one is registered with mux.HandleFunc directly.
	api := func(pattern string, h http.HandlerFunc) {
		s.apiPatterns = append(s.apiPatterns, pattern)
		mux.HandleFunc(pattern, s.requireToken(h))
	}
```

  and change every `mux.HandleFunc("<METHOD> /api/...", ...)` line to `api("<METHOD> /api/...", ...)`. Leave `/healthz`, `/static/{file}` and `/` on `mux.HandleFunc`. Change `return mux` to `return s.checkHost(mux)`.

In `internal/daemon/web.go` `handleIndex`, after the `r.URL.Path != "/"` check, insert:

```go
	if tok := r.URL.Query().Get("token"); tok != "" {
		if s.token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		// Redirect either way, so the token never stays in the address bar
		// or the history.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, signInPage)
		return
	}
```

and add near the top of `web.go`:

```go
// signInPage is what a browser without the cookie sees: the UI would only
// fail on every request.
const signInPage = `<!doctype html><meta charset="utf-8"><title>spore</title>` +
	`<body style="font-family:system-ui;padding:2rem"><p>Run <code>spore web</code> in a terminal to open the UI signed in.</p></body>`
```

Add imports `crypto/subtle` and `io` to `web.go`.

In `cmd/spore/wire.go` `buildServer`, before `srv := daemon.New(...)`:

```go
	// The token file is written before the listener opens, so a client that
	// started this daemon can read it as soon as /healthz answers.
	token, err := daemon.LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("daemon token: %w", err)
	}
```

and pass `Token: token` in `daemon.Options`.

- [ ] **Step 5: Run the new tests**

Run: `go test ./internal/daemon/ -run 'Token|APIRoute|OpenRoutes|IndexWithout|TokenQuery|ForeignHost|EmptyServerToken'`
Expected: PASS.

- [ ] **Step 6: Fix the existing tests**

Run `go test ./internal/daemon/ ./cmd/... ./internal/bridge/...`. Tests that drive the handler now get 401. Fix them this way, not by weakening the guard:

- In `internal/daemon/api_test.go` add:

```go
// authed lets the existing transport tests keep using plain http.Get and
// http.Post: it adds the bearer header to every request that has none.
func authed(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+testToken)
		}
		h.ServeHTTP(w, r)
	})
}
```

  In `newTestServer`, use `New(Options{Agent: a, Store: st, Cfg: cfg, Token: testToken})` and `httptest.NewServer(authed(s.Handler()))`.
- Every other `daemon` test that calls `New(Options{...})` and serves `Handler()` (find with `grep -n "Handler()" internal/daemon/*_test.go`): add `Token: testToken` and wrap with `authed(...)`. Tests that call `h.ServeHTTP` on a recorder: set the header on the request instead.
- `cmd/spore` tests that call `buildServer` and then make a `newClient`: set `c.token = srv.Token()` (the field is added in Task 6; if Task 6 is not done yet, do this step's cmd part in Task 6). Tests that call `daemon.New` directly: add `const testDaemonToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"` (64 `a`s) to one `cmd/spore` test file, pass `Token: testDaemonToken`, and set the client's `token` to it.
- `TestIndexRenders` in `web_test.go` keeps passing through `authed` (the header authorises `/`).

Run: `go test ./internal/daemon/ ./internal/bridge/...`
Expected: PASS.

- [ ] **Step 7: Mutation check**

Temporarily change one route in `Handler()` from `api(` to `mux.HandleFunc(`. Run `go test ./internal/daemon/ -run 'TestNoAPIRouteIsRegisteredOutsideTheGuard|TestEveryAPIRouteNeedsTheToken'`. Expected: FAIL. Revert.

- [ ] **Step 8: Commit**

```bash
git add internal/daemon/ cmd/spore/wire.go
git commit -m "daemon: require a token on every /api route and check the Host"
```

---

### Task 6: Clients send the token, `spore web`, baseline deny

**Files:**
- Modify: `cmd/spore/client.go` (struct, `do`, stream request ~line 205-215)
- Modify: `cmd/spore/autostart.go` (`ensureDaemon`)
- Modify: `cmd/spore/session_delete.go` (~line 23)
- Create: `cmd/spore/web.go`, `cmd/spore/web_test.go`
- Modify: `cmd/spore/main.go` (`dispatch`, `usage`)
- Modify: `internal/config/config.go` (`baselineDeny`)
- Modify: `web/app.js` (`api()` 401 handling)
- Modify: cmd tests from Task 5 Step 6 that still fail
- Test: `cmd/spore/client_test.go`, `cmd/spore/web_test.go`, `internal/policy/engine_test.go`, `internal/daemon/web_test.go`

**Interfaces:**
- Consumes: `daemon.ReadToken`, `daemon.TokenFile`, `Server.Token()` (Task 5).
- Produces: `client.token string` field; `func cmdWeb(ctx context.Context, cfg *config.Config, out io.Writer, tty bool, open func(string) error) error`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/spore/client_test.go`:

```go
func TestClientSendsTheTokenOnRequestsAndStreams(t *testing.T) {
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/events") {
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := newClient(strings.TrimPrefix(ts.URL, "http://"))
	c.token = "abc"
	_ = c.do(context.Background(), "GET", "/api/sessions", nil, nil)
	if len(seen) != 1 || seen[0] != "/api/sessions Bearer abc" {
		t.Errorf("seen = %v", seen)
	}
}
```

Read `client.go` around line 200-215 to find the streaming method name (the one using `c.streamClient`), and add a second assertion calling it against `/api/sessions/x/events` with a short-timeout context, checking `seen` includes `Bearer abc` for that path.

Create `cmd/spore/web_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
)

func TestSporeWebPrintsTheURLOnlyToATerminal(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Daemon.Addr = "127.0.0.1:7777"
	tok, err := daemon.LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	var opened string
	open := func(u string) error { opened = u; return nil }

	var out bytes.Buffer
	if err := cmdWebURL(cfg, &out, true, open); err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:7777/?token=" + tok
	if opened != want || !strings.Contains(out.String(), want) {
		t.Errorf("tty: opened %q, printed %q", opened, out.String())
	}

	out.Reset()
	opened = ""
	if err := cmdWebURL(cfg, &out, false, open); err != nil {
		t.Fatal(err)
	}
	if opened != want {
		t.Errorf("no tty: opened %q", opened)
	}
	if strings.Contains(out.String(), tok) {
		t.Errorf("printed the token without a terminal: %q", out.String())
	}
}
```

Append to `internal/policy/engine_test.go` inside or beside `TestBaselineHoldsAnAllowedShellAwayFromSecretsAndHome` (read it first and follow its setup through `config.Load`):

```go
func TestBaselineKeepsTheModelAwayFromTheDaemonToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.toml")
	cfgText := "default_model = \"anthropic/claude-opus-5\"\n[policy]\ndefault = \"allow\"\nallow = [\"shell_exec\", \"fs_read\"]\nask = []\n"
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := NewEngine(cfg.Policy)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// The workspace contains the token, so the outside-workspace rule is
	// not what denies it.
	sess := Session{Profile: ProfileLocal, Workspace: "/home/u"}
	for _, tc := range []struct{ tool, args, ruleHas string }{
		{"fs_read", `{"path":"/home/u/.spore/daemon.token"}`, "daemon.token"},
		{"shell_exec", `{"command":"cat ~/.spore/daemon.token"}`, "daemon.token"},
		{"shell_exec", `{"command":"spore web"}`, "spore web"},
		{"shell_exec", `{"command":"/usr/local/bin/spore   web"}`, "spore web"},
	} {
		got := e.Evaluate(sess, Call{Tool: tc.tool, Args: json.RawMessage(tc.args)})
		if got.Decision != DecisionDeny || !strings.Contains(got.Rule, tc.ruleHas) {
			t.Errorf("%s %s = %s by %q, want deny by a rule naming %q", tc.tool, tc.args, got.Decision, got.Rule, tc.ruleHas)
		}
	}
}
```

This loads through `config.Load` because that is what adds the baseline; an engine built from `config.Default()` would test nothing.

Append to `internal/daemon/web_test.go`:

```go
func TestAppJSExplainsA401(t *testing.T) {
	body, err := web.FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "res.status === 401") || !strings.Contains(string(body), "spore web") {
		t.Error("app.js must tell the user to run spore web when the daemon answers 401")
	}
}
```

(Check the import name of the embedded assets package in `web_test.go`; `TestStaticAssetsAreEmbedded` shows it.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/spore/ ./internal/policy/ ./internal/daemon/ -run 'Token|SporeWeb|DaemonToken|AppJSExplainsA401'`
Expected: build failure / FAIL.

- [ ] **Step 3: Client sends the token**

In `cmd/spore/client.go`:
- Add field `token string // token is the daemon credential, sent as a bearer header. Empty sends nothing; the daemon then answers 401.`
- Add:

```go
// authorize adds the daemon token to one request.
func (c *client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
```

- In `do`, after the `X-Spore-Client` block, call `c.authorize(req)`. In the streaming method (the `http.NewRequestWithContext(ctx, "GET", c.base+path, nil)` near line 207), call `c.authorize(req)` before `c.streamClient.Do(req)`.

In `cmd/spore/autostart.go` `ensureDaemon`: after `c := newClient(cfg.Daemon.Addr)` add `c.token, _ = daemon.ReadToken(cfg.DataDir)`. At the end, after the newly started daemon passes `waitForHealth`, re-read: `c.token, _ = daemon.ReadToken(cfg.DataDir)` (the daemon writes the file before it listens). Add the `daemon` import if missing.

In `cmd/spore/session_delete.go` after `c := newClient(cfg.Daemon.Addr)`: `c.token, _ = daemon.ReadToken(cfg.DataDir)`.

Run `grep -rn "newClient(" cmd/spore/*.go` and check every production call site sets the token.

- [ ] **Step 4: spore web**

Create `cmd/spore/web.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/mattn/go-isatty"
)

// cmdWeb opens the web UI signed in. The URL carries the daemon token once;
// the daemon swaps it for a cookie and redirects it out of the address bar.
func cmdWeb(ctx context.Context, cfg *config.Config) error {
	if _, err := ensureDaemon(ctx, cfg); err != nil {
		return err
	}
	tty := isatty.IsTerminal(os.Stdout.Fd())
	return cmdWebURL(cfg, os.Stdout, tty, openBrowser)
}

// cmdWebURL prints the URL only to a terminal: a model running spore web
// through shell_exec has no terminal, and must not be handed the token.
func cmdWebURL(cfg *config.Config, out io.Writer, tty bool, open func(string) error) error {
	tok, err := daemon.ReadToken(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("read the daemon token: %w", err)
	}
	u := fmt.Sprintf("http://%s/?token=%s", cfg.Daemon.Addr, tok)
	openErr := open(u)
	if tty {
		_, _ = fmt.Fprintf(out, "opening %s\n", u)
	} else {
		_, _ = fmt.Fprintln(out, "opened the spore web UI in your browser")
	}
	if openErr != nil && tty {
		_, _ = fmt.Fprintf(out, "could not open a browser (%v); open the URL above by hand\n", openErr)
	}
	return nil
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
```

In `cmd/spore/main.go` `dispatch`, add before `default:`:

```go
	case "web":
		return cmdWeb(ctx, cfg)
```

and in `usage`, after the `spore serve --stop` line, add:

```
  spore web                    open the web UI in your browser, signed in
```

(Run `make lint`; if gosec flags `exec.Command` with a variable argument, add `//nolint:gosec // G204: u is the local daemon URL built above` on those lines, matching the repo's existing nolint style.)

- [ ] **Step 5: Baseline deny**

In `internal/config/config.go` `baselineDeny`:
- Append `, **/daemon.token` to the end of the glob list in the `fs_*(path matches ...)` rule.
- Append `, **/daemon.token` to the end of the glob list in the `shell_exec(word matches ...)` rule.
- Add a new entry after the `rm -rf` rule:

```go
	// The daemon token is what every /api route checks. fs_read and cat are
	// held off it above; this keeps the model from running the command that
	// opens a signed-in browser. An interpreter run through shell_exec can
	// still read the file -- spore runs as the operator -- so this is a
	// speed bump, and the spec says so.
	"shell_exec(matches spore web)",
```

If a test in `internal/config` or `internal/policy` asserts the exact baseline list or count, update it to include the additions.

- [ ] **Step 6: Web UI on 401**

In `web/app.js` `api()`, inside `if (!res.ok) {`, before building the error, add:

```js
    if (res.status === 401) {
      // The daemon wants its token: this tab was opened without spore web,
      // or the token was rotated.
      const err = new Error("Signed out: run `spore web` in a terminal to open the UI again.");
      err.status = 401;
      throw err;
    }
```

- [ ] **Step 7: Finish the cmd tests left from Task 5**

Run `go test ./cmd/...`. For every test that builds a server and a client: set `c.token = srv.Token()` (for `buildServer`) or pass `Token: tok` to `daemon.New` and set `c.token = tok`. `autostart_test.go` fakes the daemon with its own handler; it only needs changes if it asserts headers.

- [ ] **Step 8: Run tests**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add cmd/spore internal/config/config.go internal/policy/engine_test.go internal/daemon/web_test.go web/app.js
git commit -m "spore web, clients send the daemon token, and the baseline holds the token back"
```

---

### Task 7: Docs, backlog and the full gate

**Files:**
- Modify: `README.md` (~line 179, ~line 371, ~line 554)
- Modify: `assets/demo/record.sh` (lines 24, 74)
- Modify: `docs/backlog.md` (the two Open entries)
- Modify: `docs/superpowers/specs/2026-08-29-spore-design.md` (section 6, policy)

- [ ] **Step 1: README**

- Line ~179: `open http://127.0.0.1:7777         # the same sessions in your browser` → `spore web                         # the same sessions in your browser`
- Line ~371: change "Open `http://127.0.0.1:7777/` while the daemon runs." to "Run `spore web` while the daemon runs; it opens the UI signed in. Every `/api` route needs the token in `~/.spore/daemon.token`, so a plain browser tab or `curl` without it gets 401."
- Line ~554: the jobs example becomes:

```bash
curl -s localhost:7777/api/jobs \
  -H "Authorization: Bearer $(cat ~/.spore/daemon.token)" \
  -d '{"spec":"0 9 * * 1-5","prompt":"summarise yesterday'\''s commits"}'
```

- Find the README's policy section (`grep -n "always allow\|pattern" README.md`) and update any text saying `p` writes a rule: it now proposes one for review in Refinements (`R`), and precedence is "deny, then the narrowest matching tier (argument rule > exact tool > tool glob), ask winning ties, then the default".

- [ ] **Step 2: record.sh**

Replace `"http://$addr/api/sessions"` with `"http://$addr/healthz"` on both lines.

- [ ] **Step 3: Backlog**

In `docs/backlog.md`, change the heading `## A pattern answer cannot outrank a hand-written ask` to `## A pattern answer cannot outrank a hand-written ask: fixed`, replace "Open." with "Closed." and add a short paragraph after the existing text answering the two open questions: (1) a learned allow outranks a written ask only when it is in a narrower tier; same-tier ties go to ask; (2) `p` is offered only when the rule it proposes would decide the call, and `p` now proposes a refinement instead of writing the config. Link the spec path.

Do the same for `## Operator routes share the daemon's unauthenticated API`: "Closed." Answer: (1) the baseline denies `fs_*`/`shell_exec` reading `daemon.token` and `shell_exec(matches spore web)`; it does not try to block reaching the address, which `python -c` would bypass; (2) every `/api` route requires the token (bearer header or the `spore web` cookie), and a Host check refuses foreign hostnames. State the remaining gap: an interpreter run through an approved `shell_exec` can still read the token, because spore runs as the operator.

- [ ] **Step 4: Design spec section 6**

Read section 6 of `docs/superpowers/specs/2026-08-29-spore-design.md`. Where it describes evaluation order ("first match", "allow and ask in configured order" or similar) and the "always allow this pattern" answer, replace with the tiered precedence and the proposal flow, and add one sentence on the daemon token. Reference the new spec by path.

- [ ] **Step 5: The full gate**

Run each and confirm clean output:

```bash
go vet ./...
make fmtcheck
make lint
make vulncheck
go mod tidy && git diff --exit-code go.mod go.sum
go test ./...
```

Expected: all pass, no diff from tidy.

- [ ] **Step 6: Commit**

```bash
git add README.md assets/demo/record.sh docs/
git commit -m "docs: spore web, the token, tiered precedence, and close two backlog entries"
```
