// Package router picks the model ref for a call site from ordered config rules.
package router

import (
	"fmt"
	"regexp"

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
)

func ValidSite(s string) bool {
	switch s {
	case SiteChat, SiteCompaction, SiteTitle, SiteClassify:
		return true
	}
	return false
}

type rule struct {
	re    *regexp.Regexp
	model string
}

type Router struct {
	rules        []rule
	defaultModel string
}

// New compiles each rule's When as an anchored regexp, so "chat" matches the
// call site "chat" but not "chatty".
func New(routes []config.Route, defaultModel string) (*Router, error) {
	r := &Router{defaultModel: defaultModel}
	for i, rt := range routes {
		re, err := regexp.Compile(`\A(?:` + rt.When + `)\z`)
		if err != nil {
			return nil, fmt.Errorf("route %d: invalid pattern %q: %w", i, rt.When, err)
		}
		r.rules = append(r.rules, rule{re: re, model: rt.Model})
	}
	return r, nil
}

// Model returns the model ref for a call site: first matching rule, else the
// configured default.
func (r *Router) Model(callSite string) string {
	for _, rule := range r.rules {
		if rule.re.MatchString(callSite) {
			return rule.model
		}
	}
	return r.defaultModel
}
