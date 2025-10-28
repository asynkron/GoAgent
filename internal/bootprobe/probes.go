package bootprobe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Result BootProbeResult mirrors the structure returned by the upstream TypeScript
// implementation and captures the detected capabilities of the current project
// and execution environment.
type Result struct {
	Node       *NodeProbeResult
	Python     *PythonProbeResult
	DotNet     *SimpleProbeResult
	Go         *SimpleProbeResult
	Rust       *RustProbeResult
	JVM        *JVMProbeResult
	Git        *SimpleProbeResult
	Containers []ContainerProbeResult
	Linters    []ToolingProbeResult
	Formatters []ToolingProbeResult
	OS         OSResult
}

// CommandStatus records whether a particular command is available on PATH.
type CommandStatus struct {
	Name      string
	Available bool
}

// SimpleProbeResult captures a boolean detection and supporting indicators for
// a tooling family.
type SimpleProbeResult struct {
	Detected   bool
	Indicators []string
	Commands   []CommandStatus
}

// NodeProbeResult captures information about a JavaScript/TypeScript project.
type NodeProbeResult struct {
	Detected        bool
	Indicators      []string
	Commands        []CommandStatus
	HasTypeScript   bool
	HasJavaScript   bool
	PackageManagers []string
}

// PythonProbeResult captures Python specific metadata.
type PythonProbeResult struct {
	Detected   bool
	Indicators []string
	Commands   []CommandStatus
	UsesPoetry bool
	UsesPipenv bool
}

// RustProbeResult captures Rust specific metadata.
type RustProbeResult struct {
	Detected   bool
	Indicators []string
	Commands   []CommandStatus
}

// JVMProbeResult captures information about JVM build tooling.
type JVMProbeResult struct {
	Detected   bool
	Indicators []string
	Commands   []CommandStatus
	BuildTools []string
}

// ContainerProbeResult describes container configuration or tooling.
type ContainerProbeResult struct {
	Detected   bool
	Indicators []string
	Commands   []CommandStatus
	Runtime    string
}

// ToolingProbeResult captures formatter or linter tools.
type ToolingProbeResult struct {
	Name       string
	Indicators []string
	Commands   []CommandStatus
}

// OSResult summarises the host operating system and architecture.
type OSResult struct {
	GOOS         string
	GOARCH       string
	Distribution string
}

// Run executes all boot probes and returns a consolidated result structure.
func Run(ctx *Context) Result {
	return Result{
		Node:       runNodeProbe(ctx),
		Python:     runPythonProbe(ctx),
		DotNet:     runDotNetProbe(ctx),
		Go:         runGoProbe(ctx),
		Rust:       runRustProbe(ctx),
		JVM:        runJVMProbe(ctx),
		Git:        runGitProbe(ctx),
		Containers: runContainerProbes(ctx),
		Linters:    runLintProbes(ctx),
		Formatters: runFormatterProbes(ctx),
		OS:         detectOS(),
	}
}

func runNodeProbe(ctx *Context) *NodeProbeResult {
	indicators := collectExistingFiles(ctx, []string{
		"package.json",
		"pnpm-workspace.yaml",
		"yarn.lock",
		"package-lock.json",
		"tsconfig.json",
		"tsconfig.base.json",
		"jsconfig.json",
	})

	hasTSFile := false
	hasJSFile := false
	if _, ok := ctx.FindFirstWithSuffix(".ts", ".tsx"); ok {
		hasTSFile = true
	}
	if _, ok := ctx.FindFirstWithSuffix(".js", ".jsx", ".mjs", ".cjs"); ok {
		hasJSFile = true
	}

	commands := commandStatuses(ctx, "node", "npm", "pnpm", "yarn", "npx")
	pkgManagers := availableCommandNames(commands, "npm", "pnpm", "yarn")

	detected := len(indicators) > 0 || hasTSFile || hasJSFile
	if !detected {
		return nil
	}

	if hasTSFile {
		indicators = append(indicators, "TypeScript sources")
	}
	if hasJSFile {
		indicators = append(indicators, "JavaScript sources")
	}

	return &NodeProbeResult{
		Detected:        true,
		Indicators:      dedupeStrings(indicators),
		Commands:        commands,
		HasTypeScript:   hasTSFile,
		HasJavaScript:   hasJSFile,
		PackageManagers: pkgManagers,
	}
}

