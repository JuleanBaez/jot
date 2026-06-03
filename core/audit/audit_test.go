package audit

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempHome points HOME at a throwaway dir so audit writes land somewhere
// the test can inspect — and so we never touch a developer's real
// ~/.jot/audit.jsonl.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestAppendAndTailRoundTrip(t *testing.T) {
	home := withTempHome(t)

	if err := Ok("cli", "iiq.ticket.take", "T-1", map[string]any{"by": "julean"}); err != nil {
		t.Fatalf("Ok: %v", err)
	}
	if err := Fail("dash", "ad.user.disable", "jdoe", errFakeIO, nil); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	// File is created in ~/.jot/audit.jsonl
	if _, err := os.Stat(filepath.Join(home, ".jot", "audit.jsonl")); err != nil {
		t.Fatalf("audit file missing: %v", err)
	}

	entries, err := Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	// Newest first
	if entries[0].Action != "ad.user.disable" || entries[0].Result != "error" {
		t.Errorf("first entry wrong: %+v", entries[0])
	}
	if entries[1].Action != "iiq.ticket.take" || entries[1].Result != "ok" {
		t.Errorf("second entry wrong: %+v", entries[1])
	}
}

func TestTailMissingFileIsEmpty(t *testing.T) {
	withTempHome(t)
	got, err := Tail(10)
	if err != nil {
		t.Fatalf("Tail on missing file should be nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %d entries", len(got))
	}
}

// errFakeIO is a sentinel for Fail tests — avoids importing errors for a
// one-liner.
type fakeErr struct{}

func (fakeErr) Error() string { return "fake io error" }

var errFakeIO = fakeErr{}

func TestHookFiresAfterSuccessfulAppend(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() { SetHook(nil) })

	var got []Entry
	SetHook(func(e Entry) { got = append(got, e) })

	if err := Ok("cli", "iiq.ticket.take", "T-42", nil); err != nil {
		t.Fatalf("Ok: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 hook call, got %d", len(got))
	}
	if got[0].Action != "iiq.ticket.take" || got[0].Subject != "T-42" {
		t.Errorf("hook saw wrong entry: %+v", got[0])
	}
}

func TestHookPanicDoesNotPropagate(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() { SetHook(nil) })

	SetHook(func(Entry) { panic("boom") })

	// Must not panic; must still return nil.
	if err := Ok("cli", "test.panic", "x", nil); err != nil {
		t.Fatalf("Ok: %v", err)
	}

	// And the entry should still be on disk.
	entries, err := Tail(1)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "test.panic" {
		t.Fatalf("entry not persisted after panicking hook: %+v", entries)
	}
}

func TestSetHookNilClears(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() { SetHook(nil) })

	calls := 0
	SetHook(func(Entry) { calls++ })
	SetHook(nil)

	if err := Ok("cli", "test.noop", "x", nil); err != nil {
		t.Fatalf("Ok: %v", err)
	}
	if calls != 0 {
		t.Errorf("expected hook to be cleared, got %d calls", calls)
	}
}
