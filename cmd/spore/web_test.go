package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
)

func TestSporeWebPrintsTheURLOnlyToATerminal(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Daemon.Addr = "127.0.0.1:7777"
	tok, err := daemon.LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	var opened string
	open := func(u string) error { opened = u; return nil }

	var out bytes.Buffer
	if err := cmdWebURL(cfg, &out, true, open); err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:7777/?token=" + tok
	if opened != want || !strings.Contains(out.String(), want) {
		t.Errorf("tty: opened %q, printed %q", opened, out.String())
	}

	out.Reset()
	opened = ""
	if err := cmdWebURL(cfg, &out, false, open); err != nil {
		t.Fatal(err)
	}
	if opened != want {
		t.Errorf("no tty: opened %q", opened)
	}
	if strings.Contains(out.String(), tok) {
		t.Errorf("printed the token without a terminal: %q", out.String())
	}
}
