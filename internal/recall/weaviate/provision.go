package weaviate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Pinned images. A floating tag turns a working machine into a broken one on
// the next pull, and the collection schema is written against a specific
// module, so both are exact.
const (
	weaviateImage  = "cr.weaviate.io/semitechnologies/weaviate:1.38.5"
	model2vecImage = "cr.weaviate.io/semitechnologies/model2vec-inference:minishlab-potion-base-32M"
)

// execCommand is the seam the provisioning tests replace. Running docker for
// real in a unit test would make the suite depend on the machine.
var execCommand = exec.CommandContext

// ComposeFile is the whole sidecar. Two services: Weaviate, and the inference
// container that computes vectors -- no Weaviate vectorizer runs in-process,
// and the alternative to a second container is an embedding API key.
//
// Both publish on 127.0.0.1 only. spore's daemon binds loopback and carries no
// authentication, so a sidecar published on every interface would be the one
// thing that exposed the machine.
func ComposeFile() string {
	return `# Written by "spore recall setup". Running setup again rewrites this file,
# so keep local changes somewhere else.
services:
  weaviate:
    image: ` + weaviateImage + `
    restart: unless-stopped
    command:
      - --host
      - 0.0.0.0
      - --port
      - "8080"
      - --scheme
      - http
    ports:
      - "127.0.0.1:8080:8080"
      - "127.0.0.1:50051:50051"
    volumes:
      - weaviate_data:/var/lib/weaviate
    environment:
      QUERY_DEFAULTS_LIMIT: 25
      AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED: "true"
      PERSISTENCE_DATA_PATH: /var/lib/weaviate
      DEFAULT_VECTORIZER_MODULE: text2vec-model2vec
      ENABLE_MODULES: text2vec-model2vec
      MODEL2VEC_INFERENCE_API: http://text2vec-model2vec:8080
      CLUSTER_HOSTNAME: node1
    depends_on:
      - text2vec-model2vec

  text2vec-model2vec:
    image: ` + model2vecImage + `
    restart: unless-stopped
    environment:
      ENABLE_CUDA: "0"

volumes:
  weaviate_data:
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
		return fmt.Errorf("docker is not on PATH; install Docker, or set recall.url to an instance you run yourself")
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

// Down stops the services. Volumes survive by default: a teardown that also
// destroyed the index would make "stop this for now" and "throw the data
// away" the same command.
func Down(ctx context.Context, dir string, removeVolumes bool) error {
	args := []string{"down"}
	if removeVolumes {
		args = append(args, "-v")
	}
	return compose(ctx, dir, args...)
}

// WaitReady polls until the instance answers or the timeout passes. A first
// start pulls two images and loads a model, so the caller's timeout is
// generous; the poll itself is cheap.
func WaitReady(ctx context.Context, b *Backend, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = b.Ready(ctx)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for %s: %w", Name, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
