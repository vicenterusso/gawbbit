package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
)

// LibraryVersion is reported as serverInfo.version. FastMCP reported its own
// package version there; the Go server keeps the same value so clients that
// log or pin it see no change.
const LibraryVersion = "3.4.11"

// latestProtocolVersion is what the Python MCP library negotiates when a
// client asks for an unsupported protocol version.
const latestProtocolVersion = "2025-11-25"

var supportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

var (
	datasetIDLogRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	toolNameLogRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)
	actorLabelRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

var (
	toolMeta     = mcp.Meta{"fastmcp": map[string]any{"tags": []string{}}}
	wrappedMeta  = mcp.Meta{"fastmcp": map[string]any{"wrap_result": true}}
	objectOutput = json.RawMessage(`{"additionalProperties":true,"type":"object"}`)
	listOutput   = json.RawMessage(`{"properties":{"result":{"items":{"additionalProperties":true,"type":"object"},"type":"array"}},"required":["result"],"type":"object","x-fastmcp-wrap-result":true}`)
)

// endpoint is one MCP server (main or scoped) with its auditing context.
type endpoint struct {
	server    *mcp.Server
	tools     []*tool
	datasetID string // "" for the main endpoint
	authMode  string
	actorOf   func(http.Header) string
	log       *Logger
	sessions  *sessionSet
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

func newEndpoint(name string, tools []*tool, datasetID, authMode string, actorOf func(http.Header) string, log *Logger) *endpoint {
	e := &endpoint{tools: tools, datasetID: datasetID, authMode: authMode, actorOf: actorOf, log: log,
		sessions: &sessionSet{ids: map[string]bool{}}}
	caps := &mcp.ServerCapabilities{
		Logging:   &mcp.LoggingCapabilities{},
		Prompts:   &mcp.PromptCapabilities{ListChanged: true},
		Resources: &mcp.ResourceCapabilities{ListChanged: true},
		Tools:     &mcp.ToolCapabilities{ListChanged: true},
	}
	caps.AddExtension("io.modelcontextprotocol/ui", nil)
	e.server = mcp.NewServer(&mcp.Implementation{Name: name, Version: LibraryVersion}, &mcp.ServerOptions{
		Capabilities: caps,
		GetSessionID: func() string {
			id := newSessionID()
			e.sessions.add(id)
			log.Info("Created new transport with session ID: %s", id)
			return id
		},
	})
	for _, t := range tools {
		out := objectOutput
		if t.wrapResult {
			out = listOutput
		}
		e.server.AddTool(&mcp.Tool{
			Meta:         toolMeta,
			Name:         t.name,
			Description:  t.description,
			InputSchema:  inputSchema(t.params),
			OutputSchema: out,
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// tools/call is fully handled by the receiving middleware.
			return nil, errors.New("unreachable")
		})
	}
	e.server.AddReceivingMiddleware(e.middleware)
	return e
}

var requestTypeNames = map[string]string{
	"tools/call":               "CallToolRequest",
	"tools/list":               "ListToolsRequest",
	"prompts/list":             "ListPromptsRequest",
	"resources/list":           "ListResourcesRequest",
	"resources/templates/list": "ListResourceTemplatesRequest",
	"ping":                     "PingRequest",
}

func (e *endpoint) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if name, ok := requestTypeNames[method]; ok {
			e.log.Info("Processing request of type %s", name)
		}
		switch method {
		case "initialize":
			if r, ok := req.(*mcp.InitializeRequest); ok && r.Params != nil &&
				!slices.Contains(supportedProtocolVersions, r.Params.ProtocolVersion) {
				r.Params.ProtocolVersion = latestProtocolVersion
			}
		case "tools/list":
			res, err := next(ctx, method, req)
			if lr, ok := res.(*mcp.ListToolsResult); ok && err == nil {
				order := map[string]int{}
				for i, t := range e.tools {
					order[t.name] = i
				}
				slices.SortStableFunc(lr.Tools, func(a, b *mcp.Tool) int { return order[a.Name] - order[b.Name] })
			}
			return res, err
		case "tools/call":
			if r, ok := req.(*mcp.CallToolRequest); ok {
				return e.callTool(ctx, r), nil
			}
		}
		return next(ctx, method, req)
	}
}

