package kubeletbridge

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogBufferScopesAndTailsAssignmentLogs(t *testing.T) {
	buffer := NewLogBuffer(1024)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := buffer.Reset("assignment-one", now, "started"); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Append(now.Add(time.Second), "line %d", 1); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Append(now.Add(2*time.Second), "line %d", 2); err != nil {
		t.Fatal(err)
	}
	entries := buffer.Snapshot("assignment-one", time.Time{}, 2)
	if len(entries) != 2 || entries[0].Message != "line 1" || entries[1].Message != "line 2" {
		t.Fatalf("entries = %#v", entries)
	}
	if entries := buffer.Snapshot("another-assignment", time.Time{}, -1); len(entries) != 0 {
		t.Fatalf("logs leaked across assignments: %#v", entries)
	}
}

func TestLogBufferStreamsNewEntries(t *testing.T) {
	buffer := NewLogBuffer(1024)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := buffer.Reset("assignment-one", now, "started"); err != nil {
		t.Fatal(err)
	}
	_, cursor, notifications, unsubscribe := buffer.SnapshotAndSubscribe("assignment-one", time.Time{}, -1)
	defer unsubscribe()
	if err := buffer.Append(now.Add(time.Second), "ready"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
		entries, gap := buffer.EntriesAfter("assignment-one", cursor)
		if gap || len(entries) != 1 || entries[0].Message != "ready" {
			t.Fatalf("entries = %#v gap=%t", entries, gap)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for streamed log")
	}
}

func TestResetClosesOldAssignmentSubscribers(t *testing.T) {
	buffer := NewLogBuffer(1024)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := buffer.Reset("assignment-one", now, "started"); err != nil {
		t.Fatal(err)
	}
	_, _, notifications, unsubscribe := buffer.SnapshotAndSubscribe("assignment-one", time.Time{}, -1)
	defer unsubscribe()
	if err := buffer.Reset("assignment-two", now.Add(time.Second), "started two"); err != nil {
		t.Fatal(err)
	}
	if _, open := <-notifications; open {
		t.Fatal("old assignment subscriber remained open after reset")
	}
}

func TestPersistentLogBufferRestoresAssignmentAndExactMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container-logs.jsonl")
	now := time.Unix(1_800_000_000, 0).UTC()
	buffer, err := OpenLogBuffer(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.Reset("assignment-one", now, "started"); err != nil {
		t.Fatal(err)
	}
	if err := buffer.AppendMessage(now.Add(time.Second), "  exact output  "); err != nil {
		t.Fatal(err)
	}
	if err := buffer.AppendMessage(now.Add(2*time.Second), ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLogBuffer(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.Snapshot("assignment-one", time.Time{}, -1)
	if len(entries) != 3 || entries[1].Message != "  exact output  " || entries[2].Message != "" {
		t.Fatalf("restored entries = %#v", entries)
	}
}

func TestEntriesAfterReportsRingEviction(t *testing.T) {
	buffer := NewLogBuffer(8)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := buffer.Reset("assignment-one", now, "start"); err != nil {
		t.Fatal(err)
	}
	if err := buffer.AppendMessage(now.Add(time.Second), "12345678"); err != nil {
		t.Fatal(err)
	}
	entries, gap := buffer.EntriesAfter("assignment-one", 0)
	if !gap || len(entries) != 1 || entries[0].Message != "12345678" {
		t.Fatalf("entries = %#v gap=%t", entries, gap)
	}
}

func TestPersistentLogBufferRepairsTruncatedFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container-logs.jsonl")
	now := time.Unix(1_800_000_000, 0).UTC()
	buffer, err := OpenLogBuffer(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.Reset("assignment-one", now, "started"); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"entry","assignment":"assignment-one"`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLogBuffer(path, 1024)
	if err != nil {
		t.Fatalf("OpenLogBuffer rejected a crash-truncated tail: %v", err)
	}
	entries := reopened.Snapshot("assignment-one", time.Time{}, -1)
	if len(entries) != 1 || entries[0].Message != "started" {
		t.Fatalf("entries = %#v", entries)
	}
	if err := reopened.AppendMessage(now.Add(time.Second), "continued"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLogBuffer(path, 1024); err != nil {
		t.Fatalf("repaired log could not be reopened: %v", err)
	}
}

func TestReadLogSnapshotIgnoresTruncatedTailWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container-logs.jsonl")
	buffer, err := OpenLogBuffer(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := buffer.Reset("assignment", now, "started"); err != nil {
		t.Fatal(err)
	}
	if err := buffer.AppendMessage(now.Add(time.Second), "complete"); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"entry"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ReadLogSnapshot(path, "assignment", 1024, time.Time{}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Message != "complete" {
		t.Fatalf("entries = %#v", entries)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only snapshot modified the log file")
	}
}
