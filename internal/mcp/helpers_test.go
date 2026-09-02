package mcp

import (
	"context"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveInMemory connects a client session to srv over the SDK's in-memory
// transports. It is the real protocol with no subprocess and no network, so
// almost every test in this package can use a genuine server rather than a
// mock of the SDK.
func serveInMemory(t *testing.T, srv *sdk.Server) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := sdk.NewClient(&sdk.Implementation{Name: "spore", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// listTools reads every tool the session's peer offers.
func listTools(t *testing.T, cs *sdk.ClientSession) []*sdk.Tool {
	t.Helper()
	var out []*sdk.Tool
	for tl, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		out = append(out, tl)
	}
	return out
}
