package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mf9z/buggy/internal/config"
)

func collect(t *testing.T, p Provider, req Request) (string, string) {
	t.Helper()
	var streamed strings.Builder
	out, err := p.Stream(context.Background(), req, func(e Event) { streamed.WriteString(e.Text) })
	if err != nil {
		t.Fatal(err)
	}
	return out, streamed.String()
}

func TestAnthropicStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad request "+r.URL.Path, 400)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["system"] != "sys" || body["stream"] != true {
			http.Error(w, "bad body", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"thinking\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	p := newHTTP("anthropic", KindAnthropic, srv.URL, "k", "m", 1000)
	out, streamed := collect(t, p, Request{System: "sys", Messages: []Message{{"user", "hi"}}})
	if out != "hello" || streamed != "hello" {
		t.Fatalf("out=%q streamed=%q", out, streamed)
	}
}

func TestOpenAIStreamAndRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":{"message":"slow down"}}`, 429)
			return
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" {
			http.Error(w, "bad messages", 400)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"o\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"k\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := newHTTP("openai", KindOpenAI, srv.URL+"/v1", "k", "m", 0)
	out, _ := collect(t, p, Request{System: "s", Messages: []Message{{"user", "hi"}}})
	if out != "ok" || hits.Load() != 2 {
		t.Fatalf("out=%q hits=%d", out, hits.Load())
	}
}

func TestAPIErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, 401)
	}))
	defer srv.Close()
	p := newHTTP("anthropic", KindAnthropic, srv.URL, "bad", "m", 1000)
	_, err := p.Stream(context.Background(), Request{Messages: []Message{{"user", "hi"}}}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseClaudeStream(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Let me look."}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Read"}},"parent_tool_use_id":null}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Let me look."}]},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"sub"}},"parent_tool_use_id":"toolu_1"}`,
		`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"[HIGH] bug"}},"parent_tool_use_id":null}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"[HIGH] bug"}`,
	}
	var statuses []string
	out, err := parseClaudeStream(strings.NewReader(strings.Join(lines, "\n")), func(e Event) {
		if e.Status != "" {
			statuses = append(statuses, e.Status)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Let me look.\n\n[HIGH] bug" {
		t.Fatalf("out = %q", out)
	}
	if len(statuses) != 1 || statuses[0] != "reading files" {
		t.Fatalf("statuses = %v", statuses)
	}

	_, err = parseClaudeStream(strings.NewReader(`{"type":"result","subtype":"error","is_error":true,"result":"Not logged in"}`), func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommandBridge(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeProxy
	cfg.Proxy.Backend = "command"
	cfg.Proxy.Command = "tr a-z A-Z"
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := collect(t, p, Request{System: "sys", Messages: []Message{{"user", "hello"}}})
	if !strings.Contains(out, "HELLO") || !strings.Contains(out, "<INSTRUCTIONS>") {
		t.Fatalf("out = %q", out)
	}
}

func TestNewRequiresKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := config.Default()
	cfg.Provider = "openai"
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-test")
	p, err := New(cfg)
	if err != nil || p.Model() != "gpt-5" {
		t.Fatalf("p=%v err=%v", p, err)
	}
}

func TestFlatten(t *testing.T) {
	if Flatten([]Message{{"user", "x"}}) != "x" {
		t.Fatal("single message should pass through")
	}
	s := Flatten([]Message{{"user", "a"}, {"assistant", "b"}, {"user", "c"}})
	if !strings.Contains(s, "<assistant>\nb\n</assistant>") || !strings.HasSuffix(s, "c") {
		t.Fatalf("flatten = %q", s)
	}
}
