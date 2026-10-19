<div align="center">

# buggy

**Find bugs, security holes and slow code in your project from the terminal.**

Bring your own API key, or proxy through a tool you're already logged into, like Claude Code.

[![CI](https://github.com/mf9z/buggy/actions/workflows/ci.yml/badge.svg)](https://github.com/mf9z/buggy/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/mf9z/buggy?logo=go&logoColor=white)](https://github.com/mf9z/buggy/blob/main/go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/mf9z/buggy)](https://goreportcard.com/report/github.com/mf9z/buggy)
[![Last Commit](https://img.shields.io/github/last-commit/mf9z/buggy)](https://github.com/mf9z/buggy/commits/main)
[![Stars](https://img.shields.io/github/stars/mf9z/buggy?style=flat)](https://github.com/mf9z/buggy/stargazers)

![Anthropic](https://img.shields.io/badge/Anthropic-191919?logo=anthropic&logoColor=white)
![OpenAI](https://img.shields.io/badge/OpenAI-412991?logo=openai&logoColor=white)
![OpenRouter](https://img.shields.io/badge/OpenRouter-6566F1)
![NVIDIA](https://img.shields.io/badge/NVIDIA-76B900?logo=nvidia&logoColor=white)
![Gemini](https://img.shields.io/badge/Gemini-8E75B2?logo=googlegemini&logoColor=white)
![Claude Code](https://img.shields.io/badge/Claude_Code-D97757?logo=claude&logoColor=white)

</div>

```
buggy
find the bugs before your users do

› Chat      ask about bugs, security and performance in your code
  Audit     scan this project and rank every issue by severity
  Options   providers, API keys, proxy and audit settings
  Exit
```

Findings are colored by severity: **red** for high, **orange** for medium,
**yellow** for low.

## Install

```sh
go install github.com/mf9z/buggy@latest
```

Or build from source:

```sh
git clone https://github.com/mf9z/buggy && cd buggy
make build        # ./buggy
```

Needs Go 1.26+ to build. The result is a single static binary with no runtime dependencies.

## Use

```sh
buggy                          # interactive menu in the current directory
buggy chat ~/code/api          # jump straight into chat
buggy audit .                  # audit and print the report
buggy audit src --out report.md --fail-on high   # CI: exit 1 on any high finding
buggy audit --json | jq '.findings[] | select(.severity=="high")'
```

In chat, `/add path/to/file` attaches code to your next message. `/help`
lists the other commands.

In audit results: `enter` expands a finding, `f`/`c` filter by severity or
category, `s` saves `buggy-report.md`, and `J` saves JSON.

## Backends

### BYOK (bring your own key)

| id           | provider                    | env var              |
|--------------|-----------------------------|----------------------|
| `anthropic`  | Anthropic                   | `ANTHROPIC_API_KEY`  |
| `openai`     | OpenAI                      | `OPENAI_API_KEY`     |
| `openrouter` | OpenRouter                  | `OPENROUTER_API_KEY` |
| `nvidia`     | NVIDIA NIM                  | `NVIDIA_API_KEY`     |
| `gemini`     | Google Gemini               | `GEMINI_API_KEY`     |
| `groq`       | Groq                        | `GROQ_API_KEY`       |
| `deepseek`   | DeepSeek                    | `DEEPSEEK_API_KEY`   |
| `mistral`    | Mistral                     | `MISTRAL_API_KEY`    |
| `xai`        | xAI                         | `XAI_API_KEY`        |
| `together`   | Together AI                 | `TOGETHER_API_KEY`   |
| `ollama`     | Ollama (local, no key)      |                      |
| `custom`     | any OpenAI-compatible URL   | `BUGGY_API_KEY`      |

Set keys in **Options**, or export the env var. Keys are stored in
`~/.config/buggy/config.json` with `0600` permissions. In Options, pressing
`enter` on **Model** fetches the provider's live model list.

### Proxy

Proxy mode sends requests through something you're already signed into, so
you can use a subscription instead of an API key.

| backend       | how it works |
|---------------|--------------|
| `claude-code` | runs `claude -p` headless. In chat it can explore your project with read-only tools (Read, Grep, Glob); in audits all tools are off |
| `codex`       | runs `codex exec` in a read-only sandbox |
| `gemini-cli`  | runs `gemini -p` in plan (read-only) mode |
| `command`     | any shell command: the prompt goes in on stdin and the reply comes out on stdout. `$BUGGY_MODEL` is set |
| `http`        | any OpenAI- or Anthropic-compatible proxy URL (LiteLLM, claude-code-proxy, a company gateway) |

```sh
buggy audit --proxy claude-code --model sonnet
```

## How audits work

1. Walks the project, honoring `.gitignore` and `.buggyignore`. It skips
   dependencies, build output, lockfiles, binaries, minified or generated
   files, and `.env` secrets (those are never sent).
2. Packs files into batches of about 96 KB (configurable) with line numbers in
   the gutter. Large files are split into line ranges.
3. Sends the batches in parallel (4 by default) and asks for strict JSON
   findings. If the reply isn't valid JSON, it retries once.
4. Normalizes, dedupes, filters and sorts the findings by severity.

## Flags

```
--provider ID   --model NAME   --proxy BACKEND   --key KEY
audit: --json  --out FILE  --fail-on SEV  --min SEV  --only security,performance,bugs  --concurrency N
```

`BUGGY_CONFIG` overrides the config file path.
