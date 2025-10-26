package runtime

func registerBuiltinInternalCommands(executor *CommandExecutor) error {
	if executor == nil {
		return nil
	}
	builtins := map[string]InternalCommandHandler{
		"apply_patch": applyPatchCommand,
	}
	for name, handler := range builtins {
		if err := executor.RegisterInternalCommand(name, handler); err != nil {
			return err
		}
	}
	return nil
}
