# Container for bench/scenarios: the four agents, spore, and a copy of this
# repository at bench/agents/bench.py's WORKSPACE_REF, and nothing of the
# host's. Outside a container, a model went looking in the real checkout.
#
# Build context (assembled by hand; paths are those of a default install):
#   agents/opencode      ~/.opencode/bin/opencode
#   agents/pi-node       ~/.local/share/pi-node
#   agents/prime-agent   ~/.local/share/prime-agent
#   agents/kernel-venv   ~/.prime/agent/kernel-venv   (only this, never auth.json)
#   agents/uv-python     ~/.local/share/uv/python/cpython-3.11*
#   spore                make build
#   snapshot/            git archive <WORKSPACE_REF>
#   bench/               bench/agents/*.py and bench/scenarios/*.py
#
# Run:  docker run --rm --env-file <file with ANTHROPIC_API_KEY> -e SCEN_OUT=/out/results.jsonl \
#         -v <dir>:/out spore-scenarios python3 /bench/scenarios/scenario_b.py 1 3
# Isolation for the scenario benchmarks: the agents can reach the API and a
# copy of the repo, and nothing of the host's (no real repo, no real home).
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates bash coreutils curl git make python3 \
    && rm -rf /var/lib/apt/lists/*
RUN userdel -r ubuntu 2>/dev/null; useradd -m -u 1000 -d /home/code -s /bin/bash code
COPY --chown=code:code agents/opencode /home/code/.opencode/bin/opencode
COPY --chown=code:code agents/pi-node /home/code/.local/share/pi-node
COPY --chown=code:code agents/prime-agent /home/code/.local/share/prime-agent
COPY --chown=code:code agents/uv-python /home/code/.local/share/uv/python
COPY --chown=code:code agents/kernel-venv /home/code/.prime/agent/kernel-venv
COPY spore /usr/local/bin/spore
COPY snapshot /snapshot
COPY bench /bench
ENV PATH=/home/code/.local/share/pi-node/node-v22.22.3-linux-x64/bin:/home/code/.opencode/bin:/home/code/.local/share/prime-agent/bin:/usr/local/bin:/usr/bin:/bin \
    BENCH_SNAPSHOT=/snapshot SPORE_BIN=/usr/local/bin/spore PRIME_AGENT=/home/code/.local/share/prime-agent/bin/prime-agent
USER code
WORKDIR /home/code
