package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/asynkron/goagent/pkg/patch"
)

const applyPatchCommandName = "apply_patch"

func newApplyPatchCommand() InternalCommandHandler {
	return func(ctx context.Context, req InternalCommandRequest) (PlanObservationPayload, error) {
		payload := PlanObservationPayload{}

		commandLine, patchInput := splitCommandAndPatch(req.Raw)
		if strings.TrimSpace(commandLine) == "" {
			return failApplyPatch(&payload, "internal command: apply_patch requires a command line"), errors.New("apply_patch: missing command line")
		}

		opts, err := parseApplyPatchOptions(commandLine, req.Step.Command.Cwd)
		if err != nil {
			return failApplyPatch(&payload, err.Error()), err
		}

		if strings.TrimSpace(patchInput) == "" {
			err := errors.New("apply_patch: no patch provided")
			return failApplyPatch(&payload, err.Error()), err
		}

		operations, err := patch.Parse(patchInput)
		if err != nil {
			message := fmt.Sprintf("apply_patch: %v", err)
			return failApplyPatch(&payload, message), fmt.Errorf("apply_patch: %w", err)
		}

		if len(operations) == 0 {
			err := errors.New("apply_patch: no patch operations detected")
			return failApplyPatch(&payload, err.Error()), err
		}

		results, applyErr := patch.ApplyFilesystem(ctx, operations, opts)
		if applyErr != nil {
			var perr *patch.Error
			if errors.As(applyErr, &perr) {
				formatted := patch.FormatError(perr)
				return failApplyPatch(&payload, formatted), perr
			}
			return failApplyPatch(&payload, applyErr.Error()), applyErr
		}

		if len(results) == 0 {
			payload.Stdout = "No changes applied."
			zero := 0
			payload.ExitCode = &zero
			return payload, nil
		}

		sort.Slice(results, func(i, j int) bool {
			return results[i].Path < results[j].Path
		})

		builder := strings.Builder{}
		builder.WriteString("Success. Updated the following files:\n")
		for _, entry := range results {
			builder.WriteString(entry.Status)
			builder.WriteString(" ")
			builder.WriteString(entry.Path)
			builder.WriteString("\n")
		}

		payload.Stdout = strings.TrimRight(builder.String(), "\n")
		zero := 0
		payload.ExitCode = &zero
		return payload, nil
	}
}

func failApplyPatch(payload *PlanObservationPayload, message string) PlanObservationPayload {
	if payload == nil {
		payload = &PlanObservationPayload{}
	}
	payload.Stderr = message
	payload.Details = message
	one := 1
	payload.ExitCode = &one
	return *payload
}

func splitCommandAndPatch(raw string) (commandLine, patch string) {
	trimmed := strings.TrimLeftFunc(raw, unicode.IsSpace)
	if trimmed == "" {
		return "", ""
	}
	line, rest, found := strings.Cut(trimmed, "\n")
	if !found {
		return trimmed, ""
	}
	return line, rest
}

func parseApplyPatchOptions(commandLine, cwd string) (patch.FilesystemOptions, error) {
	tokens, err := tokenizeInternalCommand(commandLine)
	if err != nil {
		return patch.FilesystemOptions{}, fmt.Errorf("failed to parse command line: %w", err)
	}
	if len(tokens) == 0 {
		return patch.FilesystemOptions{}, errors.New("apply_patch: missing command name")
	}

	workingDir := strings.TrimSpace(cwd)
	if workingDir == "" {
		if wd, getErr := os.Getwd(); getErr == nil {
			workingDir = wd
		} else {
			return patch.FilesystemOptions{}, fmt.Errorf("failed to determine working directory: %w", getErr)
		}
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = abs
	}

	opts := patch.FilesystemOptions{Options: patch.Options{IgnoreWhitespace: true}, WorkingDir: workingDir}
	for _, token := range tokens[1:] {
		if eq := strings.IndexRune(token, '='); eq != -1 {
			key := strings.TrimSpace(token[:eq])
			value := strings.TrimSpace(token[eq+1:])
			switch strings.ToLower(key) {
			case "ignore_whitespace", "ignore-whitespace":
				if strings.EqualFold(value, "false") {
					opts.IgnoreWhitespace = false
				} else if strings.EqualFold(value, "true") {
					opts.IgnoreWhitespace = true
				}
			case "respect_whitespace", "respect-whitespace":
				if strings.EqualFold(value, "true") {
					opts.IgnoreWhitespace = false
				}
			}
			continue
		}

		switch token {
		case "--ignore-whitespace", "-w":
			opts.IgnoreWhitespace = true
		case "--respect-whitespace", "--no-ignore-whitespace", "-W":
			opts.IgnoreWhitespace = false
		default:
			switch strings.ToLower(token) {
			case "--respect-whitespace", "--no-ignore-whitespace":
				opts.IgnoreWhitespace = false
			case "--ignore-whitespace":
				opts.IgnoreWhitespace = true
			}
		}
	}
	return opts, nil
}

func registerBuiltinInternalCommands(rt *Runtime, executor *CommandExecutor) error {
	if executor == nil {
		return errors.New("nil executor")
	}
	if err := executor.RegisterInternalCommand(applyPatchCommandName, newApplyPatchCommand()); err != nil {
		return err
	}
	return executor.RegisterInternalCommand(runResearchCommandName, newRunResearchCommand(rt))
}