func runPythonProbe(ctx *Context) *PythonProbeResult {
	indicators := collectExistingFiles(ctx, []string{
		"pyproject.toml",
		"requirements.txt",
		"requirements-dev.txt",
		"Pipfile",
		"setup.cfg",
		"setup.py",
		"environment.yml",
	})

	usesPoetry := ctx.HasFile("poetry.lock")
	usesPipenv := ctx.HasAnyFile("Pipfile", "Pipfile.lock")
	if usesPoetry {
		indicators = append(indicators, "poetry.lock")
	}
	if usesPipenv {
		indicators = append(indicators, "Pipenv files")
	}

	commands := commandStatuses(ctx, "python3", "python", "pip", "poetry", "pipenv")
	if len(indicators) == 0 {
		return nil
	}

	return &PythonProbeResult{
		Detected:   true,
		Indicators: dedupeStrings(indicators),
		Commands:   commands,
		UsesPoetry: usesPoetry,
		UsesPipenv: usesPipenv,
	}
}

func runDotNetProbe(ctx *Context) *SimpleProbeResult {
	var indicators []string
	if path, ok := ctx.FindFirstWithSuffix(".csproj", ".fsproj"); ok {
		indicators = append(indicators, filepath.Base(path))
	}
	if ctx.HasAnyFile("global.json", "Directory.Build.props", "Directory.Build.targets") {
		indicators = append(indicators, "SDK configuration")
	}
	commands := commandStatuses(ctx, "dotnet")
	if len(indicators) == 0 {
		return nil
	}

	return &SimpleProbeResult{
		Detected:   true,
		Indicators: dedupeStrings(indicators),
		Commands:   commands,
	}
}

func runGoProbe(ctx *Context) *SimpleProbeResult {
	indicators := collectExistingFiles(ctx, []string{"go.mod", "go.sum", "go.work"})
	// Check for common Go-related commands beyond just `go`.
	commands := commandStatuses(ctx, "go", "gofmt", "goimports", "golangci-lint", "staticcheck")
	if len(indicators) == 0 {
		return nil
	}

	// Try to capture `go version`.
	if ctx.CommandExists("go") {
		if out, err := ctx.RunCommandOutput("go", "version"); err == nil {
			ver := strings.TrimSpace(out)
			if ver != "" {
				indicators = append(indicators, "go version: "+ver)
			}
		}
	}

	// Parse toolchain directive from go.mod if present (Go 1.21+ feature).
	if ctx.HasFile("go.mod") {
		if data, err := ctx.ReadFile("go.mod"); err == nil {
			if tc := parseGoToolchain(string(data)); tc != "" {
				indicators = append(indicators, "toolchain: "+tc)
			}
		}
	}

	// Query `go env -json` for key environment values.
	if ctx.CommandExists("go") {
		if out, err := ctx.RunCommandOutput("go", "env", "-json"); err == nil {
			type goEnv struct {
				GOPATH     string `json:"GOPATH"`
				GOROOT     string `json:"GOROOT"`
				GOMODCACHE string `json:"GOMODCACHE"`
			}
			var env goEnv
			if jsonErr := json.Unmarshal([]byte(out), &env); jsonErr == nil {
				if env.GOROOT != "" {
					indicators = append(indicators, "GOROOT="+env.GOROOT)
				}
				if env.GOPATH != "" {
					indicators = append(indicators, "GOPATH="+env.GOPATH)
				}
				if env.GOMODCACHE != "" {
					indicators = append(indicators, "GOMODCACHE="+env.GOMODCACHE)
				}
			}
		}
	}

	return &SimpleProbeResult{
		Detected:   true,
		Indicators: dedupeStrings(indicators),
		Commands:   commands,
	}
}

// parseGoToolchain extracts the value of the `toolchain` directive from a go.mod
// file content. It returns an empty string if not present.
func parseGoToolchain(modFile string) string {
	// A very small and robust parser: scan lines and look for a line starting
	// with "toolchain" followed by the toolchain string.
	// Examples:
	//   toolchain go1.22.3
	//   toolchain golang.org/toolchain@v0.0.1-go1.22.0
	if modFile == "" {
		return ""
	}
	lines := strings.Split(modFile, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "toolchain ") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "toolchain "))
			// Strip optional trailing comments.
			if idx := strings.IndexAny(value, "\t #"); idx >= 0 {
				value = strings.TrimSpace(value[:idx])
			}
			return value
		}
	}
	return ""
}

