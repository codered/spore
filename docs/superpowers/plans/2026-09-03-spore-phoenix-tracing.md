# Plan 5c — Phoenix tracing sidecar

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the span layer somewhere to send its traces — a pinned Phoenix container provisioned by `spore trace setup`, with `status` and `teardown` to match.

**Architecture:** The instrumentation already exists. `internal/trace` opens turn, LLM, tool and retriever spans with OpenInference attributes, `Init` builds an OTLP/HTTP exporter when `[trace] enabled` is true, and `[trace]` already carries `endpoint`, `sample_rate` and `redact`. Nothing about span emission changes here. What is missing is the other end of the wire: a container to receive them, and the three verbs that start it, report on it and stop it. Phoenix is a single service with no sidecar, which makes this a strictly smaller job than 5b — one image, one volume, one health endpoint.

**Tech Stack:** Go 1.26, Docker Compose, `arizephoenix/phoenix:20.4.0`, the OTel Go SDK already vendored (`otlptracehttp`).

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md` — sections 7 (Tracing), 9 (Configuration), 10 (Testing), 11 (staging, stage 5c).

## Global Constraints

- Go 1.26. Module `github.com/codered/spore`.
- Every test command runs through the repo's tags: `go test -tags sqlite_fts5 ./...`, or `make test`. `make fmtcheck vet test` must pass before any commit.
- The default suite must not need a network, a container, or Docker. The container-backed test lives behind `-tags phoenix` and is additionally guarded by a `docker` lookup, so it skips rather than fails on a machine without it.
- Pinned image, exact tag, never `latest`: `arizephoenix/phoenix:20.4.0`. It is deliberately not the newest release — 20.7.0 shipped the day this plan was written, and a same-day pin is how a working machine breaks on the next pull.
- The service binds `127.0.0.1` only. The daemon binds loopback and carries no auth (spec section 8); a sidecar must not be the thing that opens the machine up.
- `spore trace setup` leaves `redact = false`, which is the shipped default. Full prompt and completion text reaches Phoenix. **This includes messages that arrived over a bridge** — a Discord user's text lands in the container's volume. Task 3 says so on stdout at setup time, and Task 4 says so in the README; neither is optional.
- Provisioning never edits the operator's config beyond the single line it owns. `[trace] enabled` is the only key these verbs write.

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/write.go` (modify) | Gains `setSectionKey`, a shared one-line TOML rewriter, and `SetTraceEnabled`. `SetRecallBackend` is reimplemented on the shared helper with its behaviour unchanged. |
| `internal/config/write_test.go` (modify) | Tests for the new writer and the key-matching edge it fixes. |
| `internal/trace/phoenix/provision.go` (create) | The compose file, the pinned image, the docker verbs, and the readiness poll. Everything that does not need a running container. |
| `internal/trace/phoenix/provision_test.go` (create) | Offline tests for all of the above, using the `execCommand` seam and `httptest`. |
| `internal/trace/phoenix/integration_test.go` (create) | Behind `-tags phoenix`. Asserts a real Phoenix accepts a real export. |
| `cmd/spore/trace.go` (create) | `cmdTrace` and the three verbs. |
| `cmd/spore/trace_test.go` (create) | Verb-level tests against a temp config and a stub docker. |
| `cmd/spore/main.go` (modify) | One dispatch case, five usage lines. |
| `Makefile` (modify) | `test-phoenix`. |
| `README.md` (modify) | The tracing section. |

---

### Task 1: A config writer for `[trace] enabled`

`spore recall setup` already rewrites one key in one section (`SetRecallBackend`), and `trace setup` needs exactly the same thing for a different key. Rather than a second copy of the line-walking loop, extract the shared part. The extraction also fixes a real bug in the existing matcher: it tests `strings.HasPrefix(trimmed, "backend")`, so a key named `backend_url` would be overwritten by a value meant for `backend`.

**Files:**
- Modify: `internal/config/write.go` (the `SetRecallBackend` function at the end of the file)
- Test: `internal/config/write_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func SetTraceEnabled(path string, enabled bool) error` — used by Task 3.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/write_test.go`:

```go
func TestSetTraceEnabledAddsSectionWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("default_model = \"p/m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, true); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	if !cfg.Trace.Enabled {
		t.Error("trace.enabled was not turned on")
	}
}

func TestSetTraceEnabledReplacesExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[trace]\nenabled = true\nsample_rate = 0.5\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, false); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trace.Enabled {
		t.Error("trace.enabled was not turned off")
	}
	// The rewrite owns one line and must not disturb its neighbours.
	if cfg.Trace.SampleRate != 0.5 {
		t.Errorf("sample_rate = %v, want 0.5 preserved", cfg.Trace.SampleRate)
	}
}

func TestSetTraceEnabledPreservesTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "# a comment the operator wrote\ndefault_model = \"p/m\"\n\n[recall]\nbackend = \"weaviate\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, true); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# a comment the operator wrote", "[recall]", "backend = \"weaviate\""} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rewrite lost %q:\n%s", want, out)
		}
	}
}

func TestSetSectionKeyLeavesSimilarlyNamedKeysAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[trace]\nenabled_at = \"never\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, true); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "enabled_at = \"never\"") {
		t.Errorf("a key that merely starts with the target name was overwritten:\n%s", out)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Trace.Enabled {
		t.Errorf("trace.enabled was not set:\n%s", out)
	}
}
```

`write_test.go` already imports `os`, `path/filepath`, `strings` and `testing`, so the import block needs no change.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run 'TestSetTrace|TestSetSectionKey' -v`
Expected: FAIL to build — `undefined: SetTraceEnabled`.

- [ ] **Step 3: Implement**

In `internal/config/write.go`, add `"strconv"` to the imports, then replace the whole `SetRecallBackend` function at the end of the file with:

```go
// assignsKey reports whether a trimmed config line assigns to key. Matching a
// bare prefix is not enough: "enabled_at" starts with "enabled", and
// overwriting the wrong line would silently change a setting the operator did
// not ask spore to touch.
func assignsKey(trimmed, key string) bool {
	if !strings.HasPrefix(trimmed, key) {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(trimmed[len(key):]), "=")
}

// setSectionKey rewrites one key inside one section of a TOML file, adding
// the key or the whole section when either is missing. It edits a single line
// and leaves every other byte alone, comments included: a setup verb has no
// business reformatting a file the operator maintains by hand. literal is
// written as-is, so callers pass an already-quoted string or a bare number or
// bool.
//
// It rewrites an existing section rather than appending a second one of the
// same name: duplicate sections make the file fail to load, which would turn
// a successful setup into a broken install.
func setSectionKey(path, section, key, literal string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(body), "\n")
	header := "[" + section + "]"
	setting := key + " = " + literal

	inSection, replaced, sectionAt := false, false, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inSection = trimmed == header
			if inSection {
				sectionAt = i
			}
			continue
		}
		if inSection && assignsKey(trimmed, key) {
			lines[i] = setting
			replaced = true
		}
	}
	switch {
	case replaced:
	case sectionAt >= 0:
		rest := append([]string{setting}, lines[sectionAt+1:]...)
		lines = append(lines[:sectionAt+1], rest...)
	default:
		lines = append(lines, "", header, setting, "")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// SetRecallBackend records the backend `spore recall setup` provisioned.
func SetRecallBackend(path, backend string) error {
	learnMu.Lock()
	defer learnMu.Unlock()
	switch backend {
	case RecallSQLiteFTS, RecallWeaviate:
	default:
		return fmt.Errorf("recall backend must be %s or %s, got %q", RecallSQLiteFTS, RecallWeaviate, backend)
	}
	return setSectionKey(path, "recall", "backend", strconv.Quote(backend))
}

// SetTraceEnabled records whether `spore trace setup` left tracing on. It is
// the only key the trace verbs write: endpoint, sample_rate and redact stay
// the operator's.
func SetTraceEnabled(path string, enabled bool) error {
	learnMu.Lock()
	defer learnMu.Unlock()
	return setSectionKey(path, "trace", "enabled", strconv.FormatBool(enabled))
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v`
Expected: PASS, including every pre-existing `SetRecallBackend` test. Those tests are the check that the extraction changed no behaviour — if one fails, the extraction is wrong, not the test.

