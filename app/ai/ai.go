package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"next/app/types"
	"next/internal/logger"
)

const (
	EmbCacheTTL     = 5 * time.Minute
	EmbCacheMaxSize = 500
)

type EmbCacheEntry struct {
	Vec []float32
	At  time.Time
}

const DefaultLLMTimeout = 120 * time.Second

// AI wraps an OpenAI-compatible client for reply and summarization.
type AI struct {
	mu      sync.RWMutex
	client  *openai.Client
	logger  *logger.Logger
	timeout time.Duration
	// Embedding cache — deterministic, safe to cache indefinitely within TTL
	EmbMu    sync.Mutex
	EmbCache map[string]EmbCacheEntry
}

// NewAI creates a new AI client with a custom base URL.
func NewAI(baseURL, apiKey string, l *logger.Logger) *AI {
	a := &AI{
		logger:   l,
		timeout:  DefaultLLMTimeout,
		EmbCache: make(map[string]EmbCacheEntry),
	}
	a.updateClient(baseURL, apiKey)
	return a
}

// llmContext returns a context with the configured LLM timeout.
func (a *AI) llmContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), a.timeout)
}

// isRetryable returns true for transient errors (5xx, 429, network timeouts).
// Does NOT retry on context.DeadlineExceeded (our own timeout).
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Network timeout
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// OpenAI API errors: 429 or 5xx
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == 429 || apiErr.HTTPStatusCode >= 500
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "status code: 429") ||
		strings.Contains(errMsg, "status code: 5")
}

// doCreateChatCompletion wraps client.CreateChatCompletion with 1 retry on transient errors.
func doCreateChatCompletion(ctx context.Context, client *openai.Client, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	resp, err := client.CreateChatCompletion(ctx, req)
	if err != nil && isRetryable(err) {
		time.Sleep(1 * time.Second)
		resp, err = client.CreateChatCompletion(ctx, req)
	}
	return resp, err
}

// UpdateClient recreates the client when config changes.
func (a *AI) UpdateClient(baseURL, apiKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updateClient(baseURL, apiKey)
}

func (a *AI) updateClient(baseURL, apiKey string) {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	a.client = openai.NewClientWithConfig(cfg)
}

// logLLMRequest logs the full messages sent to the LLM.
func (a *AI) logLLMRequest(label, chatID, model string, maxTokens int, msgs []openai.ChatCompletionMessage, hasTools bool, tools []openai.Tool) {
	if a.logger == nil {
		return
	}
	msgList := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]string, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				calls[i] = map[string]string{"id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments}
			}
			entry["tool_calls"] = calls
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		msgList = append(msgList, entry)
	}
	var toolNames []string
	for _, t := range tools {
		toolNames = append(toolNames, t.Function.Name)
	}
	a.logger.Log("ai_llm_request", chatID, map[string]any{
		"label": label, "model": model, "max_tokens": maxTokens,
		"messages": msgList, "msg_count": len(msgs), "has_tools": hasTools,
		"tool_names": toolNames, "tool_count": len(tools),
	})
}

