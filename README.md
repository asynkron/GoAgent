# GoAgent Runtime

A lightweight Go port of the OpenAgent orchestration loop. The runtime mirrors
the communication model of the upstream TypeScript implementation by exposing
separate input and output queues that are backed by Go channels.

## Requirements

Go 1.25 (tested with 1.25.1) or newer is required to build the runtime.

## Usage

```bash
go run ./cmd --model gpt-4.1
```

The binary reads prompts from `stdin` and prints runtime events to `stdout`. Each
prompt is currently echoed back to demonstrate the queue wiring. Type `cancel`
to simulate a cancel request or `exit` / `quit` to stop the runtime gracefully.

## Embedding the runtime

Applications embedding the runtime can import `internal/core/runtime` and access
the queues directly:

```go
rt, _ := runtime.NewRuntime(runtime.RuntimeOptions{
    APIKey:     "...",
    APIBaseURL: "https://api.openai.com/v1/chat/completions", // Optional override for self-hosted gateways.
})
go rt.Run(context.Background())

rt.SubmitPrompt("Hello")

for event := range rt.Outputs() {
fmt.Println(event.Type, event.Message)
}
```

Disable the built-in stdin/stdout bridges by setting
`DisableInputReader` and `DisableOutputForwarding` when the host wants
full control over queue processing.

### Configuration knobs

The runtime honours the following environment variables and flags:

* `OPENAI_API_KEY` (required) – API key used for all OpenAI requests.
* `OPENAI_MODEL` / `--model` – default model identifier.
* `OPENAI_REASONING_EFFORT` / `--reasoning-effort` – optional reasoning effort hint.
* `OPENAI_BASE_URL` / `--openai-base-url` – optional override for the Chat Completions endpoint, useful when routing traffic through a proxy or gateway.
