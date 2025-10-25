package bootprobe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildAugmentationIncludesSummary(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".prettierrc"), []byte("{}"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const value = 1;"), 0o644))

	lookup := func(name string) (string, error) {
		switch name {
		case "node", "npm", "prettier", "npx":
			return filepath.Join("/usr/bin", name), nil
		default:
			return "", exec.ErrNotFound
		}
	}

	ctx := NewContextWithLookPath(dir, lookup)
	result, summary, combined := BuildAugmentation(ctx, "user supplied guidance")

	require.True(t, result.HasCapabilities())
	require.NotEmpty(t, summary)
	require.True(t, strings.HasPrefix(summary, "OS:"))
	require.Contains(t, summary, "Node.js project")
	require.Contains(t, combined, summary)
	require.True(t, strings.HasSuffix(combined, "user supplied guidance"))
	require.Contains(t, combined, "Node.js project")
}
