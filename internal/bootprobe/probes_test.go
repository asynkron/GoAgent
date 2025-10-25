package bootprobe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunDetectsMultipleToolchains(t *testing.T) {
	dir := t.TempDir()

	mustWriteFile(t, dir, "package.json", "{}")
	mustWriteFile(t, dir, "tsconfig.json", "{}")
	mustWriteFile(t, dir, "yarn.lock", "lock")
	mustWriteFile(t, dir, ".prettierrc", "{}")
	mustWriteFile(t, dir, ".eslintrc.json", "{}")
	mustWriteFile(t, dir, "requirements.txt", "requests==2.0.0")
	mustWriteFile(t, dir, "poetry.lock", "package")
	mustWriteFile(t, dir, "Pipfile", "[packages]")
	mustWriteFile(t, dir, "pyproject.toml", `[tool.poetry]
name = "demo"
[tool.black]
line-length = 88
[tool.ruff]
`)
	mustWriteFile(t, dir, "setup.cfg", "[flake8]\nignore = E501")
	mustWriteFile(t, dir, "go.mod", "module example.com/demo")
	mustWriteFile(t, dir, "Cargo.toml", "[package]\nname='demo'")
	mustWriteFile(t, dir, "Dockerfile", "FROM scratch")
	mustWriteFile(t, dir, ".clang-format", "BasedOnStyle: LLVM")

	dotnetDir := filepath.Join(dir, "dotnet")
	require.NoError(t, os.MkdirAll(dotnetDir, 0o755))
	mustWriteFile(t, dotnetDir, "app.csproj", "<Project />")

	javaDir := filepath.Join(dir, "java")
	require.NoError(t, os.MkdirAll(javaDir, 0o755))
	mustWriteFile(t, javaDir, "pom.xml", "<project />")
	mustWriteFile(t, javaDir, "Main.java", "class Main {}")

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	mustWriteFile(t, srcDir, "main.ts", "export const demo = 1;")
	mustWriteFile(t, srcDir, "index.js", "module.exports = {};")

	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))

	lookups := map[string]bool{
		"node":         true,
		"npm":          true,
		"pnpm":         true,
		"yarn":         true,
		"npx":          true,
		"python3":      true,
		"python":       true,
		"pip":          true,
		"poetry":       true,
		"pipenv":       true,
		"dotnet":       true,
		"go":           true,
		"cargo":        true,
		"rustc":        true,
		"java":         true,
		"javac":        true,
		"mvn":          true,
		"gradle":       true,
		"gradlew":      true,
		"sbt":          true,
		"docker":       true,
		"eslint":       true,
		"prettier":     true,
		"black":        true,
		"ruff":         true,
		"flake8":       true,
		"clang-format": true,
		"git":          true,
	}

	ctx := NewContextWithLookPath(dir, func(name string) (string, error) {
		if lookups[name] {
			return filepath.Join("/usr/bin", name), nil
		}
		return "", exec.ErrNotFound
	})

	result := Run(ctx)
	require.NotNil(t, result.Node)
	require.Contains(t, result.Node.Indicators, "package.json")
	require.True(t, result.Node.HasTypeScript)
	require.True(t, result.Node.HasJavaScript)
	require.NotEmpty(t, result.Node.PackageManagers)

	require.NotNil(t, result.Python)
	require.Contains(t, result.Python.Indicators, "pyproject.toml")
	require.True(t, result.Python.UsesPoetry)
	require.True(t, result.Python.UsesPipenv)

	require.NotNil(t, result.DotNet)
	require.NotNil(t, result.Go)
	require.NotNil(t, result.Rust)
	require.NotNil(t, result.JVM)
	require.NotNil(t, result.Git)
	require.NotEmpty(t, result.Containers)
	require.NotEmpty(t, result.Linters)
	require.NotEmpty(t, result.Formatters)
	require.True(t, result.HasCapabilities())

	summary := FormatSummary(result)
	require.Contains(t, summary, "Node.js project")
	require.Contains(t, summary, "Go toolchain")
	require.True(t, strings.HasPrefix(summary, "OS:"))
}

func TestCombineAugmentation(t *testing.T) {
	summary := "OS: linux/amd64\n- Tooling"
	combined := CombineAugmentation(summary, "user notes")
	require.Equal(t, "OS: linux/amd64\n- Tooling\n\nuser notes", combined)

	require.Equal(t, summary, CombineAugmentation(summary, ""))
	require.Equal(t, "user", CombineAugmentation("", "user"))
	require.Equal(t, "", CombineAugmentation("", ""))
}

func mustWriteFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}
