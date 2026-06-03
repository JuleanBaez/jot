// Package audit is the append-only action log for jot. Every destructive or
// user-facing operation (ticket resolved, AD account disabled, password
// reset) writes one JSON line to ~/.jot/audit.jsonl.
//
// Design notes:
//   - JSONL, not JSON: crash-safe, append-cheap, easy to tail with the dash.
//   - One writer mutex per process: two goroutines inside `jot dash` can
//     audit concurrently without interleaving bytes in a line.
//   - Tail() reads the tail of the file in memory — fine for the sizes we
//     expect (< 1M entries / < 100MB). If the file grows past that, we'll
//     add rotation; not today.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"jot/core/config"
)

// Entry is one line in the audit log. Actor is whoever triggered the action
// ("cli" or "dash"); Action is a dotted verb ("iiq.ticket.resolve",
// "ad.user.disable") so we can filter later. Subject is the thing acted on
// (ticket ID, username). Result is "ok" or "error". Err captures the error
// message when Result == "error". Meta is free-form context.
type Entry struct {
	Time    time.Time      `json:"t"`
	Actor   string         `json:"actor"`
	Action  string         `json:"action"`
	Subject string         `json:"subject,omitempty"`
	Result  string         `json:"result"`
	Err     string         `json:"err,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

var writeMu sync.Mutex

// hook is an optional package-level callback invoked after every successful
// Append. Callers (the dash) register one via SetHook so they can forward
// audit entries onto an in-process event bus without audit itself taking a
// dependency on events. Kept behind a RWMutex because writes are rare (once
// at Run start) and reads happen on every append.
var (
	hookMu sync.RWMutex
	hook   func(Entry)
)

// SetHook registers fn as the post-append callback. Pass nil to clear.
//
// The hook runs on the goroutine that called Append, after the line has been
// flushed to disk. Hooks must be cheap and non-blocking — this is NOT the
// place to do I/O. A panicking hook will not take the caller down: panics
// are caught and swallowed so audit remains a strictly additive concern.
func SetHook(fn func(Entry)) {
	hookMu.Lock()
	hook = fn
	hookMu.Unlock()
}

// fireHook invokes the registered hook, if any, with panic protection.
func fireHook(e Entry) {
	hookMu.RLock()
	fn := hook
	hookMu.RUnlock()
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(e)
}

// LogPath returns the absolute path to the audit log, ensuring the directory
// exists.
func LogPath() (string, error) {
	d, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "audit.jsonl"), nil
}

// Append writes one entry to the log. Time is auto-stamped if zero.
func Append(e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Result == "" {
		if e.Err != "" {
			e.Result = "error"
		} else {
			e.Result = "ok"
		}
	}
	path, err := LogPath()
	if err != nil {
		return err
	}

	writeMu.Lock()
	defer writeMu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal audit entry: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}

	// Fire the hook AFTER the bytes are on disk — we don't want to notify a
	// subscriber about an entry that then failed to persist.
	fireHook(e)
	return nil
}

// Ok is a shorthand for a successful action.
func Ok(actor, action, subject string, meta map[string]any) error {
	return Append(Entry{Actor: actor, Action: action, Subject: subject, Meta: meta, Result: "ok"})
}

// Fail is a shorthand for a failed action.
func Fail(actor, action, subject string, err error, meta map[string]any) error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return Append(Entry{Actor: actor, Action: action, Subject: subject, Meta: meta, Result: "error", Err: msg})
}

// Tail returns up to n most-recent entries, newest first. Missing file is
// treated as empty (returns nil, nil) so a fresh install doesn't error.
func Tail(n int) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}
	path, err := LogPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Simple approach: read all lines, keep the last n. Audit logs stay
	// small; if someone runs jot in anger for years, we'll rotate.
	var all []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // skip corrupt lines rather than aborting
		}
		all = append(all, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	start := len(all) - n
	if start < 0 {
		start = 0
	}
	tail := all[start:]

	// Reverse to newest-first.
	out := make([]Entry, len(tail))
	for i, e := range tail {
		out[len(tail)-1-i] = e
	}
	return out, nil
}
