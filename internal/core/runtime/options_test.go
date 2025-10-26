package runtime

import "testing"

func TestRuntimeOptionsSetDefaultsHandsFreeTopic(t *testing.T) {
	t.Parallel()

	opts := RuntimeOptions{HandsFree: true}
	opts.setDefaults()
	if opts.HandsFreeTopic != "Hands-free session" {
		t.Fatalf("expected default hands-free topic, got %q", opts.HandsFreeTopic)
	}

	custom := RuntimeOptions{HandsFree: true, HandsFreeTopic: "  Explore logs   "}
	custom.setDefaults()
	if custom.HandsFreeTopic != "Explore logs" {
		t.Fatalf("expected trimmed hands-free topic, got %q", custom.HandsFreeTopic)
	}
}