func runRustProbe(ctx *Context) *RustProbeResult {
	indicators := collectExistingFiles(ctx, []string{"Cargo.toml", "Cargo.lock"})
	commands := commandStatuses(ctx, "cargo", "rustc")
	if len(indicators) == 0 {
		return nil
	}
	return &RustProbeResult{
		Detected:   true,
		Indicators: dedupeStrings(indicators),
		Commands:   commands,
	}
}

func runJVMProbe(ctx *Context) *JVMProbeResult {
	var indicators []string
	var buildTools []string

	if ctx.HasFile("pom.xml") || ctx.HasFile("pom.yaml") {
		indicators = append(indicators, "Maven project")
		buildTools = append(buildTools, "Maven")
	}
	if ctx.HasFile("build.gradle") || ctx.HasFile("build.gradle.kts") || ctx.HasFile("settings.gradle") {
		indicators = append(indicators, "Gradle project")
		buildTools = append(buildTools, "Gradle")
	}
	if ctx.HasFile("build.sbt") {
		indicators = append(indicators, "SBT project")
		buildTools = append(buildTools, "SBT")
	}
	if path, ok := ctx.FindFirstWithSuffix(".java", ".kt", ".scala"); ok {
		indicators = append(indicators, filepath.Base(path))
	}
	commands := commandStatuses(ctx, "java", "javac", "mvn", "gradle", "gradlew", "sbt")
	if len(indicators) == 0 {
		return nil
	}

	return &JVMProbeResult{
		Detected:   true,
		Indicators: dedupeStrings(indicators),
		Commands:   commands,
		BuildTools: dedupeStrings(buildTools),
	}
}

func runGitProbe(ctx *Context) *SimpleProbeResult {
	var indicators []string
	if ctx.HasDir(".git") {
		indicators = append(indicators, ".git directory")
	} else if path, ok := ctx.FindFirstFileNamed(".gitmodules"); ok {
		indicators = append(indicators, filepath.Base(path))
	}
	commands := commandStatuses(ctx, "git")
	if len(indicators) == 0 {
		return nil
	}
	return &SimpleProbeResult{
		Detected:   true,
		Indicators: dedupeStrings(indicators),
		Commands:   commands,
	}
}

func runContainerProbes(ctx *Context) []ContainerProbeResult {
	var results []ContainerProbeResult

	dockerIndicators := collectExistingFiles(ctx, []string{
		"Dockerfile",
		"docker-compose.yml",
		"docker-compose.yaml",
		".dockerignore",
	})
	dockerCommands := commandStatuses(ctx, "docker")
	if len(dockerIndicators) > 0 {
		results = append(results, ContainerProbeResult{
			Detected:   true,
			Indicators: dedupeStrings(dockerIndicators),
			Commands:   dockerCommands,
			Runtime:    "Docker",
		})
	}

	if status := commandStatuses(ctx, "podman"); len(status) > 0 && status[0].Available {
		results = append(results, ContainerProbeResult{
			Detected: true,
			Commands: status,
			Runtime:  "Podman",
		})
	}

	if status := commandStatuses(ctx, "nerdctl"); len(status) > 0 && status[0].Available {
		results = append(results, ContainerProbeResult{
			Detected: true,
			Commands: status,
			Runtime:  "nerdctl",
		})
	}

	return results
}

func runLintProbes(ctx *Context) []ToolingProbeResult {
	var results []ToolingProbeResult

	if indicators := collectExistingFiles(ctx, []string{
		".eslintrc",
		".eslintrc.json",
		".eslintrc.js",
		".eslintrc.cjs",
		".eslintrc.yaml",
		".eslintrc.yml",
	}); len(indicators) > 0 {
		results = append(results, ToolingProbeResult{
			Name:       "ESLint",
			Indicators: dedupeStrings(indicators),
			Commands:   commandStatuses(ctx, "eslint", "npx"),
		})
	}

	if ctx.HasFile("pyproject.toml") {
		if content, err := ctx.ReadFile("pyproject.toml"); err == nil && bytesContainsAny(content, []string{"[tool.flake8]", "[tool.ruff]"}) {
			results = append(results, ToolingProbeResult{
				Name:       "Python linters",
				Indicators: []string{"pyproject.toml"},
				Commands:   commandStatuses(ctx, "ruff", "flake8"),
			})
		}
	}

	return results
}

