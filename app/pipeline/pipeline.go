package pipeline

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"next/app/ai"
	"next/app/guardrails"
	"next/app/memory"
	apprag "next/app/rag"
	"next/app/tools"
	"next/app/types"
	"next/internal/config"
	"next/internal/logger"
)

// Pipeline centralizes the message processing logic shared by
// WhatsApp (main.go) and the web chat (internal/web).
type Pipeline struct {
	cfg    *config.Config
	memory *memory.Memory
	rag    *apprag.RAG
	ai     *ai.AI
	logger *logger.Logger
	guard  *guardrails.Guardrails
	tools  *tools.ToolRegistry
	chatMu sync.Map // chatID → *sync.Mutex
}

// getChatMu returns a per-chat mutex, creating one if needed.
func (p *Pipeline) getChatMu(chatID string) *sync.Mutex {
	v, _ := p.chatMu.LoadOrStore(chatID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// NewPipeline creates a pipeline with all dependencies.
func NewPipeline(cfg *config.Config, mem *memory.Memory, rag *apprag.RAG, a *ai.AI, l *logger.Logger, guard *guardrails.Guardrails, tr *tools.ToolRegistry) *Pipeline {
	return &Pipeline{cfg: cfg, memory: mem, rag: rag, ai: a, logger: l, guard: guard, tools: tr}
}

// Process runs the full pipeline: guardrails → session → RAG → AI → guardrails → save.
// Returns the result without sending it anywhere — the caller decides delivery.
func (p *Pipeline) Process(chatID, text, contactName string) types.PipelineResult {
	return p.ProcessWithAgent(chatID, text, contactName, 0)
}

// ProcessWithAgent processes a message optionally forcing a specific agent by ID.
func (p *Pipeline) ProcessWithAgent(chatID, text, contactName string, agentID int64) types.PipelineResult {
	// Lock per chatID to prevent concurrent session manipulation
	mu := p.getChatMu(chatID)
	mu.Lock()
	defer mu.Unlock()

	// 1. Read global config snapshot
	p.cfg.Mu.RLock()
	timeoutMin := p.cfg.SessionTimeoutMin
	maxHistory := p.cfg.MaxHistory
	contextBudget := p.cfg.ContextBudget
	p.cfg.Mu.RUnlock()

	// 2. Agent resolution: explicit agentID > routing > default agent
	var agent *types.Agent
	var agentRAGTags []string
	if p.tools != nil {
		if agentID > 0 {
			agent = p.tools.GetAgentByID(agentID)
		}
		if agent == nil {
			agent = p.tools.GetAgentForChatID(chatID)
		}
		if agent == nil {
			agent = p.tools.GetDefaultAgent()
		}
	}

	// 3. Extract per-agent settings (fallback = off when no agent)
	systemPrompt, userPrompt, model, maxTokens := "", "", "gpt-4.1-mini", 1024
	ragEnabled, ragMaxResults, ragCompressed, ragMaxTokens := false, 3, false, 500
	ragEmbeddings := false
	ragEmbeddingModel := ""
	toolsEnabled, toolsMaxRounds, toolTimeoutSec := false, 3, 30
	var guardSettings guardrails.GuardSettings

	if agent != nil {
		systemPrompt = agent.SystemPrompt
		userPrompt = agent.UserPrompt
		model = agent.Model
		maxTokens = agent.MaxTokens
		if agent.RAGTags != "" {
			for _, t := range strings.Split(agent.RAGTags, ",") {
				t = strings.TrimSpace(strings.ToLower(t))
				if t != "" {
					agentRAGTags = append(agentRAGTags, t)
				}
			}
		}
		ragEnabled = agent.RAGEnabled
		ragMaxResults = agent.RAGMaxResults
		ragCompressed = agent.RAGCompressed
		ragMaxTokens = agent.RAGMaxTokens
		ragEmbeddings = agent.RAGEmbeddings
		ragEmbeddingModel = agent.RAGEmbeddingModel
		toolsEnabled = agent.ToolsEnabled
		toolsMaxRounds = agent.ToolsMaxRounds
		toolTimeoutSec = agent.ToolTimeoutSec
		guardSettings = guardrails.SettingsFromAgent(agent)
	}

	// 4. Guardrail: check input (uses per-agent settings)
	if p.guard != nil {
		inResult := p.guard.CheckInput(chatID, text, guardSettings)
		if !inResult.Allowed {
			p.logger.Log("pipeline_guardrail_blocked", chatID, map[string]any{
				"direction": "input", "reason": inResult.Reason, "text": text,
			})
			return types.PipelineResult{Response: inResult.Reply, Blocked: true, Reason: inResult.Reason}
		}
	}

	// Inject contact name into system prompt (sanitize: keep only letters and spaces)
	if contactName != "" {
		clean := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsSpace(r) {
				return r
			}
			return -1
		}, contactName)
		clean = strings.Join(strings.Fields(clean), " ")
		if clean != "" {
			systemPrompt += "\n\n---\nNome do contato: " + clean
		}
	}

	// 5. Session management
	sessionID, isNew, oldSessionID := p.memory.GetOrCreateSession(chatID, timeoutMin)

	if isNew {
		p.logger.Log("pipeline_session_new", chatID, map[string]any{"session_id": sessionID, "reason": func() string {
			if oldSessionID == 0 {
				return "first"
			}
			return "timeout"
		}()})

		if oldSessionID != 0 {
			oldMsgs, err := p.memory.GetOldSessionMessages(chatID, oldSessionID)
			if err == nil && len(oldMsgs) > 0 {
				summary, err := p.ai.Summarize(model, oldMsgs)
				if err == nil {
					if saveErr := p.memory.SaveSummary(chatID, oldSessionID, summary); saveErr != nil {
						p.logger.Log("error", chatID, map[string]any{
							"source": "pipeline", "message": "save_summary: " + saveErr.Error(),
							"session_id": oldSessionID, "msg_count": len(oldMsgs),
						})
					} else {
						p.logger.Log("pipeline_session_summarized", chatID, map[string]any{
							"summary": summary, "old_session_id": oldSessionID, "msg_count": len(oldMsgs),
						})
					}
				} else {
					p.logger.Log("error", chatID, map[string]any{
						"source": "pipeline", "message": "summarize: " + err.Error(),
						"session_id": oldSessionID, "msg_count": len(oldMsgs),
					})
				}

				// Trigger knowledge extraction in background
				if agent != nil && agent.KnowledgeExtract && p.rag != nil {
					go p.extractKnowledge(chatID, model, oldMsgs, agent)
				}
			}
		}
	}

	// 6. Build context
	summary := p.memory.GetLatestSummary(chatID)
	history, err := p.memory.GetSessionHistory(chatID, sessionID, maxHistory)
	if err != nil {
		p.logger.Log("error", chatID, map[string]any{"source": "pipeline", "message": "get history: " + err.Error()})
		history = nil
	}

	// 7. RAG auto-search (when enabled on agent)
	var ragContext string
	var ragTokens int
	if ragEnabled {
		entries, err := p.rag.HybridSearch(chatID, p.ai, ragEmbeddingModel, text, ragMaxResults, ragEmbeddings, agentRAGTags...)
		if err != nil {
			p.logger.Log("error", chatID, map[string]any{"source": "pipeline", "message": "rag search: " + err.Error()})
		} else if len(entries) > 0 {
			ragContext = apprag.FormatContextFromEntriesWithBudget(entries, ragCompressed, ragMaxTokens)
			ragTokens = len(ragContext) / 3
			titles := make([]string, len(entries))
			for i, e := range entries {
				titles[i] = e.Title
			}
			p.logger.Log("pipeline_rag_search", chatID, map[string]any{
				"query": text, "results": len(entries), "titles": titles,
				"compressed": ragCompressed, "rag_tokens": ragTokens, "hybrid": ragEmbeddings,
			})
			if ragTokens == 0 {
				lengths := make([]int, len(entries))
				for i, e := range entries {
					lengths[i] = len(e.Content)
				}
				p.logger.Log("pipeline_rag_empty_context", chatID, map[string]any{
					"entries": len(entries), "content_lengths": lengths,
					"compressed": ragCompressed, "max_tokens": ragMaxTokens,
				})
			}
		}
	}

	// 8. Budget and compaction
	effectiveBudget := contextBudget - ragTokens
	if effectiveBudget < 200 {
		effectiveBudget = 200
	}

	if memory.EstimateTokens(history) > effectiveBudget && len(history) > 2 {
		mid := len(history) / 2
		older := history[:mid]
		history = history[mid:]

		compact, err := p.ai.Summarize(model, older)
		if err != nil {
			p.logger.Log("error", chatID, map[string]any{
				"source": "pipeline", "message": "compact: " + err.Error(),
				"session_id": sessionID, "msg_count": len(older),
			})
		} else {
			p.logger.Log("pipeline_context_compacted", chatID, map[string]any{
				"removed_msgs": len(older), "kept_msgs": len(history), "compact": compact,
			})
			if summary != "" {
				summary = summary + "\n" + compact
			} else {
				summary = compact
			}
		}
	}
	history = memory.TrimToTokenBudget(history, effectiveBudget)

	// 9. Log AI request
	tokensEst := memory.EstimateTokens(history) + ragTokens + len(text)/3 + len(systemPrompt)/3
	p.logger.Log("pipeline_ai_request", chatID, map[string]any{
		"tokens_est": tokensEst, "model": model, "max_tokens": maxTokens, "tools_enabled": toolsEnabled,
	})

	// 10. AI reply
	start := time.Now()
	var response string
	agentBaseURL, agentAPIKey := "", ""
	if agent != nil && agent.BaseURL != "" && agent.APIKey != "" {
		agentBaseURL = agent.BaseURL
		agentAPIKey = agent.APIKey
	}
	useAgentClient := agentBaseURL != "" && agentAPIKey != ""

	// Build tool executor with per-agent timeout
	execFn := func(name, cID, args string) (string, error) {
		return p.tools.ExecuteWithTimeout(name, cID, args, toolTimeoutSec)
	}

	if toolsEnabled && p.tools != nil {
		if useAgentClient {
			response, err = p.ai.ReplyWithToolsClient(
				agentBaseURL, agentAPIKey, model, maxTokens,
				systemPrompt, userPrompt, ragContext, summary,
				history, text,
				p.tools.GetTools(), execFn,
				chatID, toolsMaxRounds, p.logger,
			)
		} else {
			response, err = p.ai.ReplyWithTools(
				model, maxTokens,
				systemPrompt, userPrompt, ragContext, summary,
				history, text,
				p.tools.GetTools(), execFn,
				chatID, toolsMaxRounds, p.logger,
			)
		}
	} else {
		if useAgentClient {
			response, err = p.ai.ReplyWithClient(chatID, agentBaseURL, agentAPIKey, model, maxTokens, systemPrompt, userPrompt, ragContext, summary, history, text)
		} else {
			response, err = p.ai.Reply(chatID, model, maxTokens, systemPrompt, userPrompt, ragContext, summary, history, text)
		}
	}
	latency := time.Since(start).Milliseconds()

	if err != nil {
		p.logger.Log("error", chatID, map[string]any{"source": "pipeline", "message": "ai reply: " + err.Error()})
		return types.PipelineResult{Error: err}
	}

	p.logger.Log("pipeline_ai_response", chatID, map[string]any{
		"content": response, "tokens_est": len(response) / 4, "latency_ms": latency,
	})

	// 10b. Agent chaining: each chained agent uses its own settings
	const maxChainDepth = 3
	for depth := 0; depth < maxChainDepth && agent != nil && agent.ChainTo > 0; depth++ {
		if cond := agent.ChainCondition; cond != "" && !strings.Contains(response, cond) {
			break
		}
		nextAgent := p.tools.GetAgentByID(agent.ChainTo)
		if nextAgent == nil || nextAgent.Enabled == 0 {
			break
		}

		p.logger.Log("pipeline_agent_chain", chatID, map[string]any{
			"from_agent": agent.Name, "to_agent": nextAgent.Name, "depth": depth + 1,
		})

		chainedHistory := append(history, types.Message{Role: "assistant", Content: response})
		chainBaseURL, chainAPIKey := "", ""
		if nextAgent.BaseURL != "" && nextAgent.APIKey != "" {
			chainBaseURL = nextAgent.BaseURL
			chainAPIKey = nextAgent.APIKey
		}
		useChainClient := chainBaseURL != "" && chainAPIKey != ""

		// Per-agent settings for chained agent
		chainToolsEnabled := nextAgent.ToolsEnabled
		chainToolsMaxRounds := nextAgent.ToolsMaxRounds
		chainToolTimeoutSec := nextAgent.ToolTimeoutSec
		chainExecFn := func(name, cID, args string) (string, error) {
			return p.tools.ExecuteWithTimeout(name, cID, args, chainToolTimeoutSec)
		}

		var chainResp string
		var chainErr error
		chainStart := time.Now()
		if chainToolsEnabled && p.tools != nil {
			if useChainClient {
				chainResp, chainErr = p.ai.ReplyWithToolsClient(
					chainBaseURL, chainAPIKey, nextAgent.Model, nextAgent.MaxTokens,
					nextAgent.SystemPrompt, nextAgent.UserPrompt, ragContext, summary,
					chainedHistory, text,
					p.tools.GetTools(), chainExecFn,
					chatID, chainToolsMaxRounds, p.logger,
				)
			} else {
				chainResp, chainErr = p.ai.ReplyWithTools(
					nextAgent.Model, nextAgent.MaxTokens,
					nextAgent.SystemPrompt, nextAgent.UserPrompt, ragContext, summary,
					chainedHistory, text,
					p.tools.GetTools(), chainExecFn,
					chatID, chainToolsMaxRounds, p.logger,
				)
			}
		} else {
			if useChainClient {
				chainResp, chainErr = p.ai.ReplyWithClient(chatID, chainBaseURL, chainAPIKey, nextAgent.Model, nextAgent.MaxTokens, nextAgent.SystemPrompt, nextAgent.UserPrompt, ragContext, summary, chainedHistory, text)
			} else {
				chainResp, chainErr = p.ai.Reply(chatID, nextAgent.Model, nextAgent.MaxTokens, nextAgent.SystemPrompt, nextAgent.UserPrompt, ragContext, summary, chainedHistory, text)
			}
		}
		if chainErr != nil {
			p.logger.Log("error", chatID, map[string]any{"source": "pipeline", "message": "agent chain: " + chainErr.Error()})
			break
		}
		response = chainResp
		agent = nextAgent

		chainLatency := time.Since(chainStart).Milliseconds()
		p.logger.Log("pipeline_ai_response", chatID, map[string]any{
			"content": response, "tokens_est": len(response) / 4, "chained_from": agent.Name, "latency_ms": chainLatency,
		})
	}

	// 11. Guardrail: check output (uses per-agent settings — use last agent in chain)
	blocked := false
	reason := ""
	if p.guard != nil {
		outGuardSettings := guardSettings
		if agent != nil {
			outGuardSettings = guardrails.SettingsFromAgent(agent)
		}
		outResult := p.guard.CheckOutput(chatID, response, outGuardSettings)
		if !outResult.Allowed {
			p.logger.Log("pipeline_guardrail_blocked", chatID, map[string]any{
				"direction": "output", "reason": outResult.Reason, "text": response,
			})
			response = outResult.Reply
			blocked = true
			reason = outResult.Reason
		} else if outResult.Reply != "" {
			response = outResult.Reply
		}
	}

	// 12. Persist messages
	p.memory.SaveMessage(chatID, "user", text, sessionID)
	p.memory.SaveMessage(chatID, "assistant", response, sessionID)

	p.logger.Log("pipeline_msg_sent", chatID, map[string]any{"content": response})

	return types.PipelineResult{Response: response, Blocked: blocked, Reason: reason}
}

// extractKnowledge runs in background to extract knowledge from expired session messages.
func (p *Pipeline) extractKnowledge(chatID, model string, messages []types.Message, agent *types.Agent) {
	items, err := p.ai.ExtractKnowledge(model, messages)
	if err != nil {
		p.logger.Log("error", chatID, map[string]any{"source": "pipeline", "message": "extract_knowledge: " + err.Error()})
		return
	}
	if len(items) == 0 {
		return
	}

	inserted := 0
	for _, item := range items {
		if item.Title == "" || item.Content == "" {
			continue
		}
		if _, err := p.rag.AddEntry(item.Title, item.Content, "auto"); err != nil {
			p.logger.Log("error", chatID, map[string]any{
				"source": "pipeline", "message": fmt.Sprintf("extract_knowledge add entry: %s", err.Error()),
			})
			continue
		}
		inserted++
	}

	if inserted > 0 {
		p.logger.Log("pipeline_knowledge_extracted", chatID, map[string]any{
			"count": inserted, "agent": agent.Name,
		})
	}
}
