// Package router picks the model ref for a call site from ordered config rules.
package router

import (
	"fmt"
	"regexp"
	"slices"
	"sync"

	"github.com/codered/spore/internal/config"
)

// The fixed set of call sites. Every LLM call in spore names one of these;
// there is deliberately no "embed" site — embeddings are computed by the
// recall backend, not routed here.
const (
	SiteChat       = "chat"
	SiteCompaction = "compaction"
	SiteTitle      = "title"
	SiteClassify   = "classify"
	// SiteSubagent is a sub-agent's own turns. It is a site of its own so
	// delegated work can be routed to a cheaper model in configuration
	// alone, with no model selection in the sub-agent path.
	SiteSubagent = "subagent"
	// SiteRefinement is the reviewer pass that proposes memory and
	// project-note edits. A site of its own so it can run on a cheaper model.
	SiteRefinement = "refinement"
)

func ValidSite(s string) bool {
	switch s {
	case SiteChat, SiteCompaction, SiteTitle, SiteClassify, SiteSubagent, SiteRefinement:
		return true
	}
	return false
}

// Sites is every call site, in the order /model shows them.
var Sites = []string{SiteChat, SiteCompaction, SiteTitle, SiteClassify, SiteRefinement, SiteSubagent}

// GlobalSites are the call sites /model sets for the whole daemon. chat and
// subagent are chosen per session instead, so they are never overridden here.
var GlobalSites = []string{SiteCompaction, SiteTitle, SiteClassify, SiteRefinement}

// IsGlobalSite reports whether /model sets site daemon-wide.
func IsGlobalSite(site string) bool { return slices.Contains(GlobalSites, site) }

type rule struct {
	re    *regexp.Regexp
	model string
}

// Router is shared by every caller that picks a model (agent, compaction,
// title, refinement), so an override set by /model reaches all of them
// without anything being rebuilt. The mutex is why it is safe to change
// while turns are running.
type Router struct {
	mu           sync.RWMutex
	rules        []rule
	defaultModel string
	overrides    map[string]string
}

// New compiles each rule's When as an anchored regexp, so "chat" matches the
// call site "chat" but not "chatty".
func New(routes []config.Route, defaultModel string) (*Router, error) {
	r := &Router{defaultModel: defaultModel, overrides: map[string]string{}}
	for i, rt := range routes {
		re, err := regexp.Compile(`\A(?:` + rt.When + `)\z`)
		if err != nil {
			return nil, fmt.Errorf("route %d: invalid pattern %q: %w", i, rt.When, err)
		}
		r.rules = append(r.rules, rule{re: re, model: rt.Model})
	}
	return r, nil
}

// Model returns the model ref for a call site: a /model override, else the
// first matching rule, else the configured default.
func (r *Router) Model(callSite string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ref, ok := r.overrides[callSite]; ok {
		return ref
	}
	return r.ruleModel(callSite)
}

// Default is Model without overrides: what config.toml alone says. /model
// marks it with "*", and choosing it clears the override.
func (r *Router) Default(callSite string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ruleModel(callSite)
}

func (r *Router) ruleModel(callSite string) string {
	for _, rule := range r.rules {
		if rule.re.MatchString(callSite) {
			return rule.model
		}
	}
	return r.defaultModel
}

// SetOverride makes ref the model for a daemon-wide site; "" removes the
// override. Per-session sites are refused: their choice lives on the session.
func (r *Router) SetOverride(site, ref string) error {
	if !IsGlobalSite(site) {
		return fmt.Errorf("%q is not a call site /model sets daemon-wide", site)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref == "" {
		delete(r.overrides, site)
		return nil
	}
	r.overrides[site] = ref
	return nil
}

// RuleMatches reports whether any hand-written rule matches site. Startup
// uses it to warn that a subagent rule no longer selects anything.
func (r *Router) RuleMatches(site string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if rule.re.MatchString(site) {
			return true
		}
	}
	return false
}