func runFormatterProbes(ctx *Context) []ToolingProbeResult {
	var results []ToolingProbeResult

	if indicators := collectExistingFiles(ctx, []string{
		".prettierrc",
		".prettierrc.json",
		".prettierrc.js",
		".prettierrc.cjs",
		".prettierrc.yaml",
		".prettierrc.yml",
		"prettier.config.js",
		"prettier.config.cjs",
	}); len(indicators) > 0 {
		results = append(results, ToolingProbeResult{
			Name:       "Prettier",
			Indicators: dedupeStrings(indicators),
			Commands:   commandStatuses(ctx, "prettier", "npx"),
		})
	}

	if ctx.HasFile("pyproject.toml") {
		if content, err := ctx.ReadFile("pyproject.toml"); err == nil && bytesContainsAny(content, []string{"[tool.black]", "[tool.ruff.format]"}) {
			results = append(results, ToolingProbeResult{
				Name:       "Python formatters",
				Indicators: []string{"pyproject.toml"},
				Commands:   commandStatuses(ctx, "black", "ruff"),
			})
		}
	}

	if ctx.HasFile(".clang-format") {
		results = append(results, ToolingProbeResult{
			Name:       "clang-format",
			Indicators: []string{".clang-format"},
			Commands:   commandStatuses(ctx, "clang-format"),
		})
	}

	return results
}

func detectOS() OSResult {
	return OSResult{
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		Distribution: readOSRelease(),
	}
}

func collectExistingFiles(ctx *Context, files []string) []string {
	var results []string
	for _, file := range files {
		if ctx.HasFile(file) {
			results = append(results, file)
		}
	}
	return results
}

func commandStatuses(ctx *Context, commands ...string) []CommandStatus {
	statuses := make([]CommandStatus, 0, len(commands))
	for _, cmd := range commands {
		statuses = append(statuses, CommandStatus{
			Name:      cmd,
			Available: ctx.CommandExists(cmd),
		})
	}
	return statuses
}

func availableCommandNames(commands []CommandStatus, names ...string) []string {
	lookup := map[string]struct{}{}
	includeAll := len(names) == 0
	for _, name := range names {
		lookup[name] = struct{}{}
	}
	var available []string
	for _, cmd := range commands {
		if !cmd.Available {
			continue
		}
		if includeAll {
			available = append(available, cmd.Name)
			continue
		}
		if _, ok := lookup[cmd.Name]; ok {
			available = append(available, cmd.Name)
		}
	}
	return available
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var result []string
	for _, value := range values {
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func readOSRelease() string {
	candidates := []string{"/etc/os-release", "/usr/lib/os-release"}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(data))
		lines := strings.Split(lower, "\n")
		for i, line := range lines {
			lines[i] = strings.TrimSpace(line)
		}
		for idx, line := range lines {
			if strings.HasPrefix(line, "pretty_name=") {
				originalLines := strings.Split(string(data), "\n")
				if idx < len(originalLines) {
					value := strings.TrimSpace(originalLines[idx])
					value = strings.TrimPrefix(value, "PRETTY_NAME=")
					value = strings.Trim(value, "\"")
					if value != "" {
						return value
					}
				}
			}
		}
	}
	return ""
}

