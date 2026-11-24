// Command mcpgo is the bench's toy app on mark3labs/mcp-go
// (github.com/mark3labs/mcp-go), wrapped with the Go SDK's mcp-go adapter
// (pkg/wakeshift/mcpgo), served as mcp-go's README serves a Streamable HTTP
// server (WB_APP=go-mcpgo; bench/app/CONTRACT.md). Shared parts are in
// ../toy.
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wakeshift/server/bench/app/go/toy"
	"github.com/wakeshift/server/pkg/wakeshift"
	"github.com/wakeshift/server/pkg/wakeshift/mcpgo"
)

type watchIn struct{}

type seenIn struct {
	IDs []string `json:"ids"`
}

func main() {
	app := toy.Open(context.Background())
	c := app.Contract

	srv := server.NewMCPServer(c.ServerName(), c.Server.Version, server.WithToolCapabilities(false))
	// Every caller of a lane's server is that lane's user; its lanes use
	// only their own tools, so attach adds none of its own.
	attached := mcpgo.Attach(app.Kit, srv, wakeshift.AttachOptions{Identify: toy.Lane,
		BuiltinTools: wakeshift.BuiltinTools{DisableSubscribe: true, DisableList: true, DisableUnsubscribe: true, Wait: toy.Wait()}})
	mcpgo.AddTool(attached, mcp.NewTool("watch", mcp.WithDescription(c.Tools.Watch.Description)),
		func(ctx context.Context, call *wakeshift.ToolCall, req mcp.CallToolRequest, _ watchIn) (*mcp.CallToolResult, error) {
			s, err := app.Watch(ctx, call, toy.LaneOf(req.Header), req.Header)
			if err != nil {
				return nil, err
			}
			return mcp.NewToolResultText(s), nil
		}, wakeshift.Subscribes(app.Event))
	mcpgo.AddTool(attached, mcp.NewTool("unwatch", mcp.WithDescription(c.Tools.Unwatch.Description)),
		func(ctx context.Context, call *wakeshift.ToolCall, req mcp.CallToolRequest, _ watchIn) (*mcp.CallToolResult, error) {
			s, err := app.Unwatch(ctx, call, toy.LaneOf(req.Header), req.Header)
			if err != nil {
				return nil, err
			}
			return mcp.NewToolResultText(s), nil
		}, wakeshift.Subscribes())
	mcpgo.AddTool(attached, mcp.NewTool("report_seen", mcp.WithDescription(c.Tools.ReportSeen.Description),
		mcp.WithArray("ids", mcp.Required(), mcp.Description(c.Tools.ReportSeen.IDsDescription), mcp.WithStringItems())),
		func(_ context.Context, _ *wakeshift.ToolCall, req mcp.CallToolRequest, in seenIn) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(app.ReportSeen(toy.LaneOf(req.Header), in.IDs, req.Header)), nil
		}, wakeshift.Subscribes())
	app.Serve(attached.Handler(server.NewStreamableHTTPServer(srv)))
}
