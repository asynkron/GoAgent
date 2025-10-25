# GoAgent Runtime

A lightweight Go port of the OpenAgent orchestration loop. The runtime mirrors
the communication model of the upstream TypeScript implementation by exposing
separate input and output queues that are backed by Go channels.

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
rt, _ := runtime.NewRuntime(runtime.RuntimeOptions{APIKey: "..."})
go rt.Run(context.Background())

rt.SubmitPrompt("Hello")

for event := range rt.Outputs() {
fmt.Println(event.Type, event.Message)
}
```

Disable the built-in stdin/stdout bridges by setting
`DisableInputReader` and `DisableOutputForwarding` when the host wants
full control over queue processing.
