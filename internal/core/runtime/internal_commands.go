package runtime

// registerDefaultInternalCommands wires the built-in internal commands into a new executor.
// Additional commands supplied via RuntimeOptions can extend or override these defaults.
func registerDefaultInternalCommands(executor *CommandExecutor) error {
	if executor == nil {
		return nil
	}
	return executor.RegisterInternalCommand("apply_patch", newApplyPatchCommand())
}