func (e *endpoint) find(name string) *tool {
	for _, t := range e.tools {
		if t.name == name {
			return t
		}
	}
	return nil
}

// callTool mirrors FastMCP's tool execution plus SafeToolAuditMiddleware.
func (e *endpoint) callTool(ctx context.Context, req *mcp.CallToolRequest) *mcp.CallToolResult {
	started := time.Now()
	args := pyval.NewDict(0)
	var argsErr error
	if raw := req.Params.Arguments; len(raw) > 0 && string(raw) != "null" {
		decoded, err := pyval.DecodeJSON(raw)
		if d, ok := decoded.(*pyval.Dict); ok && err == nil {
			args = d
		} else {
			argsErr = errors.New("arguments must be an object")
		}
	}

	endpointClass, datasetID := "main", "default"
	if e.datasetID != "" {
		endpointClass, datasetID = "scoped", e.datasetID
	} else if requested, ok := args.Get("dataset_id"); ok && requested != nil {
		if s, isStr := requested.(string); isStr && datasetIDLogRe.MatchString(s) {
			datasetID = s
		} else {
			datasetID = "invalid"
		}
	}
	actor := "unknown"
	if req.Extra != nil && req.Extra.Header != nil {
		actor = e.actorOf(req.Extra.Header)
	}
	toolName := "unknown"
	if toolNameLogRe.MatchString(req.Params.Name) {
		toolName = req.Params.Name
	}

	result, failed := e.execute(ctx, req.Params.Name, args, argsErr)
	outcome := "ok"
	if failed {
		outcome = "exception"
	}
	e.log.Info("tool_call endpoint=%s dataset_id=%s tool=%s actor=%s result=%s duration_ms=%.2f",
		endpointClass, datasetID, toolName, actor, outcome, float64(time.Since(started).Microseconds())/1000)
	return result
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

// execute runs a tool; failed reports an exception (validation, unknown
// tool or a raised error), which FastMCP surfaces as isError results.
func (e *endpoint) execute(ctx context.Context, name string, args *pyval.Dict, argsErr error) (*mcp.CallToolResult, bool) {
	t := e.find(name)
	if t == nil {
		return errorResult(fmt.Sprintf("Unknown tool: %s", pyval.StrRepr(name))), true
	}
	if argsErr != nil {
		return errorResult(fmt.Sprintf("Error calling tool %s: %s", pyval.StrRepr(name), argsErr)), true
	}
	values, err := validateArgs(t.name, t.params, args)
	if err != nil {
		e.log.Warning("Invalid arguments for tool %s", pyval.StrRepr(name))
		return errorResult(err.Error()), true
	}
	value, err := t.run(ctx, values)
	if err != nil {
		e.log.Error("Error calling tool %s", pyval.StrRepr(name))
		return errorResult(fmt.Sprintf("Error calling tool %s: %s", pyval.StrRepr(name), err)), true
	}
	return e.toResult(name, t, value)
}

func (e *endpoint) toResult(name string, t *tool, value pyval.Value) (*mcp.CallToolResult, bool) {
	text, err := pyval.EncodeJSON(value, pyval.NonFiniteConstants)
	if err != nil {
		return errorResult(fmt.Sprintf("Error calling tool %s: %s", pyval.StrRepr(name), err)), true
	}
	structuredValue := value
	if t.wrapResult {
		wrapper := pyval.NewDict(1)
		wrapper.Set("result", value)
		structuredValue = wrapper
	}
	structured, err := pyval.EncodeJSON(structuredValue, pyval.NonFiniteNull)
	if err != nil {
		return errorResult(fmt.Sprintf("Error calling tool %s: %s", pyval.StrRepr(name), err)), true
	}
	content := []mcp.Content{}
	if list, isList := value.(pyval.List); !isList || len(list) > 0 {
		content = append(content, &mcp.TextContent{Text: string(text)})
	}
	res := &mcp.CallToolResult{Content: content, StructuredContent: json.RawMessage(structured)}
	if t.wrapResult {
		res.Meta = wrappedMeta
	}
	return res, false
}
