package bootprobe

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Context provides helper methods for inspecting the repository root and
// interrogating the current execution environment. The helpers are intentionally
// lightweight so that unit tests can supply fixture directories and a custom
// command lookup implementation.
type Context struct {
	root     string
	lookPath func(string) (string, error)
}

// NewContext constructs a Context rooted at the provided path. Commands are
// resolved using exec.LookPath by default.
func NewContext(root string) *Context {
	return &Context{
		root:     root,
		lookPath: exec.LookPath,
	}
}

// NewContextWithLookPath allows tests to override the command lookup
// implementation so that probes can be exercised without relying on tools being
// present on the host PATH.
func NewContextWithLookPath(root string, lookPath func(string) (string, error)) *Context {
	ctx := NewContext(root)
	if lookPath != nil {
		ctx.lookPath = lookPath
	}
	return ctx
}

// Root returns the root directory that probes should inspect.
func (c *Context) Root() string {
	return c.root
}

// HasFile reports whether a file exists relative to the repository root.
func (c *Context) HasFile(relPath string) bool {
	if relPath == "" {
		return false
	}
	path := filepath.Join(c.root, relPath)
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// HasDir reports whether a directory exists relative to the repository root.
func (c *Context) HasDir(relPath string) bool {
	if relPath == "" {
		return false
	}
	path := filepath.Join(c.root, relPath)
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// HasAnyFile returns true if any of the provided relative paths exist.
func (c *Context) HasAnyFile(relPaths ...string) bool {
	for _, rel := range relPaths {
		if c.HasFile(rel) {
			return true
		}
	}
	return false
}

// ReadFile loads the contents of a project file relative to the repository
// root.
func (c *Context) ReadFile(relPath string) ([]byte, error) {
	if relPath == "" {
		return nil, errors.New("path must be provided")
	}
	return os.ReadFile(filepath.Join(c.root, relPath))
}

// CommandExists reports whether a command is available on PATH.
func (c *Context) CommandExists(name string) bool {
	if name == "" {
		return false
	}
	_, err := c.lookPath(name)
	return err == nil
}

// RunCommandOutput resolves and executes a command, returning its combined
// stdout/stderr output. Intended for lightweight, read-only probes such as
// `go version` and `go env -json`.
func (c *Context) RunCommandOutput(name string, args ...string) (string, error) {
	if name == "" {
		return "", errors.New("command name must be provided")
	}
	path, err := c.lookPath(name)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(path, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// FindFirstWithSuffix walks the repository looking for a file with any of the
// provided suffixes and returns the first match. The suffix comparison is
// case-insensitive and should include the dot (e.g. ".csproj").
func (c *Context) FindFirstWithSuffix(suffixes ...string) (string, bool) {
	if len(suffixes) == 0 {
		return "", false
	}
	lowerSuffixes := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		if suffix == "" {
			continue
		}
		lowerSuffixes = append(lowerSuffixes, strings.ToLower(suffix))
	}
	if len(lowerSuffixes) == 0 {
		return "", false
	}

	var (
		match    string
		foundErr = errors.New("found")
	)
	err := filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Skip large dependency directories that do not affect the probes.
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == "vendor" || name == "target" {
				return filepath.SkipDir
			}
			return nil
		}

		lower := strings.ToLower(filepath.Ext(path))
		for _, suffix := range lowerSuffixes {
			if lower == suffix {
				match = path
				return foundErr
			}
		}
		return nil
	})

	if err != nil && !errors.Is(err, foundErr) {
		return "", false
	}
	return match, match != ""
}

// FindFirstFileNamed walks the repository and returns the first file whose name
// exactly matches one of the provided candidates.
func (c *Context) FindFirstFileNamed(names ...string) (string, bool) {
	if len(names) == 0 {
		return "", false
	}
	normalized := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		normalized[strings.ToLower(name)] = struct{}{}
	}
	if len(normalized) == 0 {
		return "", false
	}

	var (
		match    string
		foundErr = errors.New("found")
	)
	err := filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == "vendor" || name == "target" {
				return filepath.SkipDir
			}
			return nil
		}

		if _, ok := normalized[strings.ToLower(filepath.Base(path))]; ok {
			match = path
			return foundErr
		}
		return nil
	})

	if err != nil && !errors.Is(err, foundErr) {
		return "", false
	}
	return match, match != ""
}
