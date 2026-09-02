// Command envprobe is a tiny MCP server used by spore's tests. Its one tool
// reports the environment and working directory the server was started with,
// which is how the tests prove the child gets nothing it was not given.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type probeIn struct{}

type report struct {
	Env []string `json:"env"`
	Cwd string   `json:"cwd"`
}

func main() {
	srv := sdk.NewServer(&sdk.Implementation{Name: "envprobe", Version: "v0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "probe", Description: "report the process environment"},
		func(ctx context.Context, req *sdk.CallToolRequest, in probeIn) (*sdk.CallToolResult, any, error) {
			cwd, _ := os.Getwd()
			body, err := json.Marshal(report{Env: os.Environ(), Cwd: cwd})
			if err != nil {
				return nil, nil, err
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(body)}}}, nil, nil
		})
	if err := srv.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
