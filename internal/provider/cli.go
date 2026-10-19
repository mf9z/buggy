package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// cliBridge runs an agent CLI the user is already logged into, so they can
// use a subscription instead of an API key.
type cliBridge struct {
	backend string
	model   string
	command string
}

func (c *cliBridge) Name() string { return c.backend }

func (c *cliBridge) Model() string {
	if c.model == "" {
		return "default"
	}
	return c.model
}

const readOnlyTools = "Read,Grep,Glob"

func (c *cliBridge) Models(ctx context.Context) ([]string, error) {
	switch c.backend {
	case "claude-code":
		return []string{"opus", "sonnet", "haiku", "fable", "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1"}, nil
	case "gemini-cli":
		return []string{"gemini-2.5-pro", "gemini-2.5-flash"}, nil
	}
	return nil, errors.New("this backend does not list models; type a model name instead")
}

func (c *cliBridge) Stream(ctx context.Context, req Request, emit func(Event)) (string, error) {
	prompt := Flatten(req.Messages)
	var cmd *exec.Cmd
	switch c.backend {
	case "claude-code":
		bin, err := lookPath("claude", "Claude Code is not installed (https://claude.com/claude-code)")
		if err != nil {
			return "", err
		}
		args := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--no-session-persistence"}
		if c.model != "" {
			args = append(args, "--model", c.model)
		}
		if req.Purpose == PurposeAudit {
			// Audits pass the code inline; tools would only slow it down.
			args = append(args, "--tools", "", "--system-prompt", req.System)
		} else {
			args = append(args, "--tools", readOnlyTools, "--allowedTools", readOnlyTools, "--append-system-prompt", req.System)
		}
		cmd = exec.CommandContext(ctx, bin, args...)
	case "codex":
		bin, err := lookPath("codex", "Codex CLI is not installed (npm i -g @openai/codex)")
		if err != nil {
			return "", err
		}
		args := []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", "read-only"}
		if c.model != "" {
			args = append(args, "-m", c.model)
		}
		args = append(args, "-")
		cmd = exec.CommandContext(ctx, bin, args...)
		prompt = withSystem(req.System, prompt)
	case "gemini-cli":
		bin, err := lookPath("gemini", "Gemini CLI is not installed (npm i -g @google/gemini-cli)")
		if err != nil {
			return "", err
		}
		args := []string{"-p", "Follow the instructions given above.", "--approval-mode", "plan", "--skip-trust", "-o", "text"}
		if c.model != "" {
			args = append(args, "-m", c.model)
		}
		cmd = exec.CommandContext(ctx, bin, args...)
		prompt = withSystem(req.System, prompt)
	case "command":
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd", "/C", c.command)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", c.command)
		}
		cmd.Env = append(cmd.Environ(), "BUGGY_MODEL="+c.model)
		prompt = withSystem(req.System, prompt)
	default:
		return "", fmt.Errorf("unknown proxy backend %q", c.backend)
	}
	if req.Dir != "" {
		if abs, err := filepath.Abs(req.Dir); err == nil {
			cmd.Dir = abs
		}
	}
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%s: %w", c.backend, err)
	}

	var out string
	var perr error
	switch c.backend {
	case "claude-code":
		out, perr = parseClaudeStream(stdout, emit)
	case "codex":
		out, perr = parseCodexStream(stdout, emit)
	default:
		out, perr = copyRaw(stdout, emit)
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if perr != nil {
		return out, perr
	}
	if werr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = werr.Error()
		}
		return out, fmt.Errorf("%s: %s", c.backend, lastLines(msg, 6))
	}
	if strings.TrimSpace(out) == "" {
		return out, fmt.Errorf("%s returned an empty reply", c.backend)
	}
	return out, nil
}

func withSystem(system, prompt string) string {
	if system == "" {
		return prompt
	}
	return "<instructions>\n" + system + "\n</instructions>\n\n" + prompt
}

