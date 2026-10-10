// Package companion is spore noticing what the user keeps coming back to,
// and keeping its own notes. This file is the signal vocabulary: what a
// refinement round may report, validated here because the planner is a
// model and nothing it says is trusted until it is checked.
package companion

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/codered/spore/internal/store"
)

// Signal is one sighting of a recurring interest, as the planner reports it.
type Signal struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// MaxSignalsPerRound bounds what one round can add.
const MaxSignalsPerRound = 10

var (
	keyRE = regexp.MustCompile(`^(stock|crypto|topic|person|place|project):[a-z0-9._-]{1,48}$`)
	// Kinds is the closed set a signal may declare.
	Kinds = []string{"asked", "mentioned", "acted"}
)

// Normalize lowercases and trims the key, collapses whitespace in the label,
// and rejects anything outside the vocabulary.
func Normalize(s Signal) (Signal, error) {
	s.Key = strings.ToLower(strings.TrimSpace(s.Key))
	s.Label = strings.Join(strings.Fields(s.Label), " ")
	s.Kind = strings.TrimSpace(s.Kind)
	if !keyRE.MatchString(s.Key) {
		return Signal{}, fmt.Errorf("signal key %q must look like stock:zs or topic:daughter-reading", s.Key)
	}
	if n := len([]rune(s.Label)); n < 1 || n > 80 {
		return Signal{}, fmt.Errorf("signal %s: label must be 1..80 characters, got %d", s.Key, n)
	}
	if !slices.Contains(Kinds, s.Kind) {
		return Signal{}, fmt.Errorf("signal %s: kind %q must be one of %s", s.Key, s.Kind, strings.Join(Kinds, ", "))
	}
	return s, nil
}

// Trusted reports whether a session's signals count. A signal only raises a
// count -- every action still needs the user's yes -- so Discord counts as
// well as chat. Spore's own sessions (jobs, sub-agents, the companion) never
// do: a ZS watch must not count as the user checking ZS.
func Trusted(sess store.Session) bool {
	if sess.ParentID != "" {
		return false
	}
	return sess.Source == store.SourceChat || sess.Source == store.SourceDiscord
}
