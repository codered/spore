package policy

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/codered/spore/internal/config"
)

// Result is one policy decision, carrying the rule that produced it so the
// audit log, the span and the model's error message all name the same thing.
type Result struct {
	Decision Decision
	Rule     string
	// Detail explains one deny in terms the model can act on: which argument
	// value offended, where it resolved and what the bound was. Only the MCP
	// containment rule fills it; every other decision leaves it empty.
	Detail string
}

// ruleset is the ordered evaluation list for one trust profile. deny is held
// separately because it is evaluated first and cannot be overridden.
type ruleset struct {
	deny        []Rule
	allowAndAsk []Rule
	fallback    Decision
}

type Engine struct {
	env      Env
	base     ruleset
	profiles map[Profile]ruleset
	timeout  time.Duration
}

func parseAll(d Decision, srcs []string) ([]Rule, error) {
	var out []Rule
	for _, s := range srcs {
		r, err := ParseRule(d, s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// NewEngine compiles every configured rule up front, so a typo in policy is a
// startup error rather than a surprise at the first tool call.
func NewEngine(cfg config.PolicyConfig) (*Engine, error) {
	timeout, err := time.ParseDuration(cfg.ApprovalTimeout)
	if err != nil {
		return nil, fmt.Errorf("policy.approval_timeout %q: %w", cfg.ApprovalTimeout, err)
	}
	base, err := buildRuleset(cfg.Default, cfg.Allow, cfg.Ask, cfg.Deny, cfg.Learned)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		env:      Env{Workspace: cfg.Workspace, MCP: cfg.MCPPaths},
		base:     base,
		profiles: map[Profile]ruleset{},
		timeout:  timeout,
	}
	for name, p := range cfg.Profiles {
		def := p.Default
		if def == "" {
			def = cfg.Default
		}
		// Deny is global: a profile's own deny rules extend the base set,
		// they never replace it.
		deny := append(append([]string{}, cfg.Deny...), p.Deny...)
		allow, ask := p.Allow, p.Ask
		// A profile that names neither allow nor ask is not asking for an
		// empty ruleset — it inherits the base's, the same way its deny list
		// is additive rather than a replacement. A profile that names either
		// one keeps today's full-replacement semantics: once an operator
		// writes any allow or ask rule for a profile, that list IS the
		// intended ruleset and base rules are not silently mixed in. The
		// alternative — copying the base allow/ask lists into the default
		// "remote" profile in config.Default() — would duplicate two lists
		// that will drift apart the moment one of them changes.
		if len(allow) == 0 && len(ask) == 0 {
			allow, ask = cfg.Allow, cfg.Ask
		}
		// Learned allow/ask rules are earned in one trust context and do not
		// carry into another: an "always allow" answered at the terminal must
		// not silently extend to the Telegram bridge. Learned DENY is global,
		// because deny is absolute and only ever additive.
		rs, err := buildRuleset(def, allow, ask, deny, config.LearnedPolicy{Deny: cfg.Learned.Deny})
		if err != nil {
			return nil, fmt.Errorf("policy.profile.%s: %w", name, err)
		}
		e.profiles[Profile(name)] = rs
	}
	return e, nil
}

func buildRuleset(def string, allow, ask, deny []string, learned config.LearnedPolicy) (ruleset, error) {
	var rs ruleset
	configDeny, err := parseAll(DecisionDeny, deny)
	if err != nil {
		return ruleset{}, err
	}
	baseline := config.BaselineDeny()
	for i := range configDeny {
		configDeny[i].Source = SourceConfig
		if slices.Contains(baseline, configDeny[i].Raw) {
			configDeny[i].Source = SourceBaseline
		}
	}
	learnedDeny, err := parseAll(DecisionDeny, learned.Deny)
	if err != nil {
		return ruleset{}, err
	}
	rs.deny = append(configDeny, tagged(learnedDeny, SourceLearned)...)

	allowRules, err := parseAll(DecisionAllow, allow)
	if err != nil {
		return ruleset{}, err
	}
	askRules, err := parseAll(DecisionAsk, ask)
	if err != nil {
		return ruleset{}, err
	}
	learnedAllow, err := parseAll(DecisionAllow, learned.Allow)
	if err != nil {
		return ruleset{}, err
	}
	learnedAsk, err := parseAll(DecisionAsk, learned.Ask)
	if err != nil {
		return ruleset{}, err
	}
	// Source plays no part in a decision: Evaluate ranks by tier, and ask
	// wins within a tier. The list is kept in tier order so the policy view
	// reads in the order decisions are made.
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(allowRules, SourceConfig)...)
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(askRules, SourceConfig)...)
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(learnedAllow, SourceLearned)...)
	rs.allowAndAsk = append(rs.allowAndAsk, tagged(learnedAsk, SourceLearned)...)
	slices.SortStableFunc(rs.allowAndAsk, func(a, b Rule) int { return a.tier - b.tier })

	switch def {
	case "allow":
		rs.fallback = DecisionAllow
	case "deny":
		rs.fallback = DecisionDeny
	default:
		rs.fallback = DecisionAsk
	}
	return rs, nil
}

