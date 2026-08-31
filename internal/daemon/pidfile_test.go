package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func TestAcquirePidFileTwiceFromSameProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")

	// First acquire should succeed.
	if err := AcquirePidFile(path); err != nil {
		t.Fatalf("first AcquirePidFile: %v", err)
	}

	// Second acquire from the same process should fail with already-running.
	err := AcquirePidFile(path)
	if err == nil {
		t.Fatal("second AcquirePidFile should have failed")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("expected ErrAlreadyRunning, got %v", err)
	}

	// The pidfile should still name this process.
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pidfile changed to %d, expected %d", pid, os.Getpid())
	}
}

func TestAcquirePidFileOverStalePidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")

	// Write a stale pidfile (a pid that is not running).
	stalePid := 4194303
	if PidAlive(stalePid) {
		t.Skip("stale pid 4194303 happens to exist on this machine")
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(stalePid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Acquiring should succeed and replace the stale file.
	if err := AcquirePidFile(path); err != nil {
		t.Fatalf("AcquirePidFile over stale: %v", err)
	}

	// The pidfile should now name this process.
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pidfile = %d, want %d", pid, os.Getpid())
	}
}

func TestAcquirePidFileOverGarbagePidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")

	// Write a garbage pidfile.
	if err := os.WriteFile(path, []byte("not a pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Acquiring should succeed and replace the garbage file.
	if err := AcquirePidFile(path); err != nil {
		t.Fatalf("AcquirePidFile over garbage: %v", err)
	}

	// The pidfile should now name this process.
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pidfile = %d, want %d", pid, os.Getpid())
	}
}

func TestReleasePidFileRemovesOwnFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")

	// Acquire the file.
	if err := AcquirePidFile(path); err != nil {
		t.Fatalf("AcquirePidFile: %v", err)
	}

	// Release it.
	if err := ReleasePidFile(path); err != nil {
		t.Fatalf("ReleasePidFile: %v", err)
	}

	// File should be gone.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("ReleasePidFile left the file behind")
	}
}

func TestReleasePidFileLeavesOthersPidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")

	// Write a pidfile for a different process.
	otherPid := 4194303
	if err := os.WriteFile(path, []byte(strconv.Itoa(otherPid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Try to release it from this process.
	if err := ReleasePidFile(path); err != nil {
		t.Fatalf("ReleasePidFile: %v", err)
	}

	// File should still be there, unchanged.
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if pid != otherPid {
		t.Errorf("pidfile was modified: was %d, now %d", otherPid, pid)
	}
}
