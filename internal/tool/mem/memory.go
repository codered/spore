package mem

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/tool"
)

// FactIndexer is the slice of the store this tool needs. Facts are file-owned,
// so writing one is two steps -- the file, then the index -- and the tool is
// where they are sequenced.
type FactIndexer interface {
	IndexFact(ctx context.Context, name, text string) error
	UnindexFact(ctx context.Context, name string) error
}

type memoryTool struct {
	cache *memory.Cache
	idx   FactIndexer
}

// NewMemory builds the fact-writing tool. It is `ask` in the default policy
// and denied under the remote profile: a fact written once shapes every later
// turn in every session, so a human sees each one before it lands.
func NewMemory(cache *memory.Cache, idx FactIndexer) tool.Tool {
	return memoryTool{cache: cache, idx: idx}
}

func (memoryTool) Name() string { return "memory" }

func (memoryTool) Description() string {
	return "Write or delete a memory fact: a short markdown file about the user, the project, " +
		"or how they want you to work, loaded into every future conversation. " +
		"Write a fact when the user tells you something worth remembering beyond this session."
}

func (memoryTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "op": {"type": "string", "enum": ["write", "delete"]},
	    "name": {"type": "string", "description": "Lowercase kebab-case identifier, e.g. prefers-tabs."},
	    "description": {"type": "string", "description": "One line saying what the fact covers. Required for write."},
	    "type": {"type": "string", "enum": ["user", "feedback", "project", "reference"]},
	    "body": {"type": "string", "description": "The fact itself, in markdown. Required for write."}
	  },
	  "required": ["op", "name"]
	}`)
}

// ReadOnly is false: this writes files, so the loop must not dispatch it
// alongside other calls.
func (memoryTool) ReadOnly() bool { return false }

func (t memoryTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Op          string `json:"op"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Body        string `json:"body"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	dir := t.cache.Dir()

	switch a.Op {
	case "write":
		f := memory.Fact{Name: a.Name, Description: a.Description, Type: a.Type, Body: a.Body}
		if err := memory.Write(dir, f); err != nil {
			return "", err
		}
		// Reload before indexing: the cache is what the next turn assembles
		// from, and a fact the model cannot see is worse than one it cannot
		// search.
		t.reload()
		// The description is indexed with the body so a search for what a fact
		// is about finds it even when the body words differ.
		if err := t.idx.IndexFact(ctx, f.Name, f.Description+"\n"+f.Body); err != nil {
			return "", fmt.Errorf("fact %q was written but could not be indexed: %w", f.Name, err)
		}
		return fmt.Sprintf("wrote fact %q", f.Name), nil

	case "delete":
		if err := memory.Delete(dir, a.Name); err != nil {
			return "", err
		}
		t.reload()
		if err := t.idx.UnindexFact(ctx, a.Name); err != nil {
			return "", fmt.Errorf("fact %q was deleted but could not be unindexed: %w", a.Name, err)
		}
		return fmt.Sprintf("deleted fact %q", a.Name), nil

	default:
		return "", fmt.Errorf("op must be write or delete, got %q", a.Op)
	}
}

// reload refreshes the fact set the next turn will assemble. Parse errors are
// not returned to the model: the write it just made succeeded, and someone
// else's malformed file is not this call's failure.
func (t memoryTool) reload() { t.cache.Reload() }
