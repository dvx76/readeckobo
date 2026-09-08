package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunQNDBRescan covers the exec wrapper: success, tolerated nonzero exit
// (NickelDBus signal timeout while the import continues), and the missing
// binary sentinel.
func TestRunQNDBRescan(t *testing.T) {
	dir := t.TempDir()
	log, _ := newLogger("", io.Discard)
	defer log.Close()

	ok := filepath.Join(dir, "ok-qndb")
	if err := os.WriteFile(ok, []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runQNDBRescan(context.Background(), ok, []string{"-t", "1", "-m", "pfmRescanBooksFull"}, log); err != nil {
		t.Errorf("success path: %v", err)
	}

	bad := filepath.Join(dir, "bad-qndb")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runQNDBRescan(context.Background(), bad, []string{}, log); err != nil {
		t.Errorf("nonzero exit should be tolerated (import keeps running), got %v", err)
	}

	err := runQNDBRescan(context.Background(), filepath.Join(dir, "missing-qndb"), []string{}, log)
	if !errors.Is(err, errQNDBNotFound) {
		t.Errorf("missing binary: want errQNDBNotFound, got %v", err)
	}
}

// TestRescanAndPollSkipsWhenUnavailable verifies that a missing rescan tool
// does not burn the whole poll window.
func TestRescanAndPollSkipsWhenUnavailable(t *testing.T) {
	env := newLoopEnv(t)
	ag := env.newAgent(t, func(o *agentOptions) { o.pollTime = 5 * time.Second })
	ag.rescan = func(ctx context.Context) error { return errQNDBNotFound }
	ag.idx.set(indexEntry{Filename: "never-imported-x.kepub.epub", BookmarkID: "nope", ETag: "e", State: stateDownloaded})

	start := time.Now()
	err := ag.rescanAndPoll(context.Background())
	if !errors.Is(err, errQNDBNotFound) {
		t.Fatalf("err = %v, want errQNDBNotFound", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("rescanAndPoll took %v; must skip polling when qndb is missing", elapsed)
	}
}

// TestPollForImports covers the poll loop: it returns promptly when every
// pending import has a content row, and times out (with the file list) when a
// row never appears.
func TestPollForImports(t *testing.T) {
	env := newLoopEnv(t)
	ag := env.newAgent(t, func(o *agentOptions) {
		o.pollWait = 2 * time.Millisecond
		o.pollTime = 100 * time.Millisecond
	})
	// fileA has a real content row in the (rewritten) fixture → found.
	ag.idx.set(indexEntry{Filename: fileA, BookmarkID: bookAID, ETag: "e", Updated: "2026-09-07T10:00:00Z", State: stateDownloaded})
	if err := ag.pollForImports(context.Background()); err != nil {
		t.Fatalf("poll should succeed for an existing row: %v", err)
	}

	// A file with no content row → timeout error naming the file.
	ag.idx.set(indexEntry{Filename: "never-imported-y.kepub.epub", BookmarkID: "nope", ETag: "e", State: stateDownloaded})
	err := ag.pollForImports(context.Background())
	if err == nil || !strings.Contains(err.Error(), "never-imported-y.kepub.epub") {
		t.Fatalf("poll timeout err = %v, want timeout mentioning the missing file", err)
	}
}

// TestSyncOnceFatalServerUnreachable asserts the top-level error path: an
// unreachable server fails the pass (the loop mode in main.go keeps retrying).
func TestSyncOnceFatalServerUnreachable(t *testing.T) {
	env := newLoopEnv(t)
	cfg := &Config{
		ServerURL:    "http://127.0.0.1:1", // closed port
		Token:        "t",
		DeviceSerial: "s",
		Collection:   "Readeck",
		QNDBPath:     "/bin/true",
		HTTPTimeout:  300 * time.Millisecond,
		Path:         filepath.Join(env.dataDir, "config"),
	}
	logger, _ := newLogger("", io.Discard)
	defer logger.Close()
	ag, err := newAgent(agentOptions{
		cfg: cfg, logger: logger, onboard: env.onboard, koboDB: env.koboDB,
		dataDir: env.dataDir, noRescan: true, snapshot: env.snapshot,
		pollWait: time.Millisecond, pollTime: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = ag.syncOnce(context.Background())
	if err == nil {
		t.Fatal("expected a fatal error from an unreachable server")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("err = %v, want a state-step error", err)
	}
}