- [ ] **Step 5: Commit**

```bash
git add internal/config/write.go internal/config/write_test.go
git commit -m "feat(config): add the [trace] enabled write-back"
```

---

### Task 2: Provisioning Phoenix

Everything a network cannot decide: the compose file, the pinned image, the docker verbs and the readiness poll. Phoenix is one service — no sidecar, because unlike Weaviate it computes no embeddings.

**Files:**
- Create: `internal/trace/phoenix/provision.go`
- Test: `internal/trace/phoenix/provision_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all used by Task 3:
  - `const Image = "arizephoenix/phoenix:20.4.0"`
  - `func ComposeFile() string`
  - `func WriteCompose(dir string) (string, error)`
  - `func DockerAvailable() error`
  - `func Up(ctx context.Context, dir string) error`
  - `func Down(ctx context.Context, dir string, removeVolumes bool) error`
  - `func HealthURL(traceEndpoint string) (string, error)`
  - `func Ready(ctx context.Context, healthURL string) error`
  - `func WaitReady(ctx context.Context, healthURL string, timeout time.Duration) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/trace/phoenix/provision_test.go`:

```go
package phoenix

import (
	"context"
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
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/trace/phoenix/ -v`
Expected: FAIL to build — the package does not exist yet.

- [ ] **Step 3: Implement**

Create `internal/trace/phoenix/provision.go`:

```go
// Package phoenix provisions the Phoenix container that receives spore's
// traces. It owns the compose file and the container lifecycle and nothing
// else: span creation lives in internal/trace, and this package is never
// imported by the agent's hot path.
package phoenix

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Image is pinned exactly. A floating tag turns a working machine into a
// broken one on the next pull.
const Image = "arizephoenix/phoenix:20.4.0"

// execCommand is the seam the provisioning tests replace. Running docker for
// real in a unit test would make the suite depend on the machine.
var execCommand = exec.CommandContext

// ComposeFile is the whole sidecar: one service, because Phoenix computes no
// embeddings and so needs no inference container beside it.
//
// Both ports publish on 127.0.0.1 only. spore's daemon binds loopback and
// carries no authentication, so a sidecar published on every interface would
// be the one thing that exposed the machine -- and this one holds prompts.
func ComposeFile() string {
	return `# Written by "spore trace setup". Running setup again rewrites this file,
# so keep local changes somewhere else.
services:
  phoenix:
    image: ` + Image + `
    restart: unless-stopped
    ports:
      - "127.0.0.1:6006:6006"
      - "127.0.0.1:4317:4317"
    volumes:
      - phoenix_data:/mnt/data
    environment:
      PHOENIX_WORKING_DIR: /mnt/data

volumes:
  phoenix_data:
`
}

// WriteCompose puts the compose file where teardown can find it again.
func WriteCompose(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, []byte(ComposeFile()), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// DockerAvailable is the preflight. Failing here with a plain message beats
// half-provisioning and leaving the operator to work out which step broke.
func DockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not on PATH; install Docker, or point trace.endpoint at a collector you run yourself")
	}
	return nil
}

func compose(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"compose", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yml")}, args...)
	out, err := execCommand(ctx, "docker", full...).CombinedOutput()
	if err != nil {
		// The operator fixes this by hand, so docker's own words are worth
		// more than a wrapped exit status.
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("docker %s: %w: %s", strings.Join(full, " "), err, msg)
		}
		return fmt.Errorf("docker %s: %w", strings.Join(full, " "), err)
	}
	return nil
}

func Up(ctx context.Context, dir string) error { return compose(ctx, dir, "up", "-d") }

// Down stops the service. The volume survives by default: a teardown that
// also destroyed it would make "stop this for now" and "throw the traces
// away" the same command.
func Down(ctx context.Context, dir string, removeVolumes bool) error {
	args := []string{"down"}
	if removeVolumes {
		args = append(args, "-v")
	}
	return compose(ctx, dir, args...)
}

