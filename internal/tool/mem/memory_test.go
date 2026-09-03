package mem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/memory"
)

type fakeIndex struct {
	indexed   map[string]string
	unindexed []string
}

func newFakeIndex() *fakeIndex { return &fakeIndex{indexed: map[string]string{}} }

func (f *fakeIndex) IndexFact(_ context.Context, name, text string) error {
	f.indexed[name] = text
	return nil
}
func (f *fakeIndex) UnindexFact(_ context.Context, name string) error {
	f.unindexed = append(f.unindexed, name)
	return nil
}

func TestMemoryWriteCreatesReloadsAndIndexes(t *testing.T) {
	dir := t.TempDir()
	cache := memory.NewCache(dir)
	cache.Reload()
	idx := newFakeIndex()

	out, err := NewMemory(cache, idx).Call(context.Background(), json.RawMessage(
		`{"op":"write","name":"prefers-tabs","description":"formatting","type":"user","body":"Tabs."}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("result does not name the fact: %q", out)
	}
	facts := cache.Facts()
	if len(facts) != 1 || facts[0].Body != "Tabs." {
		t.Fatalf("cache not reloaded after the write: %+v", facts)
	}
	if got := idx.indexed["prefers-tabs"]; !strings.Contains(got, "Tabs.") {
		t.Fatalf("fact not indexed: %q", got)
	}
	if !strings.Contains(idx.indexed["prefers-tabs"], "formatting") {
		t.Fatal("the description should be searchable alongside the body")
	}
}

func TestMemoryDeleteRemovesReloadsAndUnindexes(t *testing.T) {
	dir := t.TempDir()
	if err := memory.Write(dir, memory.Fact{Name: "old", Description: "d", Type: "user", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	cache := memory.NewCache(dir)
	cache.Reload()
	idx := newFakeIndex()

	if _, err := NewMemory(cache, idx).Call(context.Background(), json.RawMessage(`{"op":"delete","name":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if len(cache.Facts()) != 0 {
		t.Fatal("cache still holds the deleted fact")
	}
	if len(idx.unindexed) != 1 || idx.unindexed[0] != "old" {
		t.Fatalf("fact not removed from the index: %v", idx.unindexed)
	}
}

func TestMemoryRejectsBadInput(t *testing.T) {
	tl := NewMemory(memory.NewCache(t.TempDir()), newFakeIndex())
	for _, args := range []string{
		`{"op":"wibble","name":"x"}`,
		`{"op":"write","name":"../escape","description":"d","type":"user","body":"b"}`,
		`{"op":"write","name":"ok","type":"user","body":"b"}`,
		`{"op":"write","name":"ok","description":"d","type":"nope","body":"b"}`,
		`{"op":"delete"}`,
		`{"op":"delete","name":"missing"}`,
		`{}`,
	} {
		if _, err := tl.Call(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("accepted bad args: %s", args)
		}
	}
}

// A failed write must leave nothing behind for the index to disagree with.
func TestMemoryDoesNotIndexAFailedWrite(t *testing.T) {
	idx := newFakeIndex()
	tl := NewMemory(memory.NewCache(t.TempDir()), idx)
	if _, err := tl.Call(context.Background(), json.RawMessage(
		`{"op":"write","name":"bad name","description":"d","type":"user","body":"b"}`)); err == nil {
		t.Fatal("expected a rejection")
	}
	if len(idx.indexed) != 0 {
		t.Fatalf("indexed a fact that was never written: %v", idx.indexed)
	}
}

func TestMemoryIsNotReadOnly(t *testing.T) {
	if NewMemory(memory.NewCache(t.TempDir()), newFakeIndex()).ReadOnly() {
		t.Fatal("memory writes files and must not be dispatched concurrently as read-only")
	}
}