// logLLMResponse logs the full response received from the LLM.
func (a *AI) logLLMResponse(label, chatID, model string, resp openai.ChatCompletionResponse, latencyMs int64) {
	if a.logger == nil {
		return
	}
	data := map[string]any{
		"label": label, "model": model, "latency_ms": latencyMs,
	}
	if resp.Usage.TotalTokens > 0 {
		data["prompt_tokens"] = resp.Usage.PromptTokens
		data["completion_tokens"] = resp.Usage.CompletionTokens
		data["total_tokens"] = resp.Usage.TotalTokens
	}
	if len(resp.Choices) > 0 {
		data["finish_reason"] = string(resp.Choices[0].FinishReason)
		data["content"] = resp.Choices[0].Message.Content
		if len(resp.Choices[0].Message.ToolCalls) > 0 {
			calls := make([]map[string]string, len(resp.Choices[0].Message.ToolCalls))
			for i, tc := range resp.Choices[0].Message.ToolCalls {
				calls[i] = map[string]string{"id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments}
			}
			data["tool_calls"] = calls
		}
	}
	a.logger.Log("ai_llm_response", chatID, data)
}

// buildMessages assembles the chat messages array used by Reply and ReplyWithTools.
// RAG context and summary are consolidated into the system prompt to avoid
// multiple consecutive system messages which some APIs handle poorly.
func (a *AI) buildMessages(systemPrompt, userPrompt, ragContext, summary string, history []types.Message, userMsg string) []openai.ChatCompletionMessage {
	var msgs []openai.ChatCompletionMessage

	// Consolidate system prompt + RAG + summary into a single system message
	sys := systemPrompt
	if ragContext != "" {
		sys += "\n\n---\nBase de conhecimento:\n" + ragContext
	}
	if summary != "" {
		sys += "\n\n---\nContexto da conversa anterior:\n" + summary
	}
	msgs = append(msgs, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: sys,
	})

	if userPrompt != "" {
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: userPrompt,
		})
	}
	for _, m := range history {
		role := openai.ChatMessageRoleUser
		if m.Role == "assistant" {
			role = openai.ChatMessageRoleAssistant
		}
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    role,
			Content: m.Content,
		})
	}
	msgs = append(msgs, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userMsg,
	})
	return msgs
}

// Reply sends a message to the AI and returns the response.
func (a *AI) Reply(chatID, model string, maxTokens int, systemPrompt, userPrompt, ragContext, summary string, history []types.Message, userMsg string) (string, error) {
	a.mu.RLock()
	client := a.client
	a.mu.RUnlock()

	msgs := a.buildMessages(systemPrompt, userPrompt, ragContext, summary, history, userMsg)
	a.logLLMRequest("reply", chatID, model, maxTokens, msgs, false, nil)

	ctx, cancel := a.llmContext()
	defer cancel()

	start := time.Now()
	resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", fmt.Errorf("ai reply: %w", err)
	}
	a.logLLMResponse("reply", chatID, model, resp, latency)
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ai reply: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}

// ReplyWithTools sends a message with function calling support.
// Loops until the AI produces a final text response or maxRounds is reached.
func (a *AI) ReplyWithTools(
	model string, maxTokens int,
	systemPrompt, userPrompt, ragContext, summary string,
	history []types.Message, userMsg string,
	tools []openai.Tool,
	executeTool func(name, chatID, args string) (string, error),
	chatID string, maxRounds int, l *logger.Logger,
) (string, error) {
	a.mu.RLock()
	client := a.client
	a.mu.RUnlock()

	msgs := a.buildMessages(systemPrompt, userPrompt, ragContext, summary, history, userMsg)

	// Single context for the entire tool loop
	ctx, cancel := a.llmContext()
	defer cancel()

	// Tool call loop
	for round := 0; round < maxRounds; round++ {
		a.logLLMRequest(fmt.Sprintf("reply_tools_round_%d", round), chatID, model, maxTokens, msgs, true, tools)

		start := time.Now()
		resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
			Model:     model,
			Messages:  msgs,
			MaxTokens: maxTokens,
			Tools:     tools,
		})
		latency := time.Since(start).Milliseconds()
		if err != nil {
			return "", fmt.Errorf("ai reply (round %d): %w", round, err)
		}
		a.logLLMResponse(fmt.Sprintf("reply_tools_round_%d", round), chatID, model, resp, latency)
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("ai reply: no choices returned")
		}

		choice := resp.Choices[0]

		// If no tool calls, return the text response
		if choice.FinishReason != openai.FinishReasonToolCalls || len(choice.Message.ToolCalls) == 0 {
			return choice.Message.Content, nil
		}

		// Append assistant message with tool calls
		msgs = append(msgs, choice.Message)

		// Execute each tool call
		for _, tc := range choice.Message.ToolCalls {
			start := time.Now()
			result, execErr := executeTool(tc.Function.Name, chatID, tc.Function.Arguments)
			latency := time.Since(start).Milliseconds()

			if l != nil {
				l.Log("ai_tool_call", chatID, map[string]any{
					"tool":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
					"round":     round,
				})
			}

			if execErr != nil {
				result = "Erro: " + execErr.Error()
			}

			if l != nil {
				l.Log("ai_tool_result", chatID, map[string]any{
					"tool":       tc.Function.Name,
					"result":     result,
					"error":      execErr != nil,
					"latency_ms": latency,
				})
			}

			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	// Exhausted rounds — final call without tools to get text response
	a.logLLMRequest("reply_tools_final", chatID, model, maxTokens, msgs, false, nil)
	startFinal := time.Now()
	resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
	})
	latencyFinal := time.Since(startFinal).Milliseconds()
	if err != nil {
		return "", fmt.Errorf("ai reply (final): %w", err)
	}
	a.logLLMResponse("reply_tools_final", chatID, model, resp, latencyFinal)
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ai reply: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}

