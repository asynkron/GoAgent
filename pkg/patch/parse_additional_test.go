package patch

import "testing"

func TestParseSupportsMoveWithoutHunks(t *testing.T) {
	t.Parallel()

	patchBody := "*** Begin Patch\n*** Update File: old.txt\n*** Move to: new.txt\n*** End Patch\n"
	ops, err := Parse(patchBody)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected one operation, got %d", len(ops))
	}
	if ops[0].MovePath != "new.txt" || ops[0].Type != OperationUpdate {
		t.Fatalf("unexpected operation: %#v", ops[0])
	}
}

func TestParseErrorsOnUnexpectedDiffContent(t *testing.T) {
	t.Parallel()

	patchBody := "*** Begin Patch\nnoise\n*** End Patch\n"
	if _, err := Parse(patchBody); err == nil {
		t.Fatalf("expected parse error for stray content")
	}
}

func TestParseErrorsOnMissingEnd(t *testing.T) {
	t.Parallel()

	patchBody := "*** Begin Patch\n*** Add File: foo.txt\n@@\n+foo\n"
	if _, err := Parse(patchBody); err == nil {
		t.Fatalf("expected error for missing terminator")
	}
}
