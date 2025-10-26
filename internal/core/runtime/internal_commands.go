package runtime

// registerBuiltinInternalCommands installs the default internal command handlers.
func registerBuiltinInternalCommands(executor *CommandExecutor) error {
	if executor == nil {
		return nil
	}
	if err := executor.RegisterInternalCommand("apply_patch", newApplyPatchCommand()); err != nil {
		return err
	}
	return nil
}