// makeClient creates an openai.Client for the given base URL and API key.
// If both are empty, returns the global client.
func (a *AI) makeClient(baseURL, apiKey string) *openai.Client {
	if baseURL == "" && apiKey == "" {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.client
	}
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	return openai.NewClientWithConfig(cfg)
}

// ReplyWithClient sends a message using an agent-specific or global client.
func (a *AI) ReplyWithClient(chatID, baseURL, apiKey, model string, maxTokens int, systemPrompt, userPrompt, ragContext, summary string, history []types.Message, userMsg string) (string, error) {
	client := a.makeClient(baseURL, apiKey)

	msgs := a.buildMessages(systemPrompt, userPrompt, ragContext, summary, history, userMsg)
	a.logLLMRequest("reply_client", chatID, model, maxTokens, msgs, false, nil)

	ctx, cancel := a.llmContext()
	defer cancel()

	start := time.Now()
	resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", fmt.Errorf("ai reply: %w", err)
	}
	a.logLLMResponse("reply_client", chatID, model, resp, latency)
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ai reply: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}

// ReplyWithToolsClient sends a message with function calling using an agent-specific or global client.
func (a *AI) ReplyWithToolsClient(
	baseURL, apiKey, model string, maxTokens int,
	systemPrompt, userPrompt, ragContext, summary string,
	history []types.Message, userMsg string,
	tools []openai.Tool,
	executeTool func(name, chatID, args string) (string, error),
	chatID string, maxRounds int, l *logger.Logger,
) (string, error) {
	client := a.makeClient(baseURL, apiKey)

	msgs := a.buildMessages(systemPrompt, userPrompt, ragContext, summary, history, userMsg)

	// Single context for the entire tool loop
	ctx, cancel := a.llmContext()
	defer cancel()

	for round := 0; round < maxRounds; round++ {
		a.logLLMRequest(fmt.Sprintf("reply_tools_client_round_%d", round), chatID, model, maxTokens, msgs, true, tools)

		startRound := time.Now()
		resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
			Model:     model,
			Messages:  msgs,
			MaxTokens: maxTokens,
			Tools:     tools,
		})
		latencyRound := time.Since(startRound).Milliseconds()
		if err != nil {
			return "", fmt.Errorf("ai reply (round %d): %w", round, err)
		}
		a.logLLMResponse(fmt.Sprintf("reply_tools_client_round_%d", round), chatID, model, resp, latencyRound)
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("ai reply: no choices returned")
		}

		choice := resp.Choices[0]
		if choice.FinishReason != openai.FinishReasonToolCalls || len(choice.Message.ToolCalls) == 0 {
			return choice.Message.Content, nil
		}

		msgs = append(msgs, choice.Message)
		for _, tc := range choice.Message.ToolCalls {
			startTC := time.Now()
			result, execErr := executeTool(tc.Function.Name, chatID, tc.Function.Arguments)
			latencyTC := time.Since(startTC).Milliseconds()

			if l != nil {
				l.Log("ai_tool_call", chatID, map[string]any{
					"tool": tc.Function.Name, "arguments": tc.Function.Arguments, "round": round,
				})
			}
			if execErr != nil {
				result = "Erro: " + execErr.Error()
			}
			if l != nil {
				l.Log("ai_tool_result", chatID, map[string]any{
					"tool": tc.Function.Name, "result": result, "error": execErr != nil, "latency_ms": latencyTC,
				})
			}

			msgs = append(msgs, openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleTool, Content: result, ToolCallID: tc.ID,
			})
		}
	}

	a.logLLMRequest("reply_tools_client_final", chatID, model, maxTokens, msgs, false, nil)
	startFinal := time.Now()
	resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
		Model: model, Messages: msgs, MaxTokens: maxTokens,
	})
	latencyFinal := time.Since(startFinal).Milliseconds()
	if err != nil {
		return "", fmt.Errorf("ai reply (final): %w", err)
	}
	a.logLLMResponse("reply_tools_client_final", chatID, model, resp, latencyFinal)
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ai reply: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}

