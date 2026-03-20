package guardrails

import (
	"regexp"
	"strings"

	"next/app/types"
	"next/internal/logger"
)

// GuardSettings holds per-agent guardrail configuration.
type GuardSettings struct {
	MaxInput, MaxOutput                       int
	BlockedInput, BlockedOutput               string
	PhoneList, PhoneMode                      string
	BlockInjection, BlockPII                  bool
	BlockPIIPhone, BlockPIIEmail, BlockPIICPF bool
}

// SettingsFromAgent extracts GuardSettings from an Agent.
func SettingsFromAgent(a *types.Agent) GuardSettings {
	return GuardSettings{
		MaxInput:       a.GuardMaxInput,
		MaxOutput:      a.GuardMaxOutput,
		BlockedInput:   a.GuardBlockedInput,
		BlockedOutput:  a.GuardBlockedOutput,
		PhoneList:      a.GuardPhoneList,
		PhoneMode:      a.GuardPhoneMode,
		BlockInjection: a.GuardBlockInjection,
		BlockPII:       a.GuardBlockPII,
		BlockPIIPhone:  a.GuardBlockPIIPhone,
		BlockPIIEmail:  a.GuardBlockPIIEmail,
		BlockPIICPF:    a.GuardBlockPIICPF,
	}
}

// Guardrails provides pre- and post-AI message filtering.
type Guardrails struct {
	logger   *logger.Logger
	piiPhone *regexp.Regexp
	piiEmail *regexp.Regexp
	piiCPF   *regexp.Regexp
}

// injectionPatterns are hardcoded prompt injection indicators (case-insensitive).
var injectionPatterns = []string{
	"ignore previous instructions",
	"ignore all instructions",
	"disregard previous",
	"forget your instructions",
	"new instructions:",
	"you are now",
	"system prompt:",
	"[system]",
}

// NewGuardrails creates a new Guardrails with compiled PII regexes.
func NewGuardrails(l *logger.Logger) *Guardrails {
	return &Guardrails{
		logger:   l,
		piiPhone: regexp.MustCompile(`(?:\+55\s?)?(?:\(?\d{2}\)?\s?)?\d{4,5}[-.\s]?\d{4}`),
		piiEmail: regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
		piiCPF:   regexp.MustCompile(`\d{3}[.\s]?\d{3}[.\s]?\d{3}[-.\s]?\d{2}`),
	}
}

// CheckInput runs pre-AI filters on incoming messages.
// Order: phone filter → max length → blocked patterns → injection.
func (g *Guardrails) CheckInput(chatID, text string, s GuardSettings) types.GuardrailResult {
	if g.logger != nil {
		g.logger.Log("guardrail_check_input", chatID, map[string]any{
			"text": text, "phone_mode": s.PhoneMode, "max_input": s.MaxInput,
			"block_injection": s.BlockInjection,
		})
	}

	// 1. Phone whitelist/blacklist
	if s.PhoneMode != "off" && s.PhoneList != "" {
		phones := splitList(s.PhoneList)
		found := containsPhone(phones, chatID)
		if s.PhoneMode == "whitelist" && !found {
			return types.GuardrailResult{Allowed: false, Reason: "phone_not_whitelisted"}
		}
		if s.PhoneMode == "blacklist" && found {
			return types.GuardrailResult{Allowed: false, Reason: "phone_blacklisted"}
		}
	}

	// 2. Max input length
	if s.MaxInput > 0 && len(text) > s.MaxInput {
		return types.GuardrailResult{
			Allowed: false,
			Reason:  "input_too_long",
			Reply:   "Mensagem muito longa. Por favor, envie uma mensagem mais curta.",
		}
	}

	// 3. Blocked input patterns (one per line)
	if s.BlockedInput != "" {
		lower := strings.ToLower(text)
		for _, pattern := range strings.Split(s.BlockedInput, "\n") {
			pattern = strings.TrimSpace(pattern)
			if pattern != "" && strings.Contains(lower, strings.ToLower(pattern)) {
				return types.GuardrailResult{
					Allowed: false,
					Reason:  "blocked_pattern",
					Reply:   "Nao posso ajudar com esse assunto.",
				}
			}
		}
	}

	// 4. Prompt injection detection
	if s.BlockInjection {
		lower := strings.ToLower(text)
		for _, pattern := range injectionPatterns {
			if strings.Contains(lower, pattern) {
				return types.GuardrailResult{
					Allowed: false,
					Reason:  "prompt_injection",
					Reply:   "Nao posso processar essa mensagem.",
				}
			}
		}
	}

	return types.GuardrailResult{Allowed: true}
}

// CheckOutput runs post-AI filters on the AI response.
// Order: truncate → blocked patterns → PII.
func (g *Guardrails) CheckOutput(chatID, text string, s GuardSettings) types.GuardrailResult {
	if g.logger != nil {
		g.logger.Log("guardrail_check_output", chatID, map[string]any{
			"text": text, "max_output": s.MaxOutput,
			"block_pii": s.BlockPII, "block_pii_phone": s.BlockPIIPhone,
			"block_pii_email": s.BlockPIIEmail, "block_pii_cpf": s.BlockPIICPF,
		})
	}

	result := text

	// 1. Max output length — truncate, don't block
	if s.MaxOutput > 0 && len(result) > s.MaxOutput {
		result = result[:s.MaxOutput] + "..."
	}

	// 2. Blocked output patterns
	if s.BlockedOutput != "" {
		lower := strings.ToLower(result)
		for _, pattern := range strings.Split(s.BlockedOutput, "\n") {
			pattern = strings.TrimSpace(pattern)
			if pattern != "" && strings.Contains(lower, strings.ToLower(pattern)) {
				return types.GuardrailResult{
					Allowed: false,
					Reason:  "blocked_output_pattern",
					Reply:   "Desculpe, nao posso fornecer essa informacao.",
				}
			}
		}
	}

	// 3. PII detection (phone, email, CPF) — granular or legacy master toggle
	{
		piiPhone := s.BlockPIIPhone || (s.BlockPII && !s.BlockPIIPhone && !s.BlockPIIEmail && !s.BlockPIICPF)
		piiEmail := s.BlockPIIEmail || (s.BlockPII && !s.BlockPIIPhone && !s.BlockPIIEmail && !s.BlockPIICPF)
		piiCPF := s.BlockPIICPF || (s.BlockPII && !s.BlockPIIPhone && !s.BlockPIIEmail && !s.BlockPIICPF)

		var detected []string
		if piiPhone && g.piiPhone.MatchString(result) {
			detected = append(detected, "telefone")
		}
		if piiEmail && g.piiEmail.MatchString(result) {
			detected = append(detected, "email")
		}
		if piiCPF && g.piiCPF.MatchString(result) {
			detected = append(detected, "CPF")
		}
		if len(detected) > 0 {
			return types.GuardrailResult{
				Allowed: false,
				Reason:  "pii_detected",
				Reply:   "Desculpe, nao posso compartilhar informacoes pessoais (" + strings.Join(detected, ", ") + ").",
			}
		}
	}

	// Truncated but allowed
	if result != text {
		return types.GuardrailResult{Allowed: true, Reply: result}
	}

	return types.GuardrailResult{Allowed: true}
}

// splitList splits a comma-separated list into trimmed, non-empty strings.
func splitList(s string) []string {
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

// containsPhone checks if the phone matches any entry in the list (suffix match).
func containsPhone(list []string, phone string) bool {
	for _, p := range list {
		if p == phone || strings.HasSuffix(phone, p) || strings.HasSuffix(p, phone) {
			return true
		}
	}
	return false
}
