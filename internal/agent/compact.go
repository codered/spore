package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

const compactionPrompt = `Summarise the conversation below for your own future reference.
Keep decisions, file paths, names, numbers and open questions. Drop pleasantries.
Write at most 300 words of plain prose, no preamble.`

// MaybeCompact summarises the older part of a session once its assembled size
// passes context.compact_at of context.max_tokens. Original messages are never
// deleted: only the summary boundary moves, and Snapshot skips rows at or
// below it.
func (a *Agent) MaybeCompact(ctx context.Context, sessionID string) error {
	snap, err := a.Snapshot(ctx, sessionID)
	if err != nil {
		return err
	}
	budget := int(float64(a.Cfg.Context.MaxTokens) * a.Cfg.Context.CompactAt)
	if SnapshotTokens(snap) <= budget {
		return nil
	}

	rows, err := a.Store.Messages(ctx, sessionID)
	if err != nil {
		return err
	}
	_, through, err := a.Store.Summary(ctx, sessionID)
	if err != nil {
		return err
	}

	// live rows are those not already folded into the summary; they line up
	// one-for-one with snap.Messages, which Snapshot built the same way.
	var live []store.Message
	for _, r := range rows {
		if r.Seq > through {
			live = append(live, r)
		}
	}
	if len(live) <= a.Cfg.Context.KeepRecent {
		return nil // nothing outside the protected window
	}
	foldCount := len(live) - a.Cfg.Context.KeepRecent
	cut := live[foldCount-1].Seq
	pending := snap.Messages[:foldCount]

	var transcript strings.Builder
	if snap.Summary != "" {
		transcript.WriteString("Summary so far:\n")
		transcript.WriteString(snap.Summary)
		transcript.WriteString("\n\n")
	}
	for _, m := range pending {
		transcript.WriteString(string(m.Role))
		transcript.WriteString(": ")
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockText:
				transcript.WriteString(b.Text)
			case provider.BlockToolUse:
				fmt.Fprintf(&transcript, "[called %s]", b.Name)
			case provider.BlockToolResult:
				fmt.Fprintf(&transcript, "[tool result: %d bytes]", len(b.Content))
			}
		}
		transcript.WriteString("\n")
	}

	ref := a.Router.Model(router.SiteCompaction)
	p, model, price, err := a.Registry.Resolve(ref)
	if err != nil {
		return err
	}
	_, span := sporetrace.StartLLM(ctx, router.SiteCompaction, ref)
	ch, err := p.Stream(ctx, provider.Request{
		Model:     model,
		System:    compactionPrompt,
		MaxTokens: 1024,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: transcript.String()}},
		}},
	})
	if err != nil {
		span.RecordError(err)
		span.End()
		return fmt.Errorf("compaction provider %s: %w", ref, err)
	}

	var summary string
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			summary += ev.Text
		case provider.EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			span.RecordError(ev.Err)
			span.End()
			return ev.Err
		}
	}
	if strings.TrimSpace(summary) == "" {
		err := fmt.Errorf("compaction produced an empty summary")
		span.RecordError(err)
		span.End()
		return err
	}
	sporetrace.EndLLM(span, transcript.String(), summary, usage, price.Cost(usage))

	return a.Store.SetSummary(ctx, sessionID, summary, cut)
}
