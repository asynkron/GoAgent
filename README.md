# GoAgent Runtime

A lightweight Go port of the OpenAgent orchestration loop. The runtime mirrors the communication model of the upstream TypeScript implementation by exposing separate input and output queues backed by Go channels.

## Requirements

- Go 1.25 (tested with 1.25.1 or newer)

## Quick start

Run the CLI with your preferred model:

```bash
go run ./cmd --model gpt-4.1
```

You can also pass a one-off prompt and see streaming assistant deltas immediately:

```bash
OPENAI_API_KEY=sk-... go run ./cmd -prompt "Say hello in 3 words"
```

## HTTP SSE streaming example

This repo includes a minimal SSE server that streams assistant tokens in real time.

Run the server:

```bash
OPENAI_API_KEY=sk-... go run ./cmd/sse
```

Then in another shell, test streaming with curl (you should see data lines appear incrementally):

```bash
curl -N "http://localhost:8080/stream?q=Write%20a%20haiku%20about%20autumn"
```

The runtime emits two kinds of assistant events:

- `assistant_delta`: the streaming chunks; these arrive token-by-token.
- `assistant_message`: the final consolidated content at the end of the stream.

SSE server requirements to avoid buffering:

- Set headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`.
- Use `http.Flusher` and call `Flush()` after each event write.
- Do not wrap `ResponseWriter` with gzip or other buffering middleware.

If you run behind nginx or similar proxies, ensure buffering is disabled and HTTP/1.1 is used end-to-end:

- `proxy_http_version 1.1;`
- `proxy_set_header Connection "keep-alive";`
- `proxy_buffering off;`
- `chunked_transfer_encoding on;` (or leave default if supported).

In browsers, prefer `EventSource` or a streaming `fetch()` reader to consume tokens incrementally.

## Hands-free research mode

Run the agent in a hands-free loop with an overarching goal and a fixed number of turns. The agent will auto‑reply when it requests human input so it continues working toward the goal:

```bash
OPENAI_API_KEY=sk-... go run ./cmd --research '{"goal":"try to find any race-condition bugs in this codebase","turns":20}'
```

This sets the initial goal prompt, caps execution at 20 passes, and auto‑responds to any input requests with a message like:

```text
Please continue to work on the set goal. No human available. Goal: try to find any race-condition bugs in this codebase
```

### Exit codes and output in hands-free mode

- Success (goal completed or no further steps):
  - Exit code: 0
  - STDOUT: final assistant message
- Failure (turn budget reached without a solution or runtime error):
  - Exit code: non‑zero
  - STDERR: final assistant message or an explanatory error

## Embedding the runtime

Applications embedding the runtime can import `internal/core/runtime` and access the queues directly:

```go
rt, _ := runtime.NewRuntime(runtime.RuntimeOptions{
    APIKey:     "...",
    APIBaseURL: "https://api.openai.com/v1", // Optional override for self-hosted gateways.
})
go rt.Run(context.Background())

rt.SubmitPrompt("Hello")

for event := range rt.Outputs() {
    fmt.Println(event.Type, event.Message)
}
```

Disable the built-in stdin/stdout bridges by setting `DisableInputReader` and `DisableOutputForwarding` when the host wants full control over queue processing.

## Configuration knobs

The runtime honours the following environment variables and flags:

- `OPENAI_API_KEY` (required) – API key used for all OpenAI requests.
- `OPENAI_MODEL` / `--model` – default model identifier. (Default may be `gpt-5` depending on your environment.)
- `OPENAI_REASONING_EFFORT` / `--reasoning-effort` – optional reasoning effort hint (`low`, `medium`, `high`).
- `OPENAI_BASE_URL` / `--openai-base-url` – optional override for the OpenAI API base URL (e.g., https://api.openai.com/v1), useful when routing through a proxy or gateway.