func lookPath(name, hint string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%q not found on PATH: %s", name, hint)
	}
	return p, nil
}

func parseClaudeStream(r io.Reader, emit func(Event)) (string, error) {
	var out strings.Builder
	var result string
	sawDelta := false
	err := scanLines(r, func(line []byte) error {
		var ev struct {
			Type            string          `json:"type"`
			Subtype         string          `json:"subtype"`
			ParentToolUseID *string         `json:"parent_tool_use_id"`
			Event           json.RawMessage `json:"event"`
			Message         struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return nil
		}
		if ev.ParentToolUseID != nil && *ev.ParentToolUseID != "" {
			return nil // subagent chatter
		}
		switch ev.Type {
		case "stream_event":
			var se struct {
				Type         string `json:"type"`
				ContentBlock struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content_block"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if json.Unmarshal(ev.Event, &se) != nil {
				return nil
			}
			switch se.Type {
			case "message_start":
				if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n\n") {
					out.WriteString("\n\n")
					emit(Event{Text: "\n\n"})
				}
			case "content_block_start":
				switch se.ContentBlock.Type {
				case "tool_use":
					emit(Event{Status: toolStatus(se.ContentBlock.Name)})
				case "thinking":
					emit(Event{Status: "thinking"})
				}
			case "content_block_delta":
				if se.Delta.Type == "text_delta" && se.Delta.Text != "" {
					sawDelta = true
					out.WriteString(se.Delta.Text)
					emit(Event{Text: se.Delta.Text})
				}
			}
		case "assistant":
			if sawDelta {
				return nil
			}
			for _, c := range ev.Message.Content {
				if c.Type == "text" && c.Text != "" {
					out.WriteString(c.Text)
					emit(Event{Text: c.Text})
				}
			}
		case "result":
			if ev.IsError {
				msg := ev.Result
				if msg == "" {
					msg = ev.Subtype
				}
				return fmt.Errorf("claude-code: %s", msg)
			}
			result = ev.Result
		}
		return nil
	})
	if out.Len() == 0 && result != "" {
		emit(Event{Text: result})
		return result, err
	}
	return out.String(), err
}

func toolStatus(name string) string {
	switch name {
	case "Read":
		return "reading files"
	case "Grep":
		return "searching code"
	case "Glob":
		return "listing files"
	}
	return "using " + name
}

func parseCodexStream(r io.Reader, emit func(Event)) (string, error) {
	var out strings.Builder
	err := scanLines(r, func(line []byte) error {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Command string `json:"command"`
			} `json:"item"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "item.started":
			switch ev.Item.Type {
			case "command_execution":
				emit(Event{Status: "running " + ev.Item.Command})
			case "reasoning":
				emit(Event{Status: "thinking"})
			}
		case "item.completed":
			if ev.Item.Type == "agent_message" && ev.Item.Text != "" {
				if out.Len() > 0 {
					out.WriteString("\n\n")
					emit(Event{Text: "\n\n"})
				}
				out.WriteString(ev.Item.Text)
				emit(Event{Text: ev.Item.Text})
			}
		case "turn.failed":
			return fmt.Errorf("codex: %s", ev.Error.Message)
		case "error":
			return fmt.Errorf("codex: %s", ev.Message)
		}
		return nil
	})
	return out.String(), err
}

func copyRaw(r io.Reader, emit func(Event)) (string, error) {
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s := string(buf[:n])
			out.WriteString(s)
			emit(Event{Text: s})
		}
		if err == io.EOF {
			return out.String(), nil
		}
		if err != nil {
			return out.String(), err
		}
	}
}

func scanLines(r io.Reader, fn func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() {
		if err := fn(sc.Bytes()); err != nil {
			// Drain so the child process doesn't block on a full pipe.
			io.Copy(io.Discard, r)
			return err
		}
	}
	return sc.Err()
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	l.w.Write(q)
	return len(p), nil
}
