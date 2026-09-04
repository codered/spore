package phoenix

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComposePinsTheImageAndBindsLoopback(t *testing.T) {
	body := ComposeFile()

	if !strings.Contains(body, Image) {
		t.Errorf("compose does not pin the image %q:\n%s", Image, body)
	}
	if strings.Contains(body, ":latest") {
		t.Errorf("compose uses a floating tag:\n%s", body)
	}
	// Every published port must be loopback-only. A sidecar on 0.0.0.0 would
	// expose an unauthenticated trace store to the network.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- \"") || !strings.Contains(trimmed, ":") {
			continue
		}
		if strings.Contains(trimmed, "6006") || strings.Contains(trimmed, "4317") {
			if !strings.Contains(trimmed, "127.0.0.1:") {
				t.Errorf("port publishes beyond loopback: %s", trimmed)
			}
		}
	}
}

func TestComposePersistsTheTraceDatabase(t *testing.T) {
	body := ComposeFile()
	for _, want := range []string{"PHOENIX_WORKING_DIR", "/mnt/data", "volumes:"} {
		if !strings.Contains(body, want) {
			t.Errorf("compose is missing %q, so traces would not survive a restart:\n%s", want, body)
		}
	}
}

func TestWriteComposeCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "phoenix")

	path, err := WriteCompose(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "compose.yml") {
		t.Errorf("path = %q, want the compose file inside %q", path, dir)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != ComposeFile() {
		t.Error("what was written is not what ComposeFile returned")
	}
}

// fakeDocker replaces the exec seam and records the argv it was handed.
func fakeDocker(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	original := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.CommandContext(ctx, "true")
	}
	t.Cleanup(func() { execCommand = original })
	return &calls
}

func TestUpRunsComposeUpDetached(t *testing.T) {
	calls := fakeDocker(t)
	dir := t.TempDir()

	if err := Up(context.Background(), dir); err != nil {
		t.Fatal(err)
	}

	if len(*calls) != 1 {
		t.Fatalf("ran %d commands, want 1: %v", len(*calls), *calls)
	}
	argv := strings.Join((*calls)[0], " ")
	for _, want := range []string{"docker compose", "--project-directory " + dir, "up -d"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q is missing %q", argv, want)
		}
	}
}

func TestDownKeepsTheVolumeUnlessAsked(t *testing.T) {
	calls := fakeDocker(t)
	dir := t.TempDir()

	if err := Down(context.Background(), dir, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join((*calls)[0], " "), " -v") {
		t.Errorf("plain teardown destroyed the volume: %v", (*calls)[0])
	}

	if err := Down(context.Background(), dir, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join((*calls)[1], " "), " -v") {
		t.Errorf("purge did not remove the volume: %v", (*calls)[1])
	}
}

func TestHealthURLIsDerivedFromTheTraceEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://localhost:6006/v1/traces": "http://localhost:6006/healthz",
		"http://127.0.0.1:6006/v1/traces": "http://127.0.0.1:6006/healthz",
		"http://box.local:9999/v1/traces": "http://box.local:9999/healthz",
	}
	for in, want := range cases {
		got, err := HealthURL(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("HealthURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := HealthURL("://not a url"); err == nil {
		t.Error("a malformed endpoint should be an error, not a guess")
	}
}

func TestReadyAnswersFromTheHealthEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("polled %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := Ready(context.Background(), srv.URL+"/healthz"); err != nil {
		t.Errorf("a healthy server reported an error: %v", err)
	}
}

func TestReadyFailsWhenNothingIsListening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/healthz"
	srv.Close() // nothing is listening now

	if err := Ready(context.Background(), url); err == nil {
		t.Error("a closed port reported ready")
	}
}

func TestWaitReadyGivesUpAtTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	start := time.Now()
	err := WaitReady(context.Background(), srv.URL+"/healthz", 300*time.Millisecond)
	if err == nil {
		t.Fatal("WaitReady returned success against a server that never became ready")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("WaitReady overran its timeout by too much: %v", time.Since(start))
	}
}

func TestWaitReadyBoundsIndividualAttempts(t *testing.T) {
	// Test that a stalled HTTP response (connection accepted, no response sent)
	// cannot outlive WaitReady's timeout. Without per-attempt context bounding,
	// a single blocked read can hang indefinitely.

	// Create a listener that accepts connections but never sends data.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("create listener: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Accept connections but never respond, just read/discard.
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Just accept and hold the connection open forever.
			// The client's context timeout should interrupt.
			go func() {
				defer conn.Close()
				select {}
			}()
		}
	}()

	healthURL := "http://" + ln.Addr().String() + "/healthz"

	start := time.Now()
	timeout := 100 * time.Millisecond
	err = WaitReady(context.Background(), healthURL, timeout)
	elapsed := time.Since(start)

	ln.Close() // Stop accepting connections
	<-done     // Wait for goroutine to exit

	if err == nil {
		t.Fatal("WaitReady returned success against a server that never responded")
	}
	// The timeout should bound the wait. Allow generous slack for scheduling,
	// but a stalled response should not cause WaitReady to hang for seconds.
	// Without per-attempt context bounding, this could hang for 10+ seconds.
	if elapsed > timeout*5 {
		t.Errorf("WaitReady blocked for %v against a %v timeout; per-attempt context not bounding attempts", elapsed, timeout)
	}
}
