// Package provider talks to BYOK HTTP APIs and proxied agent CLIs.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mf9z/buggy/internal/config"
)

// Purpose tells a backend what the request is for. Agent CLIs get read-only
// project tools in chat and no tools during audits.
type Purpose int

const (
	PurposeChat Purpose = iota
	PurposeAudit
)

type Message struct {
	Role    string // "user" or "assistant"
	Content string
}

type Request struct {
	System   string
	Messages []Message
	Purpose  Purpose
	Dir      string
}

// Event carries reply text or a transient status such as "reading files".
type Event struct {
	Text   string
	Status string
}

type Provider interface {
	Name() string
	Model() string
	Stream(ctx context.Context, req Request, emit func(Event)) (string, error)
	Models(ctx context.Context) ([]string, error)
}

type Kind string

const (
	KindAnthropic Kind = "anthropic"
	KindOpenAI    Kind = "openai"
)

type Spec struct {
	ID           string
	Name         string
	Kind         Kind
	BaseURL      string
	EnvKey       string
	DefaultModel string
	// MaxTokens is sent as the output cap; 0 leaves it to the provider.
	MaxTokens   int
	KeyOptional bool
}

var Catalog = []Spec{
	{ID: "anthropic", Name: "Anthropic", Kind: KindAnthropic, BaseURL: "https://api.anthropic.com", EnvKey: "ANTHROPIC_API_KEY", DefaultModel: "claude-opus-5-5", MaxTokens: 32000},
	{ID: "openai", Name: "OpenAI", Kind: KindOpenAI, BaseURL: "https://api.openai.com/v1", EnvKey: "OPENAI_API_KEY", DefaultModel: "gpt-5"},
	{ID: "openrouter", Name: "OpenRouter", Kind: KindOpenAI, BaseURL: "https://openrouter.ai/api/v1", EnvKey: "OPENROUTER_API_KEY", DefaultModel: "openrouter/auto"},
	{ID: "nvidia", Name: "NVIDIA NIM", Kind: KindOpenAI, BaseURL: "https://integrate.api.nvidia.com/v1", EnvKey: "NVIDIA_API_KEY", DefaultModel: "meta/llama-3.3-70b-instruct", MaxTokens: 8192},
	{ID: "gemini", Name: "Google Gemini", Kind: KindOpenAI, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", EnvKey: "GEMINI_API_KEY", DefaultModel: "gemini-2.5-pro"},
	{ID: "groq", Name: "Groq", Kind: KindOpenAI, BaseURL: "https://api.groq.com/openai/v1", EnvKey: "GROQ_API_KEY", DefaultModel: "llama-3.3-70b-versatile", MaxTokens: 8192},
	{ID: "deepseek", Name: "DeepSeek", Kind: KindOpenAI, BaseURL: "https://api.deepseek.com/v1", EnvKey: "DEEPSEEK_API_KEY", DefaultModel: "deepseek-chat", MaxTokens: 8192},
	{ID: "mistral", Name: "Mistral", Kind: KindOpenAI, BaseURL: "https://api.mistral.ai/v1", EnvKey: "MISTRAL_API_KEY", DefaultModel: "mistral-large-latest"},
	{ID: "xai", Name: "xAI", Kind: KindOpenAI, BaseURL: "https://api.x.ai/v1", EnvKey: "XAI_API_KEY", DefaultModel: "grok-4"},
	{ID: "together", Name: "Together AI", Kind: KindOpenAI, BaseURL: "https://api.together.xyz/v1", EnvKey: "TOGETHER_API_KEY", DefaultModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo", MaxTokens: 8192},
	{ID: "ollama", Name: "Ollama (local)", Kind: KindOpenAI, BaseURL: "http://localhost:11434/v1", DefaultModel: "llama3.1", KeyOptional: true},
	{ID: "custom", Name: "Custom (OpenAI-compatible)", Kind: KindOpenAI, EnvKey: "BUGGY_API_KEY", KeyOptional: true},
}

var ProxyBackends = []string{"claude-code", "codex", "gemini-cli", "command", "http"}

func ProxyBackendLabel(id string) string {
	switch id {
	case "claude-code":
		return "Claude Code (uses your claude login)"
	case "codex":
		return "Codex CLI (uses your codex login)"
	case "gemini-cli":
		return "Gemini CLI (uses your gemini login)"
	case "command":
		return "Custom command (stdin → stdout)"
	case "http":
		return "HTTP proxy (OpenAI/Anthropic-compatible URL)"
	}
	return id
}

func LookupSpec(id string) (Spec, bool) {
	for _, s := range Catalog {
		if s.ID == id {
			return s, true
		}
	}
	return Spec{}, false
}

// ResolveKey prefers the configured key and falls back to the env var.
func ResolveKey(spec Spec, pc *config.ProviderConfig) (key, source string) {
	if pc != nil && pc.APIKey != "" {
		return pc.APIKey, "config"
	}
	if spec.EnvKey != "" {
		if v := os.Getenv(spec.EnvKey); v != "" {
			return v, "$" + spec.EnvKey
		}
	}
	return "", ""
}

func New(cfg *config.Config) (Provider, error) {
	if cfg.Mode == config.ModeProxy {
		return newProxy(cfg.Proxy)
	}
	spec, ok := LookupSpec(cfg.Provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
	pc := cfg.ProviderSettings(spec.ID)
	key, _ := ResolveKey(spec, pc)
	if key == "" && !spec.KeyOptional {
		hint := "open options and set an API key"
		if spec.EnvKey != "" {
			hint += " (or export " + spec.EnvKey + ")"
		}
		return nil, fmt.Errorf("no API key for %s: %s", spec.Name, hint)
	}
	base := firstNonEmpty(pc.BaseURL, spec.BaseURL)
	if base == "" {
		return nil, errors.New("custom provider needs a base URL; set one in options")
	}
	model := firstNonEmpty(pc.Model, spec.DefaultModel)
	if model == "" {
		return nil, fmt.Errorf("no model set for %s; set one in options", spec.Name)
	}
	return newHTTP(spec.ID, spec.Kind, base, key, model, spec.MaxTokens), nil
}

func newProxy(pc config.ProxyConfig) (Provider, error) {
	switch pc.Backend {
	case "claude-code", "codex", "gemini-cli":
		return &cliBridge{backend: pc.Backend, model: pc.Model}, nil
	case "command":
		if strings.TrimSpace(pc.Command) == "" {
			return nil, errors.New("proxy backend \"command\" needs a command; set one in options")
		}
		return &cliBridge{backend: "command", model: pc.Model, command: pc.Command}, nil
	case "http":
		if pc.URL == "" {
			return nil, errors.New("proxy backend \"http\" needs a URL; set one in options")
		}
		kind := KindOpenAI
		max := 0
		if pc.Format == "anthropic" {
			kind, max = KindAnthropic, 32000
		}
		model := pc.Model
		if model == "" {
			model = "default"
		}
		return newHTTP("proxy", kind, pc.URL, pc.APIKey, model, max), nil
	}
	return nil, fmt.Errorf("unknown proxy backend %q", pc.Backend)
}

func Describe(cfg *config.Config) string {
	if cfg.Mode == config.ModeProxy {
		m := cfg.Proxy.Model
		if m == "" {
			m = "default model"
		}
		return "proxy · " + cfg.Proxy.Backend + " · " + m
	}
	spec, _ := LookupSpec(cfg.Provider)
	pc := cfg.ProviderSettings(cfg.Provider)
	return "byok · " + cfg.Provider + " · " + firstNonEmpty(pc.Model, spec.DefaultModel, "no model")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Flatten renders a conversation as one prompt for single-input backends.
func Flatten(msgs []Message) string {
	if len(msgs) == 1 {
		return msgs[0].Content
	}
	var b strings.Builder
	b.WriteString("Conversation so far:\n\n")
	for _, m := range msgs[:len(msgs)-1] {
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n\n", m.Role, m.Content, m.Role)
	}
	b.WriteString("Reply to this latest user message:\n\n")
	b.WriteString(msgs[len(msgs)-1].Content)
	return b.String()
}
