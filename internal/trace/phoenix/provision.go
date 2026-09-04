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
// Each poll attempt is bounded by the remaining time, so a stalled response
// cannot outlive the overall timeout.
func WaitReady(ctx context.Context, healthURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		// Bound each poll attempt. A connection that is accepted but stalled
		// cannot outlive the overall deadline. Even with timeout <= 0, we
		// execute at least one attempt: context.WithTimeout with non-positive
		// duration creates an already-cancelled context, and Ready returns
		// immediately with the real connection error.
		remaining := time.Until(deadline)
		attemptCtx, cancel := context.WithTimeout(ctx, remaining)
		last = Ready(attemptCtx, healthURL)
		cancel()
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
