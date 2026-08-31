package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPidFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")
	if _, err := ReadPidFile(path); err == nil {
		t.Error("reading a missing pidfile succeeded")
	}
	if err := WritePidFile(path); err != nil {
		t.Fatalf("WritePidFile: %v", err)
	}
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want this process %d", pid, os.Getpid())
	}
	if !PidAlive(pid) {
		t.Error("PidAlive says this very process is dead")
	}
	if err := RemovePidFile(path); err != nil {
		t.Fatalf("RemovePidFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("RemovePidFile left the file behind")
	}
	// Removing a file that is already gone is not an error: shutdown paths
	// call it more than once.
	if err := RemovePidFile(path); err != nil {
		t.Errorf("second RemovePidFile: %v", err)
	}
}

func TestAStalePidFileIsNotALiveDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")
	// A pid that cannot be running: the kernel reserves 0, and pid 1 is not
	// ours to signal, so use a very high one that no test machine will have.
	if err := os.WriteFile(path, []byte("4194303\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if PidAlive(pid) {
		t.Skip("pid 4194303 happens to exist on this machine")
	}
	// This is the case that matters: a crashed daemon leaves its pidfile, and
	// treating that as "already running" would make the CLI refuse to start
	// forever.
	if _, err := ReadPidFile(filepath.Join(t.TempDir(), "absent.pid")); err == nil {
		t.Error("a missing pidfile read as present")
	}
}

func TestReadPidFileRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")
	if err := os.WriteFile(path, []byte("not a pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPidFile(path); err == nil {
		t.Error("ReadPidFile accepted a file that is not a number")
	}
}