// Summarize generates a short summary of a conversation.
func (a *AI) Summarize(model string, messages []types.Message) (string, error) {
	a.mu.RLock()
	client := a.client
	a.mu.RUnlock()

	var msgs []openai.ChatCompletionMessage

	msgs = append(msgs, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: "Resuma esta conversa em no maximo 2 linhas curtas. Inclua apenas fatos importantes (nomes, datas, decisoes, pedidos). Nao inclua cumprimentos ou detalhes irrelevantes.",
	})

	for _, m := range messages {
		role := openai.ChatMessageRoleUser
		if m.Role == "assistant" {
			role = openai.ChatMessageRoleAssistant
		}
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    role,
			Content: m.Content,
		})
	}

	a.logLLMRequest("summarize", "", model, 200, msgs, false, nil)
	ctx, cancel := a.llmContext()
	defer cancel()
	start := time.Now()
	resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: 200,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", fmt.Errorf("ai summarize: %w", err)
	}
	a.logLLMResponse("summarize", "", model, resp, latency)
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ai summarize: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}

// KnowledgeItem represents a single piece of knowledge extracted from a conversation.
type KnowledgeItem struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// ExtractKnowledge asks the AI to extract factual knowledge items from a conversation.
// Returns up to 5 items. Best-effort: returns empty slice on parse errors.
func (a *AI) ExtractKnowledge(model string, messages []types.Message) ([]KnowledgeItem, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	a.mu.RLock()
	client := a.client
	a.mu.RUnlock()

	var msgs []openai.ChatCompletionMessage
	msgs = append(msgs, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleSystem,
		Content: `Analise a conversa abaixo e extraia conhecimentos factuais uteis que possam melhorar respostas futuras.
Retorne um JSON array com no maximo 5 itens no formato: [{"title":"titulo curto","content":"conteudo factual"}]
Regras:
- Apenas fatos concretos (nomes, datas, preferencias, decisoes, informacoes de negocio)
- Nao inclua cumprimentos, opinioes ou informacoes genericas
- Se nao houver conhecimento util, retorne []
- Responda APENAS com o JSON, sem texto adicional`,
	})

	for _, m := range messages {
		role := openai.ChatMessageRoleUser
		if m.Role == "assistant" {
			role = openai.ChatMessageRoleAssistant
		}
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    role,
			Content: m.Content,
		})
	}

	a.logLLMRequest("extract_knowledge", "", model, 500, msgs, false, nil)
	ctx, cancel := a.llmContext()
	defer cancel()
	start := time.Now()
	resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: 500,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("ai extract_knowledge: %w", err)
	}
	a.logLLMResponse("extract_knowledge", "", model, resp, latency)
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("ai extract_knowledge: no choices returned")
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	// Strip markdown code fences if present
	if strings.HasPrefix(raw, "```") {
		if idx := strings.Index(raw[3:], "\n"); idx >= 0 {
			raw = raw[3+idx+1:]
		}
		if strings.HasSuffix(raw, "```") {
			raw = raw[:len(raw)-3]
		}
		raw = strings.TrimSpace(raw)
	}

	var items []KnowledgeItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		// Best-effort: return empty on parse error
		return nil, nil
	}

	// Cap at 5 items
	if len(items) > 5 {
		items = items[:5]
	}

	return items, nil
}

