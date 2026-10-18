package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var client = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	},
}

// httpProvider speaks either the Anthropic Messages API or the OpenAI chat
// completions API (which covers most other vendors).
type httpProvider struct {
	name      string
	kind      Kind
	base      string
	key       string
	model     string
	maxTokens int
}

func newHTTP(name string, kind Kind, base, key, model string, maxTokens int) *httpProvider {
	return &httpProvider{name: name, kind: kind, base: strings.TrimRight(base, "/"), key: key, model: model, maxTokens: maxTokens}
}

func (p *httpProvider) Name() string  { return p.name }
func (p *httpProvider) Model() string { return p.model }

func (p *httpProvider) Stream(ctx context.Context, req Request, emit func(Event)) (string, error) {
	if p.kind == KindAnthropic {
		return p.streamAnthropic(ctx, req, emit)
	}
	return p.streamOpenAI(ctx, req, emit)
}

func (p *httpProvider) anthropicURL(path string) string {
	// Accept both "https://api.anthropic.com" and ".../v1" as the base.
	if strings.HasSuffix(p.base, "/v1") {
		return p.base + path
	}
	return p.base + "/v1" + path
}

func (p *httpProvider) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "buggy-cli")
	if p.kind == KindAnthropic {
		r.Header.Set("anthropic-version", "2023-06-01")
		if p.key != "" {
			r.Header.Set("x-api-key", p.key)
		}
		return
	}
	if p.key != "" {
		r.Header.Set("Authorization", "Bearer "+p.key)
	}
	if p.name == "openrouter" {
		r.Header.Set("HTTP-Referer", "https://github.com/rohan/buggy")
		r.Header.Set("X-Title", "buggy")
	}
}

func (p *httpProvider) streamAnthropic(ctx context.Context, req Request, emit func(Event)) (string, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{
		"model":      p.model,
		"max_tokens": p.maxTokens,
		"stream":     true,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	msgs := make([]msg, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, msg{m.Role, m.Content})
	}
	body["messages"] = msgs

	resp, err := p.post(ctx, p.anthropicURL("/messages"), body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out strings.Builder
	err = readSSE(resp.Body, func(data []byte) (bool, error) {
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return false, nil
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "thinking" {
				emit(Event{Status: "thinking"})
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				out.WriteString(ev.Delta.Text)
				emit(Event{Text: ev.Delta.Text})
			}
		case "message_delta":
			if ev.Delta.StopReason == "refusal" {
				return true, fmt.Errorf("the model declined this request")
			}
		case "message_stop":
			return true, nil
		case "error":
			return true, fmt.Errorf("anthropic: %s", ev.Error.Message)
		}
		return false, nil
	})
	return out.String(), err
}

func (p *httpProvider) streamOpenAI(ctx context.Context, req Request, emit func(Event)) (string, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	msgs := make([]msg, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, msg{"system", req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, msg{m.Role, m.Content})
	}
	body := map[string]any{
		"model":    p.model,
		"messages": msgs,
		"stream":   true,
	}
	if p.maxTokens > 0 {
		body["max_tokens"] = p.maxTokens
	}

	resp, err := p.post(ctx, p.base+"/chat/completions", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out strings.Builder
	thinking := false
	err = readSSE(resp.Body, func(data []byte) (bool, error) {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return true, nil
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return false, nil
		}
		if ev.Error != nil {
			return true, fmt.Errorf("%s: %s", p.name, ev.Error.Message)
		}
		for _, c := range ev.Choices {
			if !thinking && (c.Delta.ReasoningContent != "" || c.Delta.Reasoning != "") {
				thinking = true
				emit(Event{Status: "thinking"})
			}
			if c.Delta.Content != "" {
				out.WriteString(c.Delta.Content)
				emit(Event{Text: c.Delta.Content})
			}
		}
		return false, nil
	})
	return out.String(), err
}

// post sends a JSON request, retrying rate limits and transient server errors
// with backoff before any of the response has been consumed.
func (p *httpProvider) post(ctx context.Context, url string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		p.setHeaders(r)
		r.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(r)
		if err != nil {
			if ctx.Err() != nil || attempt >= 3 {
				return nil, err
			}
		} else if resp.StatusCode == http.StatusOK {
			return resp, nil
		} else {
			apiErr := readAPIError(p.name, resp)
			if !retryable(resp.StatusCode) || attempt >= 3 {
				return nil, apiErr
			}
			if ra := retryAfter(resp); ra > 0 {
				backoff = ra
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

func retryable(code int) bool {
	return code == 408 || code == 409 || code == 429 || code >= 500
}

func retryAfter(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 120 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

func readAPIError(name string, resp *http.Response) error {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error json.RawMessage `json:"error"`
		Message string        `json:"message"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &e) == nil {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &inner) == nil && inner.Message != "" {
			msg = inner.Message
		} else if s, err := strconv.Unquote(string(e.Error)); err == nil && s != "" {
			msg = s
		} else if e.Message != "" {
			msg = e.Message
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	switch resp.StatusCode {
	case 401, 403:
		return fmt.Errorf("%s: authentication failed (%d): %s — check your API key", name, resp.StatusCode, msg)
	case 404:
		return fmt.Errorf("%s: not found (404): %s — check the model name and base URL", name, msg)
	}
	return fmt.Errorf("%s: HTTP %d: %s", name, resp.StatusCode, msg)
}

// readSSE parses a server-sent-events stream, calling fn with each data
// payload. fn returns done=true to stop early.
func readSSE(r io.Reader, fn func(data []byte) (done bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var data []byte
	flush := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		d := data
		data = nil
		return fn(d)
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if done, err := flush(); done || err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			chunk := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	_, err := flush()
	return err
}

func (p *httpProvider) Models(ctx context.Context) ([]string, error) {
	url := p.base + "/models"
	if p.kind == KindAnthropic {
		url = p.anthropicURL("/models?limit=1000")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	p.setHeaders(r)
	resp, err := client.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(p.name, resp)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	for _, m := range list.Models {
		ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
	}
	sort.Strings(ids)
	return ids, nil
}
