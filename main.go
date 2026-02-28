// Package main implements a reference agent that follows the platform's
// agent contract. It reads a task, calls an LLM, executes tool calls
// via MCP, iterates until done, and writes structured output to stdout.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- Agent Contract Types (mirrors control-plane/pkg/agent) ---

type taskInput struct {
	TaskID        string         `json:"task_id"`
	Prompt        string         `json:"prompt"`
	SystemPrompt  string         `json:"system_prompt,omitempty"`
	Tools         []toolEndpoint `json:"tools,omitempty"`
	Context       []contextEntry `json:"context,omitempty"`
	MaxIterations int            `json:"max_iterations,omitempty"`
}

type toolEndpoint struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Address   string `json:"address"`
}

type contextEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type taskOutput struct {
	TaskID    string           `json:"task_id"`
	Status    string           `json:"status"`
	Result    string           `json:"result,omitempty"`
	Error     string           `json:"error,omitempty"`
	ToolCalls []toolCallRecord `json:"tool_calls,omitempty"`
}

type toolCallRecord struct {
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

// --- Anthropic API Types ---

type anthropicRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []llmTool `json:"tools,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type llmTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicResponse struct {
	Content  []contentBlock `json:"content"`
	StopReason string       `json:"stop_reason"`
	Usage    struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type contentBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
}

// --- MCP JSON-RPC Types ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolsListResult struct {
	Tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	} `json:"tools"`
}

type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func main() {
	log := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[agent] "+format+"\n", args...)
	}

	// 1. Read task input.
	input, err := readInput()
	if err != nil {
		writeOutput(taskOutput{Status: "failed", Error: fmt.Sprintf("reading input: %v", err)})
		os.Exit(1)
	}
	log("task_id=%s prompt=%q tools=%d", input.TaskID, truncate(input.Prompt, 80), len(input.Tools))

	// 2. Discover available MCP tools.
	tools := input.Tools
	if len(tools) == 0 {
		tools = parseToolEndpoints(os.Getenv("TOOL_ENDPOINTS"))
	}

	// 3. Fetch tool definitions from MCP servers.
	var llmTools []llmTool
	toolAddrs := map[string]string{} // tool name -> address
	for _, t := range tools {
		if t.Transport == "http" {
			defs, err := fetchMCPTools(t.Address)
			if err != nil {
				log("warning: failed to fetch tools from %s: %v", t.Name, err)
				continue
			}
			for _, d := range defs {
				llmTools = append(llmTools, d)
				toolAddrs[d.Name] = t.Address
			}
			log("discovered %d tools from %s", len(defs), t.Name)
		}
	}

	// 4. Build conversation with system prompt.
	systemPrompt := input.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = os.Getenv("SYSTEM_PROMPT")
	}
	if systemPrompt == "" {
		systemPrompt = "You are a helpful assistant. Complete the user's task. Use available tools when needed."
	}

	messages := []message{}
	for _, c := range input.Context {
		messages = append(messages, message{Role: c.Role, Content: c.Content})
	}
	messages = append(messages, message{Role: "user", Content: input.Prompt})

	// 5. Agent loop: call LLM, execute tool calls, repeat.
	maxIter := input.MaxIterations
	if maxIter <= 0 {
		maxIter = 10
	}

	var allToolCalls []toolCallRecord

	for i := 0; i < maxIter; i++ {
		log("iteration %d/%d", i+1, maxIter)

		resp, err := callLLM(systemPrompt, messages, llmTools)
		if err != nil {
			writeOutput(taskOutput{
				TaskID:    input.TaskID,
				Status:    "failed",
				Error:     fmt.Sprintf("llm call: %v", err),
				ToolCalls: allToolCalls,
			})
			os.Exit(1)
		}

		// Process response blocks.
		var assistantContent []any
		hasToolUse := false

		for _, block := range resp.Content {
			switch block.Type {
			case "text":
				assistantContent = append(assistantContent, block)
			case "tool_use":
				hasToolUse = true
				assistantContent = append(assistantContent, block)
			}
		}

		// Add assistant message.
		messages = append(messages, message{Role: "assistant", Content: assistantContent})

		// If no tool use, we're done.
		if !hasToolUse || resp.StopReason == "end_turn" {
			// Extract the final text.
			var result string
			for _, block := range resp.Content {
				if block.Type == "text" {
					result += block.Text
				}
			}
			writeOutput(taskOutput{
				TaskID:    input.TaskID,
				Status:    "completed",
				Result:    result,
				ToolCalls: allToolCalls,
			})
			return
		}

		// Execute tool calls.
		var toolResults []any
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}

			addr, ok := toolAddrs[block.Name]
			if !ok {
				log("unknown tool: %s", block.Name)
				toolResults = append(toolResults, map[string]any{
					"type":        "tool_result",
					"tool_use_id": block.ID,
					"content":     fmt.Sprintf("error: unknown tool %q", block.Name),
				})
				continue
			}

			inputJSON, _ := json.Marshal(block.Input)
			log("calling tool %s: %s", block.Name, string(inputJSON))

			result, err := callMCPTool(addr, block.Name, block.Input)
			if err != nil {
				log("tool %s error: %v", block.Name, err)
				toolResults = append(toolResults, map[string]any{
					"type":        "tool_result",
					"tool_use_id": block.ID,
					"content":     fmt.Sprintf("error: %v", err),
				})
			} else {
				log("tool %s result: %s", block.Name, truncate(result, 200))
				toolResults = append(toolResults, map[string]any{
					"type":        "tool_result",
					"tool_use_id": block.ID,
					"content":     result,
				})
			}

			allToolCalls = append(allToolCalls, toolCallRecord{
				Tool:   block.Name,
				Input:  string(inputJSON),
				Output: result,
			})
		}

		// Add tool results as user message.
		messages = append(messages, message{Role: "user", Content: toolResults})
	}

	// Exceeded max iterations.
	writeOutput(taskOutput{
		TaskID:    input.TaskID,
		Status:    "failed",
		Error:     fmt.Sprintf("exceeded max iterations (%d)", maxIter),
		ToolCalls: allToolCalls,
	})
	os.Exit(1)
}

// readInput reads the task input from TASK_PAYLOAD env var or stdin.
func readInput() (*taskInput, error) {
	var data []byte
	if payload := os.Getenv("TASK_PAYLOAD"); payload != "" {
		data = []byte(payload)
	} else {
		var err error
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("no task input provided (set TASK_PAYLOAD or pipe to stdin)")
	}

	var input taskInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("parsing task input: %w", err)
	}
	return &input, nil
}

// writeOutput writes the task output as JSON to stdout.
func writeOutput(out taskOutput) {
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

// parseToolEndpoints parses the TOOL_ENDPOINTS env var.
func parseToolEndpoints(s string) []toolEndpoint {
	if s == "" {
		return nil
	}
	var tools []toolEndpoint
	for _, entry := range strings.Split(s, ",") {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		addr := parts[1]
		transport := "http"
		if strings.HasPrefix(addr, "stdio://") {
			transport = "stdio"
		}
		tools = append(tools, toolEndpoint{Name: name, Transport: transport, Address: addr})
	}
	return tools
}

// callLLM makes a request to the Anthropic API (through the proxy).
func callLLM(system string, messages []message, tools []llmTool) (*anthropicResponse, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-sonnet-4-20250514"
	}

	reqBody := anthropicRequest{
		Model:     model,
		MaxTokens: 4096,
		System:    system,
		Messages:  messages,
	}
	if len(tools) > 0 {
		reqBody.Tools = tools
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result anthropicResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &result, nil
}

// fetchMCPTools calls tools/list on an MCP HTTP server.
func fetchMCPTools(addr string) ([]llmTool, error) {
	rpc := rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/list",
	}
	body, _ := json.Marshal(rpc)

	resp, err := http.Post(addr, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("calling tools/list: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decoding tools/list response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("tools/list error: %s", rpcResp.Error.Message)
	}

	var result toolsListResult
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		return nil, fmt.Errorf("parsing tools list: %w", err)
	}

	var tools []llmTool
	for _, t := range result.Tools {
		tools = append(tools, llmTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return tools, nil
}

// callMCPTool invokes a tool via MCP JSON-RPC over HTTP.
func callMCPTool(addr, name string, arguments any) (string, error) {
	rpc := rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
	body, _ := json.Marshal(rpc)

	resp, err := http.Post(addr, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("calling tool %s: %w", name, err)
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return "", fmt.Errorf("decoding tool response: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("tool error: %s", rpcResp.Error.Message)
	}

	var result toolCallResult
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		return "", fmt.Errorf("parsing tool result: %w", err)
	}

	var texts []string
	for _, c := range result.Content {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
