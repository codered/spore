package weaviate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComposePinsImagesAndBindsLoopbackOnly(t *testing.T) {
	yml := ComposeFile()
	for _, want := range []string{weaviateImage, model2vecImage} {
		if !strings.Contains(yml, want) {
			t.Errorf("compose file does not pin %q:\n%s", want, yml)
		}
	}
	if strings.Contains(yml, ":latest") {
		t.Error("compose file uses a floating tag")
	}
	// The daemon binds loopback and carries no auth. A sidecar published on
	// 0.0.0.0 would be the thing that opened the machine up.
	for _, line := range strings.Split(yml, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `- "`) {
			continue
		}
		if !strings.Contains(trimmed, "8080:") && !strings.Contains(trimmed, "50051:") {
			continue
		}
		if !strings.Contains(trimmed, "127.0.0.1:") {
			t.Errorf("port publishes beyond loopback: %s", trimmed)
		}
	}
	if !strings.Contains(yml, "MODEL2VEC_INFERENCE_API") {
		t.Error("weaviate is not pointed at the inference service")
	}
	if !strings.Contains(yml, "DEFAULT_VECTORIZER_MODULE: text2vec-model2vec") {
		t.Error("the default vectorizer is not set")
	}
	if !strings.Contains(yml, "ENABLE_MODULES: text2vec-model2vec") {
		t.Error("the vectorizer module is not enabled")
	}
}

func TestWriteComposeCreatesTheFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weaviate")
	path, err := WriteCompose(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != ComposeFile() {
		t.Error("the file on disk is not the compose file")
	}
	if filepath.Base(path) != "compose.yml" {
		t.Errorf("wrote %q, want compose.yml", path)
	}
}

func TestWriteComposeIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weaviate")
	if _, err := WriteCompose(dir); err != nil {
		t.Fatal(err)
	}
	// Re-running setup must not fail on a directory that already exists.
	if _, err := WriteCompose(dir); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
}

// fakeExec captures the command instead of running docker.
func fakeExec(t *testing.T, script string) *string {
	t.Helper()
	joined := new(string)
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*joined = name + " " + strings.Join(args, " ")
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	t.Cleanup(func() { execCommand = exec.CommandContext })
	return joined
}

func TestUpRunsComposeInTheRightDirectory(t *testing.T) {
	joined := fakeExec(t, "exit 0")
	dir := t.TempDir()
	if err := Up(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"compose", "up", "-d", dir} {
		if !strings.Contains(*joined, want) {
			t.Errorf("ran %q, want it to contain %q", *joined, want)
		}
	}
}

func TestDownKeepsDataUnlessAsked(t *testing.T) {
	joined := fakeExec(t, "exit 0")
	if err := Down(context.Background(), t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	// "stop this for now" must not be the same command as "throw the index
	// away", so -v appears only when the caller asked for it.
	if strings.Contains(*joined, " -v") {
		t.Errorf("ran %q, want the data kept by default", *joined)
	}
	if err := Down(context.Background(), t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*joined, " -v") {
		t.Errorf("ran %q, want the volumes removed when asked", *joined)
	}
}

func TestUpReportsWhatFailed(t *testing.T) {
	fakeExec(t, "echo 'no such image' >&2; exit 1")
	err := Up(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("a failing compose up reported success")
	}
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("error %q drops docker's output", err)
	}
}

func TestWaitReadyGivesUp(t *testing.T) {
	b, err := New(unreachable)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := WaitReady(context.Background(), b, 300*time.Millisecond); err == nil {
		t.Fatal("waiting on a dead address succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s, want it bounded by the timeout", elapsed)
	}
}