// Embed generates an embedding vector for the given text.
// Results are cached in-memory (embeddings are deterministic).
func (a *AI) Embed(model, text string) ([]float32, error) {
	key := model + "\x00" + text

	// Check cache
	a.EmbMu.Lock()
	if entry, ok := a.EmbCache[key]; ok && time.Since(entry.At) < EmbCacheTTL {
		a.EmbMu.Unlock()
		return entry.Vec, nil
	}
	a.EmbMu.Unlock()

	// Cache miss — call API
	a.mu.RLock()
	client := a.client
	a.mu.RUnlock()

	if a.logger != nil {
		a.logger.Log("ai_llm_request", "", map[string]any{
			"label": "embed", "model": model, "text": text,
		})
	}
	ctx, cancel := a.llmContext()
	defer cancel()
	startEmb := time.Now()
	resp, err := client.CreateEmbeddings(
		ctx,
		openai.EmbeddingRequestStrings{
			Input: []string{text},
			Model: openai.EmbeddingModel(model),
		},
	)
	latencyEmb := time.Since(startEmb).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("ai embed: %w", err)
	}
	if a.logger != nil {
		a.logger.Log("ai_llm_response", "", map[string]any{
			"label": "embed", "model": model, "latency_ms": latencyEmb,
			"dimensions": len(resp.Data[0].Embedding),
		})
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("ai embed: no data returned")
	}
	vec := resp.Data[0].Embedding

	// Store in cache + lazy cleanup when over max size
	a.EmbMu.Lock()
	a.EmbCache[key] = EmbCacheEntry{Vec: vec, At: time.Now()}
	if len(a.EmbCache) > EmbCacheMaxSize {
		now := time.Now()
		// First pass: evict expired entries
		for k, e := range a.EmbCache {
			if now.Sub(e.At) >= EmbCacheTTL {
				delete(a.EmbCache, k)
			}
		}
		// Second pass: if still over limit, evict oldest (pseudo-LRU)
		for len(a.EmbCache) > EmbCacheMaxSize {
			var oldestKey string
			var oldestTime time.Time
			first := true
			for k, e := range a.EmbCache {
				if first || e.At.Before(oldestTime) {
					oldestKey = k
					oldestTime = e.At
					first = false
				}
			}
			if oldestKey != "" {
				delete(a.EmbCache, oldestKey)
			} else {
				break
			}
		}
	}
	a.EmbMu.Unlock()

	return vec, nil
}

// CompressRAG condenses a knowledge base text, preserving key facts.
func (a *AI) CompressRAG(model, content string) (string, error) {
	a.mu.RLock()
	client := a.client
	a.mu.RUnlock()

	compMsgs := []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: "Condense este texto mantendo todos os fatos, nomes e numeros importantes. Remova redundancias. Responda apenas com o texto condensado.",
		},
		{
			Role:    openai.ChatMessageRoleUser,
			Content: content,
		},
	}
	a.logLLMRequest("compress", "", model, 200, compMsgs, false, nil)
	ctx, cancel := a.llmContext()
	defer cancel()
	start := time.Now()
	resp, err := doCreateChatCompletion(ctx, client, openai.ChatCompletionRequest{
		Model:     model,
		Messages:  compMsgs,
		MaxTokens: 200,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", fmt.Errorf("ai compress: %w", err)
	}
	a.logLLMResponse("compress", "", model, resp, latency)
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ai compress: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}
