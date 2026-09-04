package workspace

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootAllocatesWhenNothingRequested(t *testing.T) {
	got, err := Root(Request{Ceiling: "/home/user"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("root = %q, want \"\" (allocate a session directory)", got)
	}
}

func TestRootAcceptsADirectoryInsideTheCeiling(t *testing.T) {
	ceiling := t.TempDir()
	inside := filepath.Join(ceiling, "project")
	got, err := Root(Request{Requested: inside, Ceiling: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	if got != inside {
		t.Fatalf("root = %q, want %q", got, inside)
	}
}

// The ceiling is the whole point: a session rooted outside it is refused at
// creation rather than quietly moved.
func TestRootRefusesOutsideTheCeiling(t *testing.T) {
	ceiling := t.TempDir()
	other := t.TempDir()
	_, err := Root(Request{Requested: other, Ceiling: ceiling})
	if err == nil {
		t.Fatal("a root outside the ceiling must be refused")
	}
	if !strings.Contains(err.Error(), ceiling) {
		t.Fatalf("error %q should name the ceiling %q", err, ceiling)
	}
}

func TestRootRefusesARelativeRequest(t *testing.T) {
	if _, err := Root(Request{Requested: "project", Ceiling: t.TempDir()}); err == nil {
		t.Fatal("a relative workspace must be refused: there is no cwd to resolve it against")
	}
}

// A remote creator cannot name a directory at all. Left alone it is confined
// to its own session directory, which holds nothing but its own transcript.
func TestRemoteIgnoresTheRequestAndIsConfined(t *testing.T) {
	ceiling := t.TempDir()
	got, err := Root(Request{Requested: filepath.Join(ceiling, "secrets"), Ceiling: ceiling, Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("root = %q, want \"\" (a remote session gets its own directory)", got)
	}
}

// Setting [policy.remote] workspace is the operator saying a bridge user is
// meant to work on something real.
func TestRemoteRootIsUsedWhenConfigured(t *testing.T) {
	ceiling := t.TempDir()
	shared := filepath.Join(ceiling, "shared")
	got, err := Root(Request{Ceiling: ceiling, RemoteRoot: shared, Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != shared {
		t.Fatalf("root = %q, want %q", got, shared)
	}
}

func TestAllocated(t *testing.T) {
	sessions := "/data/sessions"
	if !Allocated(sessions, "/data/sessions/abc123") {
		t.Fatal("a directory under the sessions directory is spore's own")
	}
	if Allocated(sessions, "/home/user/project") {
		t.Fatal("a user directory is not spore's to create")
	}
}