func bytesContainsAny(data []byte, needles []string) bool {
	if len(data) == 0 {
		return false
	}
	lowerData := strings.ToLower(string(data))
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		if strings.Contains(lowerData, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

// HasCapabilities reports whether any tooling was detected.
func (r Result) HasCapabilities() bool {
	return r.Node != nil || r.Python != nil || r.DotNet != nil || r.Go != nil || r.Rust != nil || r.JVM != nil || r.Git != nil || len(r.Containers) > 0 || len(r.Linters) > 0 || len(r.Formatters) > 0
}

// SummaryLines returns the human-readable bullet lines describing the detected
// capabilities.
func (r Result) SummaryLines() []string {
	var lines []string

	if r.Node != nil {
		lines = append(lines, formatNodeSummary(*r.Node))
	}
	if r.Python != nil {
		lines = append(lines, formatSimpleSummary("Python project", r.Python.Indicators, r.Python.Commands))
	}
	if r.DotNet != nil {
		lines = append(lines, formatSimpleSummary(".NET SDK", r.DotNet.Indicators, r.DotNet.Commands))
	}
	if r.Go != nil {
		lines = append(lines, formatSimpleSummary("Go toolchain", r.Go.Indicators, r.Go.Commands))
	}
	if r.Rust != nil {
		lines = append(lines, formatSimpleSummary("Rust toolchain", r.Rust.Indicators, r.Rust.Commands))
	}
	if r.JVM != nil {
		lines = append(lines, formatJVMSummary(*r.JVM))
	}
	if r.Git != nil {
		lines = append(lines, formatSimpleSummary("Git repository", r.Git.Indicators, r.Git.Commands))
	}
	if len(r.Containers) > 0 {
		for _, container := range r.Containers {
			lines = append(lines, formatContainerSummary(container))
		}
	}
	if len(r.Linters) > 0 {
		lines = append(lines, formatToolSummary("Linters", r.Linters))
	}
	if len(r.Formatters) > 0 {
		lines = append(lines, formatToolSummary("Formatters", r.Formatters))
	}

	return lines
}

// FormatSummary renders a BootProbeResult into a human-readable summary. The
// OS line is always included, followed by detected capabilities rendered as
// bullet points.
func FormatSummary(result Result) string {
	osLine := FormatOSLine(result.OS)
	lines := result.SummaryLines()
	if len(lines) == 0 {
		return osLine
	}

	for i, line := range lines {
		lines[i] = "- " + line
	}

	return strings.Join(append([]string{osLine}, lines...), "\n")
}

// FormatOSLine renders a single line describing the host OS.
func FormatOSLine(osResult OSResult) string {
	if osResult.Distribution != "" {
		return fmt.Sprintf("OS: %s/%s (%s)", osResult.GOOS, osResult.GOARCH, osResult.Distribution)
	}
	return fmt.Sprintf("OS: %s/%s", osResult.GOOS, osResult.GOARCH)
}

// CombineAugmentation prepends the boot probe summary to any user-supplied
// instructions so that both are available to the runtime.
func CombineAugmentation(summary, user string) string {
	summary = strings.TrimSpace(summary)
	user = strings.TrimSpace(user)

	switch {
	case summary != "" && user != "":
		return summary + "\n\n" + user
	case summary != "":
		return summary
	case user != "":
		return user
	default:
		return ""
	}
}

func formatNodeSummary(result NodeProbeResult) string {
	var details []string
	if len(result.Indicators) > 0 {
		details = append(details, strings.Join(result.Indicators, ", "))
	}
	if len(result.PackageManagers) > 0 {
		details = append(details, "pkg mgrs: "+strings.Join(result.PackageManagers, ", "))
	}
	available := availableCommandNames(result.Commands)
	if len(available) > 0 {
		details = append(details, "commands: "+strings.Join(available, ", "))
	}
	return joinSummary("Node.js project", details)
}

func formatJVMSummary(result JVMProbeResult) string {
	var details []string
	if len(result.Indicators) > 0 {
		details = append(details, strings.Join(result.Indicators, ", "))
	}
	if len(result.BuildTools) > 0 {
		details = append(details, "build: "+strings.Join(result.BuildTools, ", "))
	}
	available := availableCommandNames(result.Commands)
	if len(available) > 0 {
		details = append(details, "commands: "+strings.Join(available, ", "))
	}
	return joinSummary("JVM tooling", details)
}

func formatContainerSummary(result ContainerProbeResult) string {
	label := "Container tooling"
	if result.Runtime != "" {
		label = result.Runtime + " tooling"
	}
	var details []string
	if len(result.Indicators) > 0 {
		details = append(details, strings.Join(result.Indicators, ", "))
	}
	available := availableCommandNames(result.Commands)
	if len(available) > 0 {
		details = append(details, "commands: "+strings.Join(available, ", "))
	}
	return joinSummary(label, details)
}

func formatToolSummary(category string, tools []ToolingProbeResult) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return fmt.Sprintf("%s: %s", category, strings.Join(names, ", "))
}

func formatSimpleSummary(title string, indicators []string, commands []CommandStatus) string {
	var details []string
	if len(indicators) > 0 {
		details = append(details, strings.Join(indicators, ", "))
	}
	available := availableCommandNames(commands)
	if len(available) > 0 {
		details = append(details, "commands: "+strings.Join(available, ", "))
	}
	return joinSummary(title, details)
}

func joinSummary(title string, details []string) string {
	if len(details) == 0 {
		return title
	}
	return fmt.Sprintf("%s (%s)", title, strings.Join(details, "; "))
}
