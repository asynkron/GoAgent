package bootprobe

// BuildAugmentation runs the boot probe suite for the provided context and
// returns the structured result, the formatted summary, and the combined
// augmentation string that should be forwarded to the runtime. Keeping this
// helper in the bootprobe package means callers can import it from a single
// place without having to remember to compile additional files manually.
func BuildAugmentation(ctx *Context, userAugment string) (Result, string, string) {
	result := Run(ctx)
	summary := FormatSummary(result)
	combined := CombineAugmentation(summary, userAugment)
	return result, summary, combined
}