// HealthURL derives the readiness endpoint from the configured trace
// endpoint, so an operator who moved Phoenix to another port is polled where
// they actually put it rather than where the default says.
func HealthURL(traceEndpoint string) (string, error) {
	u, err := url.Parse(traceEndpoint)
	if err != nil {
		return "", fmt.Errorf("trace.endpoint %q is not a URL: %w", traceEndpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("trace.endpoint %q needs a scheme and a host", traceEndpoint)
	}
	u.Path = "/healthz"
	u.RawQuery = ""
	return u.String(), nil
}

// Ready reports whether Phoenix is answering. Any 2xx counts: the endpoint
// exists to say the process is up, and reading more into it than that would
// be inventing a contract Phoenix does not offer.
func Ready(ctx context.Context, healthURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s answered %s", healthURL, resp.Status)
	}
	return nil
}

// WaitReady polls until Phoenix answers or the timeout passes. A first start
// pulls the image, so the caller's timeout is generous; the poll is cheap.
func WaitReady(ctx context.Context, healthURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = Ready(ctx, healthURL)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for phoenix at %s: %w", healthURL, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/trace/... -v`
Expected: PASS, including the pre-existing `internal/trace` tests.

- [ ] **Step 5: Prove the loopback test has teeth**

Temporarily change one published port in `ComposeFile` from `"127.0.0.1:6006:6006"` to `"6006:6006"` and run `go test -tags sqlite_fts5 ./internal/trace/phoenix/ -run TestComposePins -v`. It must FAIL. Put the line back and confirm it passes again. A test that cannot fail is not protecting the loopback rule.

- [ ] **Step 6: Commit**

```bash
git add internal/trace/phoenix/provision.go internal/trace/phoenix/provision_test.go
git commit -m "feat(trace): provision the phoenix container"
```

---

### Task 3: The CLI verbs

`spore trace setup | status | teardown`, mirroring `spore recall` so an operator who has used one already knows the other.

**Files:**
- Create: `cmd/spore/trace.go`
- Modify: `cmd/spore/main.go` (the `switch args[0]` block, and the `usage` constant)
- Test: `cmd/spore/trace_test.go`

**Interfaces:**
- Consumes: `config.SetTraceEnabled` (Task 1); `phoenix.DockerAvailable`, `WriteCompose`, `Up`, `Down`, `HealthURL`, `Ready`, `WaitReady` (Task 2).
- Produces: `func cmdTrace(ctx context.Context, cfg *config.Config, args []string) error`.

- [ ] **Step 1: Write the failing tests**

Create `cmd/spore/trace_test.go`:

```go
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
)

// traceFixture writes a minimal loadable config and returns it. The data dir
// is the temp dir, so the compose file lands somewhere the test owns.
func traceFixture(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "default_model = \"p/m\"\ndata_dir = \"" + dir + "\"\n\n[providers.p]\nkind = \"anthropic\"\napi_key = \"x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestTraceRejectsUnknownVerb(t *testing.T) {
	cfg := traceFixture(t)
	err := cmdTrace(context.Background(), cfg, []string{"frobnicate"})
	if err == nil {
		t.Fatal("an unknown verb was accepted")
	}
	if !strings.Contains(err.Error(), "setup") {
		t.Errorf("error should list the real verbs, got: %v", err)
	}
}

func TestTraceRequiresAVerb(t *testing.T) {
	cfg := traceFixture(t)
	if err := cmdTrace(context.Background(), cfg, nil); err == nil {
		t.Fatal("no verb was accepted")
	}
}

func TestTraceStatusReportsConfiguration(t *testing.T) {
	cfg := traceFixture(t)
	out := captureStdout(t, func() error {
		return cmdTrace(context.Background(), cfg, []string{"status"})
	})
	// Tracing is off in the fixture, and status must say so without needing
	// a container to talk to.
	for _, want := range []string{"enabled", "endpoint", "redact"} {
		if !strings.Contains(out, want) {
			t.Errorf("status did not report %q:\n%s", want, out)
		}
	}
}

func TestTraceTeardownTurnsTracingOffInConfig(t *testing.T) {
	cfg := traceFixture(t)
	if err := config.SetTraceEnabled(cfg.Path, true); err != nil {
		t.Fatal(err)
	}
	// Point the compose dir at a directory with no compose file: docker is
	// never reached because teardown writes config only after a successful
	// Down, so this asserts the failure path leaves config alone.
	err := cmdTrace(context.Background(), cfg, []string{"teardown"})
	if err == nil {
		t.Skip("docker is installed and answered; this test covers the no-container path")
	}
	reloaded, lerr := config.Load(cfg.Path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !reloaded.Trace.Enabled {
		t.Error("a failed teardown turned tracing off anyway; config should only change after the container is really stopped")
	}
}
```

`captureStdout` already exists in this package, in `cmd/spore/recall_test.go:61`, with the signature `func captureStdout(t *testing.T, fn func() error) string` — it fails the test itself if `fn` returns an error. Use it as written above; do **not** define a second one, which would not compile.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run TestTrace -v`
Expected: FAIL to build — `undefined: cmdTrace`.

- [ ] **Step 3: Implement the verbs**

Create `cmd/spore/trace.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/trace/phoenix"
)

