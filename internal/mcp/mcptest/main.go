package main

import (
	"context"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text"`
}

func main() {
	mode := os.Getenv("MCP_TEST_MODE")
	if mode == "exit" {
		return
	}
	if mode == "slow" {
		time.Sleep(12 * time.Second)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "stable-m07c-fixture", Version: "1"}, &mcp.ServerOptions{
		Instructions: os.Getenv("MCP_TEST_INSTRUCTIONS"),
	})
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo text"}, func(_ context.Context, _ *mcp.CallToolRequest, input echoInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: input.Text}}}, nil, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}