func tagged(rs []Rule, source string) []Rule {
	for i := range rs {
		rs[i].Source = source
	}
	return rs
}

func (e *Engine) Workspace() string              { return e.env.Workspace }
func (e *Engine) ApprovalTimeout() time.Duration { return e.timeout }

// ruleset is the rules a session on profile p is evaluated against: its own,
// or the base set when no profile of that name is configured. Evaluate makes
// the same choice.
func (e *Engine) ruleset(p Profile) ruleset {
	if rs, ok := e.profiles[p]; ok {
		return rs
	}
	return e.base
}

// Profiles lists the profiles the policy view shows: local first, because it
// is the operator's own, then every configured profile by name.
func (e *Engine) Profiles() []Profile {
	out := []Profile{ProfileLocal}
	names := make([]string, 0, len(e.profiles))
	for p := range e.profiles {
		if p != ProfileLocal {
			names = append(names, string(p))
		}
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, Profile(n))
	}
	return out
}

// RuleRow is one line of the policy view.
type RuleRow struct {
	Profile  Profile
	Decision Decision
	Rule     string
	Source   string
}

// Rules lists every profile's rules in the order Evaluate tries them, with
// the profile's default decision as its last row.
func (e *Engine) Rules() []RuleRow {
	var out []RuleRow
	for _, p := range e.Profiles() {
		rs := e.ruleset(p)
		for _, r := range rs.deny {
			out = append(out, RuleRow{Profile: p, Decision: r.Decision, Rule: r.Raw, Source: r.Source})
		}
		for _, r := range rs.allowAndAsk {
			out = append(out, RuleRow{Profile: p, Decision: r.Decision, Rule: r.Raw, Source: r.Source})
		}
		out = append(out, RuleRow{Profile: p, Decision: rs.fallback, Rule: "(default)", Source: SourceConfig})
	}
	return out
}

// ToolDecision is what a call to tool with no arguments gets under profile
// p, and whether an allow or ask rule with an argument predicate names the
// tool, so that a call with arguments could be decided differently. Deny
// rules with predicates are not counted: they are bounds that hold for every
// call, and the policy view lists them.
func (e *Engine) ToolDecision(p Profile, tool string) (Result, bool) {
	res := e.Evaluate(Session{Profile: p}, Call{Tool: tool, Args: json.RawMessage(`{}`)})
	name := normaliseToolName(tool)
	for _, r := range e.ruleset(p).allowAndAsk {
		if r.pred != nil && r.tool.MatchString(name) {
			return res, true
		}
	}
	return res, false
}

// Evaluate resolves one call for one session. Deny rules are checked first
// and win outright; then the narrowest matching tier of allow and ask rules, ask winning ties.
// Path predicates are evaluated against the CALLING session's workspace, so one daemon
// serving a local session in a project and a bridge session in its own directory applies
// the right bound to each.
func (e *Engine) Evaluate(s Session, c Call) Result {
	env := e.env
	if s.Workspace != "" {
		env.Workspace = s.Workspace
	}
	// Arguments a predicate cannot inspect are refused outright rather than
	// matched against tool-name-only rules. This gate is load-bearing
	// security, not decoration: Rule.Match returns false for every
	// argument predicate when the arguments do not decode, so without it a
	// call carrying junk would slip past the deny rules that inspect
	// arguments. Every tool takes an object, so a valid-JSON payload that
	// is not an object is refused for the same reason.
	// Note the nil check: JSON "null" unmarshals into a map without error
	// and leaves it nil, so err alone would let it through.
	var argObj map[string]json.RawMessage
	if err := json.Unmarshal(c.Args, &argObj); err != nil || argObj == nil {
		return Result{Decision: DecisionDeny, Rule: "policy.malformed-arguments"}
	}
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
