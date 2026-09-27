package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/store"
)

func TestRefineEndToEndIdleApplyThenRollback(t *testing.T) {
	s, ts := newTestServer(t) // chat turns are not needed: messages are seeded
	ref := attachRefiner(t, s, `{"edits":[{"kind":"fact.create","name":"go-test-flags","type":"feedback","description":"how to run tests","body":"Always pass -race.","rationale":"user: always use -race"}]}`)
	ctx := context.Background()
	sid, _ := s.Store().CreateSessionFrom(ctx, "chat", "", store.SourceChat)
	appendUser(t, s, sid, "no — always run go test with -race")

	if err := ref.Sweep(ctx, time.Now().Add(time.Hour), s.TurnRunning); err != nil {
		t.Fatal(err)
	}
	ref.Wait()

	path := filepath.Join(s.cfg.DataDir, "memory", "go-test-flags.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fact not written: %v", err)
	}
	msgs, _ := s.Store().Messages(ctx, sid)
	if msgs[len(msgs)-1].Role != store.RoleNote {
		t.Fatal("no refinement note row")
	}

	var rb RollbackJSON
	if code := post(t, ts.URL+"/api/sessions/"+sid+"/refine/rollback", map[string]string{}, &rb); code != http.StatusOK || len(rb.RolledBack) != 1 {
		t.Fatalf("rollback: %d %+v", code, rb)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fact survived rollback: %v", err)
	}
}
