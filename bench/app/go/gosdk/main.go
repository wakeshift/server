// Command gosdk is the bench's toy app on the official Go MCP SDK
// (github.com/modelcontextprotocol/go-sdk), wrapped with the Go SDK
// (pkg/wakeshift), served as the SDK's README serves a stateless Streamable
// HTTP server (WB_APP=go-sdk; bench/app/CONTRACT.md). Shared parts are in
// ../toy.
package main

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/bench/app/go/toy"
	"github.com/wakeshift/server/pkg/wakeshift"
)

type watchIn struct{}

type seenIn struct {
	IDs []string `json:"ids" jsonschema:"The event codes, e.g. WB-ab12-3"`
}

func main() {
	app := toy.Open(context.Background())
	c := app.Contract

	srv := mcp.NewServer(&mcp.Implementation{Name: c.ServerName(), Version: c.Server.Version}, nil)
	// Every caller of a lane's server is that lane's user; its lanes use
	// only their own tools, so attach adds none of its own.
	attached := app.Kit.Attach(srv, wakeshift.AttachOptions{Identify: toy.Lane,
		BuiltinTools: wakeshift.BuiltinTools{DisableSubscribe: true, DisableList: true, DisableUnsubscribe: true, Wait: toy.Wait()}})
	lane := func(call *wakeshift.ToolCall) (string, http.Header) {
		h := call.Request.Extra.Header
		return toy.LaneOf(h), h
	}
	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	wakeshift.AddTool(attached, &mcp.Tool{Name: "watch", Description: c.Tools.Watch.Description},
		func(ctx context.Context, call *wakeshift.ToolCall, _ watchIn) (*mcp.CallToolResult, any, error) {
			l, h := lane(call)
			s, err := app.Watch(ctx, call, l, h)
			if err != nil {
				return nil, nil, err
			}
			return text(s), nil, nil
		}, wakeshift.Subscribes(app.Event))
	wakeshift.AddTool(attached, &mcp.Tool{Name: "unwatch", Description: c.Tools.Unwatch.Description},
		func(ctx context.Context, call *wakeshift.ToolCall, _ watchIn) (*mcp.CallToolResult, any, error) {
			l, h := lane(call)
			s, err := app.Unwatch(ctx, call, l, h)
			if err != nil {
				return nil, nil, err
			}
			return text(s), nil, nil
		}, wakeshift.Subscribes())
	wakeshift.AddTool(attached, &mcp.Tool{Name: "report_seen", Description: c.Tools.ReportSeen.Description},
		func(_ context.Context, call *wakeshift.ToolCall, in seenIn) (*mcp.CallToolResult, any, error) {
			l, h := lane(call)
			return text(app.ReportSeen(l, in.IDs, h)), nil, nil
		}, wakeshift.Subscribes())

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	app.Serve(attached.Handler(handler))
}
