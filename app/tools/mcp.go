package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"

	"next/internal/config"
)

// MCPTool represents a discovered MCP tool mapped to OpenAI format.
// This type is local to app/tools and uses openai.Tool directly.
type MCPTool struct {
	ServerName string // prefix for tool name
	Name       string // original MCP tool name
	FullName   string // "servername_toolname" registered in ToolRegistry
	Definition openai.Tool
}

// MCPClient manages a connection to a single MCP server.
type MCPClient struct {
	id       int64
	name     string
	url      string
	apiKey   string
	client   *mcpclient.Client
	tools    []MCPTool
	mu       sync.RWMutex
	registry *ToolRegistry
}

// NewMCPClient creates a new (disconnected) MCP client.
func NewMCPClient(id int64, name, url, apiKey string, registry *ToolRegistry) *MCPClient {
	return &MCPClient{
		id:       id,
		name:     name,
		url:      url,
		apiKey:   apiKey,
		registry: registry,
	}
}

// Connect establishes the SSE connection, initializes protocol, and discovers tools.
func (mc *MCPClient) Connect() error {
	var opts []transport.ClientOption
	if mc.apiKey != "" {
		opts = append(opts, transport.WithHeaders(map[string]string{
			"Authorization": "Bearer " + mc.apiKey,
		}))
	}

	client, err := mcpclient.NewSSEMCPClient(mc.url, opts...)
	if err != nil {
		return fmt.Errorf("create MCP client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		client.Close()
		return fmt.Errorf("start MCP: %w", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "next",
		Version: "1.0.0",
	}

	if _, err := client.Initialize(ctx, initReq); err != nil {
		client.Close()
		return fmt.Errorf("initialize MCP: %w", err)
	}

	mc.mu.Lock()
	mc.client = client
	mc.mu.Unlock()

	if err := mc.discoverTools(); err != nil {
		client.Close()
		return fmt.Errorf("discover tools: %w", err)
	}

	return nil
}

// discoverTools lists tools from the MCP server and registers them.
func (mc *MCPClient) discoverTools() error {
	mc.mu.RLock()
	client := mc.client
	mc.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("not connected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return err
	}

	mc.mu.Lock()
	mc.tools = nil
	mc.mu.Unlock()

	serverPrefix := SanitizeMCPName(mc.name)

	for _, tool := range result.Tools {
		fullName := serverPrefix + "_" + SanitizeMCPName(tool.Name)

		def := MCPToolToOpenAI(fullName, tool)
		mcpTool := MCPTool{
			ServerName: mc.name,
			Name:       tool.Name,
			FullName:   fullName,
			Definition: def,
		}

		mc.mu.Lock()
		mc.tools = append(mc.tools, mcpTool)
		mc.mu.Unlock()

		handler := ToolHandler{
			Definition: def,
			Execute:    mc.makeExecutor(tool.Name),
		}
		mc.registry.RegisterMCPTool(fullName, handler)
	}

	return nil
}

// Disconnect closes the MCP client connection.
func (mc *MCPClient) Disconnect() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.client != nil {
		mc.client.Close()
		mc.client = nil
	}
	mc.tools = nil
}

// IsConnected returns whether the client has an active connection.
func (mc *MCPClient) IsConnected() bool {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.client != nil
}

// GetTools returns the list of discovered MCP tools.
func (mc *MCPClient) GetTools() []MCPTool {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	result := make([]MCPTool, len(mc.tools))
	copy(result, mc.tools)
	return result
}

// makeExecutor returns a tool executor that calls the MCP server.
func (mc *MCPClient) makeExecutor(toolName string) func(chatID, args string) (string, error) {
	return func(chatID, args string) (string, error) {
		mc.mu.RLock()
		client := mc.client
		mc.mu.RUnlock()

		if client == nil {
			return "Erro: servidor MCP desconectado.", nil
		}

		var argsMap map[string]any
		json.Unmarshal([]byte(args), &argsMap)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		callReq := mcp.CallToolRequest{}
		callReq.Params.Name = toolName
		callReq.Params.Arguments = argsMap

		result, err := client.CallTool(ctx, callReq)
		if err != nil {
			return fmt.Sprintf("Erro MCP: %s", err), nil
		}
		if result.IsError {
			return fmt.Sprintf("Erro na tool: %s", ExtractMCPText(result.Content)), nil
		}
		text := ExtractMCPText(result.Content)
		if len(text) > 4000 {
			text = text[:4000] + "\n[...truncado]"
		}
		return text, nil
	}
}

// MCPToolToOpenAI converts an MCP tool definition to OpenAI format.
// Exported for use in tests.
func MCPToolToOpenAI(fullName string, tool mcp.Tool) openai.Tool {
	schemaBytes, err := json.Marshal(tool.InputSchema)
	if err != nil {
		schemaBytes = []byte(`{"type":"object","properties":{}}`)
	}

	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        fullName,
			Description: tool.Description,
			Parameters:  json.RawMessage(schemaBytes),
		},
	}
}

