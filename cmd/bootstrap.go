package main

import "github.com/asynkron/goagent/internal/bootprobe"

// buildBootProbeAugmentation runs the boot probe suite for the provided context
// and returns the structured result, the formatted summary, and the combined
// augmentation string that should be forwarded to the runtime.
func buildBootProbeAugmentation(ctx *bootprobe.Context, userAugment string) (bootprobe.BootProbeResult, string, string) {
	result := bootprobe.Run(ctx)
	summary := bootprobe.FormatSummary(result)
	combined := bootprobe.CombineAugmentation(summary, userAugment)
	return result, summary, combined
}
