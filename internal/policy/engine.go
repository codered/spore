package policy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/codered/spore/internal/config"
)

// Result is one policy decision, carrying the rule that produced it so the
// audit log, the span and the model's error message all name the same thing.
type Result struct {
	Decision Decision
	Rule     string
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
		env:      Env{Workspace: cfg.Workspace},
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
	denyRules, err := parseAll(DecisionDeny, append(append([]string{}, deny...), learned.Deny...))
	if err != nil {
		return ruleset{}, err
	}
	rs.deny = denyRules

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
	// Hand-written rules are evaluated before learned ones, so a rule the
	// user typed always outranks one an approval prompt wrote.
	rs.allowAndAsk = append(rs.allowAndAsk, allowRules...)
	rs.allowAndAsk = append(rs.allowAndAsk, askRules...)
	rs.allowAndAsk = append(rs.allowAndAsk, learnedAllow...)
	rs.allowAndAsk = append(rs.allowAndAsk, learnedAsk...)

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

func (e *Engine) Workspace() string              { return e.env.Workspace }
func (e *Engine) ApprovalTimeout() time.Duration { return e.timeout }

// Evaluate resolves one call. Deny rules are checked first and win outright;
// then allow and ask rules in configured order; then the profile default.
func (e *Engine) Evaluate(profile Profile, c Call) Result {
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
	rs, ok := e.profiles[profile]
	if !ok {
		rs = e.base
	}
	for _, r := range rs.deny {
		if r.Match(c, e.env) {
			return Result{Decision: DecisionDeny, Rule: r.Raw}
		}
	}
	for _, r := range rs.allowAndAsk {
		if r.Match(c, e.env) {
			return Result{Decision: r.Decision, Rule: r.Raw}
		}
	}
	return Result{Decision: rs.fallback, Rule: "policy.default"}
}