// cmdTrace provisions and reports on the trace sidecar. Span creation needs
// none of this: it is configured by [trace] and happens whether or not spore
// started the collector.
func cmdTrace(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore trace setup | status | teardown")
	}
	switch args[0] {
	case "setup":
		return traceSetupCmd(ctx, cfg, args[1:])
	case "status":
		return traceStatusCmd(ctx, cfg)
	case "teardown":
		return traceTeardownCmd(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("unknown trace command %q: want setup, status or teardown", args[0])
	}
}

// phoenixDir is where the compose file lives: next to the database rather
// than in the workspace, because it is spore's state and not the operator's
// project.
func phoenixDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "phoenix")
}

func traceSetupCmd(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("trace setup", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the container to become ready")
	if err := fs.Parse(args); err != nil {
		return err
	}

	health, err := phoenix.HealthURL(cfg.Trace.Endpoint)
	if err != nil {
		return err
	}
	if err := phoenix.DockerAvailable(); err != nil {
		return err
	}
	dir := phoenixDir(cfg)
	path, err := phoenix.WriteCompose(dir)
	if err != nil {
		return err
	}
	fmt.Println("wrote", path)
	fmt.Println("starting phoenix (a first run pulls the image)...")
	if err := phoenix.Up(ctx, dir); err != nil {
		return err
	}

	fmt.Print("waiting for it to answer... ")
	if err := phoenix.WaitReady(ctx, health, *timeout); err != nil {
		fmt.Println("no")
		return err
	}
	fmt.Println("ready")

	if err := config.SetTraceEnabled(cfg.Path, true); err != nil {
		return err
	}
	fmt.Printf("trace.enabled is now true in %s\n", cfg.Path)
	if !cfg.Trace.Redact {
		// Say this where the operator is looking, not only in the README.
		// Turning tracing on sends prompt and completion text to a container
		// on this machine, and some of that text arrived from other people.
		fmt.Println("note: redact is off, so prompts and completions are stored in full,")
		fmt.Println("      including messages that arrived over a bridge. Set redact = true")
		fmt.Println("      under [trace] to keep span shapes and token counts without the text.")
	}
	fmt.Println("restart the daemon to pick it up: spore serve --stop && spore serve")
	fmt.Println("the UI is at http://localhost:6006")
	return nil
}

func traceStatusCmd(ctx context.Context, cfg *config.Config) error {
	fmt.Printf("enabled:     %t\n", cfg.Trace.Enabled)
	fmt.Printf("endpoint:    %s\n", cfg.Trace.Endpoint)
	fmt.Printf("sample_rate: %g\n", cfg.Trace.SampleRate)
	fmt.Printf("redact:      %t\n", cfg.Trace.Redact)

	health, err := phoenix.HealthURL(cfg.Trace.Endpoint)
	if err != nil {
		return err
	}
	// A short deadline: status reports, it does not wait.
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := phoenix.Ready(probe, health); err != nil {
		fmt.Printf("collector:   not answering (%v)\n", err)
		return nil
	}
	fmt.Println("collector:   ready")
	return nil
}

func traceTeardownCmd(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("trace teardown", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the trace data volume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Stop first, write config second. The reverse order would leave tracing
	// switched off in the file while the container kept running.
	if err := phoenix.Down(ctx, phoenixDir(cfg), *purge); err != nil {
		return err
	}
	if err := config.SetTraceEnabled(cfg.Path, false); err != nil {
		return err
	}
	fmt.Println("stopped; trace.enabled is back to false")
	if !*purge {
		fmt.Println("the trace volume was kept; pass --purge to delete it")
	}
	return nil
}
```

- [ ] **Step 4: Wire it into main**

In `cmd/spore/main.go`, add a case immediately after the `case "recall":` arm:

```go
	case "trace":
		return cmdTrace(ctx, cfg, args[1:])
```

And in the `usage` constant, add these lines directly below the `spore recall teardown` line:

```
  spore trace setup            provision the phoenix collector and turn tracing on
  spore trace status           report trace configuration and collector health
  spore trace teardown         stop the collector and turn tracing off
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/spore/trace.go cmd/spore/trace_test.go cmd/spore/main.go
git commit -m "feat(trace): add setup, status and teardown"
```

---

### Task 4: The container test, the Make target, and the README

Mirrors 5b exactly: a suite behind a build tag, guarded by a `docker` lookup so it skips rather than fails.

**Files:**
- Create: `internal/trace/phoenix/integration_test.go`
- Modify: `Makefile`
- Modify: `README.md`

**Interfaces:**
- Consumes: `phoenix.HealthURL`, `phoenix.Ready` (Task 2); `trace.Init` and the existing span helpers in `internal/trace`.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Write the test**

Create `internal/trace/phoenix/integration_test.go`:

```go
//go:build phoenix

package phoenix

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	sporetrace "github.com/codered/spore/internal/trace"
)

// This file asserts against a real Phoenix what a stub cannot tell you: that
// the exporter spore builds is one Phoenix accepts, on the endpoint the
// default config names.
//
// Run it with: make test-phoenix (after `spore trace setup`).
func liveEndpoint(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	endpoint := os.Getenv("SPORE_TRACE_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:6006/v1/traces"
	}
	health, err := HealthURL(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Ready(ctx, health); err != nil {
		t.Skipf("phoenix is not running at %s: %v (run `spore trace setup`)", endpoint, err)
	}
	return endpoint
}

func TestReadyAgainstARealServer(t *testing.T) {
	endpoint := liveEndpoint(t)
	health, err := HealthURL(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := Ready(context.Background(), health); err != nil {
		t.Errorf("a running phoenix reported not ready: %v", err)
	}
}

func TestExportReachesARealServer(t *testing.T) {
	endpoint := liveEndpoint(t)
	ctx := context.Background()

	shutdown, err := sporetrace.Init(ctx, config.TraceConfig{
		Enabled:    true,
		Endpoint:   endpoint,
		SampleRate: 1.0,
	})
	if err != nil {
		t.Fatalf("building the exporter phoenix is meant to accept: %v", err)
	}

	_, turn := sporetrace.StartTurn(ctx, "integration-session", "test")
	turn.End()

	// Shutdown flushes the batcher. An export phoenix rejects surfaces here
	// and nowhere else, which is the whole reason this test exists.
	flush, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := shutdown(flush); err != nil {
		t.Errorf("phoenix rejected the export: %v", err)
	}
}
```

- [ ] **Step 2: Add the Make target**

In `Makefile`, add below the `test-weaviate` target:

```make
# test-phoenix needs a running collector: `spore trace setup` first.
# It is not part of `make test` on purpose -- the default suite must not
# depend on a container.
test-phoenix:
	go test -tags "sqlite_fts5 phoenix" ./internal/trace/... -v
```

And add `test-phoenix` to the `.PHONY` line:

```make
.PHONY: build install test test-weaviate test-phoenix vet fmt fmtcheck
```

- [ ] **Step 3: Confirm the default suite is untouched**

Run: `make fmtcheck vet test`
Expected: PASS with no container running. The `phoenix` tag is absent from the default `TAGS`, so `integration_test.go` is not even compiled.

Then run: `go vet -tags "sqlite_fts5 phoenix" ./internal/trace/...`
Expected: PASS. The tagged file must at least compile, or it will rot unnoticed — this is the check that it does.

- [ ] **Step 4: Run it for real, if you can**

Run: `./spore trace setup && make test-phoenix`
Expected: PASS.

If Docker is not installed on this machine, the suite skips and that is the correct outcome — but it is **not** verification. Say so plainly in the task report rather than reporting a pass: `docs/backlog.md` already tracks that the equivalent 5b suite has never run, and this adds a second. Update that entry to name both suites rather than leaving it describing only `-tags weaviate`.

- [ ] **Step 5: Document it**

In `README.md`, add a tracing section:

```markdown
### Tracing

Off by default. To see turns, LLM calls, tool calls and retrievals as spans:

    spore trace setup

This writes `~/.spore/phoenix/compose.yml`, starts Phoenix on loopback, waits
for it, and sets `trace.enabled = true`. Restart the daemon afterwards. The UI
is at http://localhost:6006.

`spore trace status` reports the configuration and whether the collector is
answering; `spore trace teardown` stops it and turns tracing back off, keeping
the data volume unless you pass `--purge`.

Prompts and completions are recorded in full, **including messages that
arrived over a bridge** — a Discord user's text is stored in the container's
volume along with everything else. Set `redact = true` under `[trace]` to keep
span shapes, token counts and costs while dropping the text.

If you already run a collector, point `trace.endpoint` at it and skip setup
entirely. Export failures never block a turn.
```

- [ ] **Step 6: Commit**

```bash
git add internal/trace/phoenix/integration_test.go Makefile README.md docs/backlog.md
git commit -m "test(trace): exercise the phoenix collector against a real server"
```

---

## Self-review

**Spec coverage.** Section 7's `[trace]` keys — `enabled`, `endpoint`, `sample_rate`, `redact` — all already exist and are read by `trace.Init`; Task 1 writes the one key setup owns and Task 3 reports all four. "`spore trace setup` writes a pinned Phoenix compose file, starts it on localhost and flips the config" — Tasks 2 and 3. "with the same `status`/`teardown` verbs as recall" — Task 3, mirroring `cmd/spore/recall.go` verb for verb, including `--purge` on teardown and `--timeout` on setup. "Export failures are non-fatal and never block a turn" — unchanged behaviour, owned by `trace.Init`'s batcher and asserted by no new test because no new code touches it. Section 10's rule that the container-backed test sits behind a tag — Task 4.

**Not covered, and deliberately.** The span tree itself. Sections 7's tree diagram is already implemented (`StartTurn`, `StartLLM`, `StartTool`, `StartRetriever`, `RecordPolicy`, `EndRetriever`), so this plan adds no instrumentation. If a span attribute is found wrong while testing against a real Phoenix, that is a bug fix against `internal/trace`, not a task here.

**A known gap this plan creates.** `make test-phoenix` is a second suite that has never been executed, for the same reason as `make test-weaviate`: no Docker on the development machine. Task 4 Step 4 requires updating `docs/backlog.md` so the existing entry names both, rather than quietly leaving it describing one.

**Type consistency.** `phoenix.Image` is the pinned string in Task 2 and is asserted in Task 2's own tests only. `HealthURL(traceEndpoint string) (string, error)` takes the *trace* endpoint and returns the *health* URL; every caller in Task 3 and Task 4 passes `cfg.Trace.Endpoint` and receives the health URL, never the reverse. `Ready` and `WaitReady` both take the health URL, not the trace endpoint — the parameter is named `healthURL` in both to make a mix-up read wrong at the call site. `config.SetTraceEnabled(path string, enabled bool) error` is defined in Task 1 and called in Task 3 with `cfg.Path` in both places. `phoenixDir(cfg)` is defined once in Task 3 and used by both setup and teardown, matching `weaviateDir`'s role in `cmd/spore/recall.go`.