// ExtractMCPText concatenates text content from an MCP result.
// Exported for use in tests.
func ExtractMCPText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if tc, ok := c.(mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	if len(parts) == 0 {
		return "Sem conteudo."
	}
	return strings.Join(parts, "\n")
}

var reMCPNameClean = regexp.MustCompile(`[^a-z0-9_]`)

// SanitizeMCPName converts a name to a safe tool identifier.
// Exported for use in tests.
func SanitizeMCPName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	s = reMCPNameClean.ReplaceAllString(s, "")
	if s == "" {
		s = "mcp"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "m" + s
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// --- NextMCPServer: exposes Next tools via MCP SSE protocol ---

// NextMCPServer wraps a mcp-go SSE server that exposes Next's tool registry.
type NextMCPServer struct {
	sse   *mcpserver.SSEServer
	cfg   *config.Config
	tools *ToolRegistry
}

// NewNextMCPServer creates and configures the MCP server with all registered tools.
func NewNextMCPServer(tools *ToolRegistry, cfg *config.Config) *NextMCPServer {
	srv := mcpserver.NewMCPServer("Next", "1.0.0")

	tools.mu.RLock()
	for name, handler := range tools.tools {
		if handler.Definition.Function == nil {
			continue
		}
		fn := handler.Definition.Function
		toolName := name
		var schema json.RawMessage
		switch p := fn.Parameters.(type) {
		case json.RawMessage:
			schema = p
		default:
			schema, _ = json.Marshal(p)
		}
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		mcpTool := mcp.NewToolWithRawSchema(fn.Name, fn.Description, schema)
		srv.AddTool(mcpTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			argsJSON, _ := json.Marshal(req.GetArguments())
			result, err := tools.Execute(toolName, "mcp", string(argsJSON))
			if err != nil {
				return mcp.NewToolResultText("Erro: " + err.Error()), nil
			}
			return mcp.NewToolResultText(result), nil
		})
	}
	tools.mu.RUnlock()

	sseServer := mcpserver.NewSSEServer(srv,
		mcpserver.WithStaticBasePath("/mcp"),
		mcpserver.WithSSEEndpoint("/sse"),
		mcpserver.WithMessageEndpoint("/message"),
	)

	return &NextMCPServer{sse: sseServer, cfg: cfg, tools: tools}
}

// SSEHandler returns the SSE endpoint handler with optional bearer token auth.
func (n *NextMCPServer) SSEHandler() http.Handler {
	return n.authMiddleware(n.sse.SSEHandler())
}

// MessageHandler returns the message endpoint handler with optional bearer token auth.
func (n *NextMCPServer) MessageHandler() http.Handler {
	return n.authMiddleware(n.sse.MessageHandler())
}

func (n *NextMCPServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		n.cfg.Mu.RLock()
		token := n.cfg.MCPServerToken
		n.cfg.Mu.RUnlock()

		if token != "" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+token {
				rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(401)
			rw.Write([]byte(`{"error":"Unauthorized"}`))
				return
			}
		}
		next.ServeHTTP(rw, r)
	})
}
