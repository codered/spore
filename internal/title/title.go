// Package title names a session from its first message, so a sidebar of
// sessions reads as a list of topics rather than a column of "chat".
package title

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	sporetrace "github.com/codered/spore/internal/trace"
)

const (
	// maxRunes bounds a generated title; a sidebar row shows about this much.
	maxRunes = 60
	// fallbackRunes bounds a title cut from the message itself.
	fallbackRunes = 50
	// inputRunes bounds how much of the message the model sees. The opening
	// is what names a conversation; the rest only costs tokens.
	inputRunes = 1000
	// maxTokens leaves room for a reasoning model's <think> block.
	maxTokens = 400
)

const systemPrompt = "You name chat conversations. Reply with a title of two to six words " +
	"that says what the user's message is about. Reply with the title only: " +
	"no quotes, no punctuation at the end, no explanation."

// Titler makes the title call on the router's title site.
type Titler struct {
	Registry *provider.Registry
	Router   *router.Router
}

func New(reg *provider.Registry, rt *router.Router) *Titler {
	return &Titler{Registry: reg, Router: rt}
}

// Title asks the model for a short title for a conversation that opened with
// message. An empty or unusable reply is an error, so the caller can fall
// back to Fallback.
func (t *Titler) Title(ctx context.Context, message string) (string, error) {
	ref := t.Router.Model(router.SiteTitle)
	p, model, price, err := t.Registry.Resolve(ref)
	if err != nil {
		return "", err
	}
	input := shorten(strings.TrimSpace(message), inputRunes)
	_, span := sporetrace.StartLLM(ctx, router.SiteTitle, ref)
	ch, err := p.Stream(ctx, provider.Request{
		Model:     model,
		System:    []provider.Block{{Type: provider.BlockText, Text: systemPrompt}},
		MaxTokens: maxTokens,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: input}},
		}},
	})
	if err != nil {
		span.RecordError(err)
		span.End()
		return "", fmt.Errorf("title provider %s: %w", ref, err)
	}
	var text string
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			span.RecordError(ev.Err)
			span.End()
			return "", ev.Err
		}
	}
	sporetrace.EndLLM(span, input, text, usage, price.Cost(usage))
	out := Clean(text)
	if out == "" {
		return "", errors.New("the title reply was empty")
	}
	return out, nil
}

var (
	thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)
	labelRe    = regexp.MustCompile(`(?i)^title\s*:\s*`)
)

// Clean turns a model's reply into a title: reasoning dropped, first line
// only, no label, markdown, quotes or closing punctuation, and bounded.
func Clean(reply string) string {
	s := thinkBlock.ReplaceAllString(reply, "")
	// An unterminated <think> is a reply cut off mid-reasoning: no title.
	if i := strings.Index(s, "<think>"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s, _, _ = strings.Cut(s, "\n")
	s = strings.TrimLeft(s, "#*_` ")
	s = strings.TrimRight(s, "*_` ")
	s = labelRe.ReplaceAllString(s, "")
	s = strings.Trim(s, "\"'“”‘’` ")
	s = strings.TrimRight(s, ".!?:;, ")
	s = strings.Join(strings.Fields(s), " ")
	return shorten(s, maxRunes)
}

// Fallback is the title used when no model answers: the message's first
// line, bounded.
func Fallback(message string) string {
	s := strings.TrimSpace(message)
	s, _, _ = strings.Cut(s, "\n")
	return shorten(strings.Join(strings.Fields(s), " "), fallbackRunes)
}

// Placeholder reports whether a title is still the one a client gave a new
// session -- "chat" from the terminal, "web" from the browser, nothing from
// Discord -- rather than one that says what the session is about.
func Placeholder(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "chat", "web", "new chat":
		return true
	}
	return false
}

// shorten bounds s to n runes, ending in "…" when it cuts, and backs off to
// a word boundary when there is one.
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	cut := r[:n-1]
	if !unicode.IsSpace(r[n-1]) {
		if i := lastSpace(cut); i > 0 {
			cut = cut[:i]
		}
	}
	return strings.TrimRightFunc(string(cut), unicode.IsSpace) + "…"
}

func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if unicode.IsSpace(r[i]) {
			return i
		}
	}
	return -1
}
