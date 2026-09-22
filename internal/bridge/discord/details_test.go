package discord

import (
	"strconv"
	"strings"
	"testing"
)

func TestDetailsDropsTheOldestTranscriptsRatherThanGrowing(t *testing.T) {
	d := newDetails()
	ids := make([]string, 0, detailsKept+5)
	for i := 0; i < detailsKept+5; i++ {
		id := d.mintID()
		ids = append(ids, id)
		d.put(id, []toolEntry{{Name: "fs_read", Args: strconv.Itoa(i)}})
	}

	// The five oldest are gone; the cap is what stops a long-lived daemon
	// from holding every turn it has ever run.
	for _, id := range ids[:5] {
		if _, ok := d.get(id); ok {
			t.Fatalf("transcript %q outlived the cap", id)
		}
	}
	for _, id := range ids[5:] {
		if _, ok := d.get(id); !ok {
			t.Fatalf("transcript %q was evicted early", id)
		}
	}
}

func TestDetailsUpdatesInPlaceWithoutConsumingTheCap(t *testing.T) {
	// A turn republishes its transcript on every event, so an update must
	// not count as a new entry — otherwise one busy turn evicts every other.
	d := newDetails()
	keep := d.mintID()
	d.put(keep, []toolEntry{{Name: "fs_read"}})
	for i := 0; i < detailsKept-1; i++ {
		d.put(keep, []toolEntry{{Name: "fs_read"}, {Name: "shell_exec"}})
	}
	other := d.mintID()
	d.put(other, []toolEntry{{Name: "grep"}})

	entries, ok := d.get(keep)
	if !ok {
		t.Fatal("a turn evicted itself by updating its own transcript")
	}
	if len(entries) != 2 {
		t.Fatalf("held %d entries, want the latest 2", len(entries))
	}
	if _, ok := d.get(other); !ok {
		t.Fatal("the second turn was lost")
	}
}

func TestRenderDetailsStaysUnderDiscordsMessageLimit(t *testing.T) {
	var entries []toolEntry
	for i := 0; i < 40; i++ {
		entries = append(entries, toolEntry{
			Name:   "fs_read",
			Args:   strings.Repeat("a", 400),
			Result: strings.Repeat("b", 400),
		})
	}
	out := renderDetails(entries)
	if len(out) > messageLimit {
		t.Fatalf("details are %d bytes, over Discord's %d limit", len(out), messageLimit)
	}
	// An interaction gets exactly one response, so the trim keeps the newest
	// calls and says where the rest went.
	if !strings.Contains(out, "spore trace") {
		t.Fatalf("a trimmed transcript does not say where the whole turn lives: %q", out)
	}
}

func TestRenderDetailsOfATurnThatCalledNoTools(t *testing.T) {
	if got := renderDetails(nil); !strings.Contains(got, "no tool calls") {
		t.Fatalf("empty transcript rendered as %q", got)
	}
}
