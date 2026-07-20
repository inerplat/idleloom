package kubeletbridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultLogBytes = 1 << 20
	maxDiskLogBytes = 4 << 20
)

type LogEntry struct {
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Message  string    `json:"message"`
}

type diskRecord struct {
	Type       string    `json:"type"`
	Assignment string    `json:"assignment"`
	Sequence   uint64    `json:"sequence,omitempty"`
	Time       time.Time `json:"time,omitempty"`
	Message    string    `json:"message,omitempty"`
}

type LogBuffer struct {
	mu          sync.RWMutex
	path        string
	assignment  string
	entries     []LogEntry
	bytes       int
	fileBytes   int64
	maximum     int
	next        uint64
	subscribers map[chan struct{}]struct{}
}

func NewLogBuffer(maximumBytes int) *LogBuffer {
	if maximumBytes <= 0 {
		maximumBytes = defaultLogBytes
	}
	return &LogBuffer{maximum: maximumBytes, next: 1, subscribers: make(map[chan struct{}]struct{})}
}

func OpenLogBuffer(path string, maximumBytes int) (*LogBuffer, error) {
	buffer := NewLogBuffer(maximumBytes)
	buffer.path = path
	if err := buffer.load(); err != nil {
		return nil, err
	}
	return buffer, nil
}

// ReadLogSnapshot reads completed records without repairing or rewriting the
// agent-owned log file. An incomplete final record is ignored.
func ReadLogSnapshot(path, assignment string, maximumBytes int, since time.Time, tailLines int64) ([]LogEntry, error) {
	buffer := NewLogBuffer(maximumBytes)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no local workload logs exist on this Mac; --local only works on the joined host after a workload has run — drop --local to read logs through the cluster")
		}
		return nil, fmt.Errorf("open native container log: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxDiskLogBytes {
		return nil, fmt.Errorf("native container log is too large: %d bytes", info.Size())
	}
	reader := bufio.NewReader(io.LimitReader(file, maxDiskLogBytes+1))
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF && len(line) > 0 && line[len(line)-1] != '\n' {
			break
		}
		if len(line) > 0 {
			var record diskRecord
			if err := json.Unmarshal(line, &record); err != nil {
				return nil, fmt.Errorf("decode native container log: %w", err)
			}
			switch record.Type {
			case "reset":
				buffer.assignment = record.Assignment
				buffer.entries = nil
				buffer.bytes = 0
				buffer.next = 1
			case "entry":
				if record.Assignment != buffer.assignment || record.Sequence < buffer.next {
					return nil, fmt.Errorf("native container log sequence is invalid")
				}
				buffer.next = record.Sequence + 1
				buffer.appendLocked(LogEntry{Sequence: record.Sequence, Time: record.Time.UTC(), Message: record.Message})
			default:
				return nil, fmt.Errorf("native container log record type %q is invalid", record.Type)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return buffer.Snapshot(assignment, since, tailLines), nil
}

func (buffer *LogBuffer) Reset(assignment string, now time.Time, message string) error {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	for subscriber := range buffer.subscribers {
		close(subscriber)
	}
	buffer.subscribers = make(map[chan struct{}]struct{})
	buffer.assignment = assignment
	buffer.entries = nil
	buffer.bytes = 0
	buffer.next = 1
	if message != "" {
		buffer.appendLocked(LogEntry{Sequence: buffer.next, Time: now.UTC(), Message: message})
		buffer.next++
	}
	return buffer.rewriteLocked()
}

func (buffer *LogBuffer) Append(now time.Time, format string, values ...any) error {
	return buffer.AppendMessage(now, fmt.Sprintf(format, values...))
}

func (buffer *LogBuffer) AppendMessage(now time.Time, message string) error {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.assignment == "" {
		return nil
	}
	entry := LogEntry{Sequence: buffer.next, Time: now.UTC(), Message: message}
	buffer.next++
	buffer.appendLocked(entry)
	if err := buffer.appendDiskLocked(entry); err != nil {
		return err
	}
	for subscriber := range buffer.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
	return nil
}

func (buffer *LogBuffer) Snapshot(assignment string, since time.Time, tailLines int64) []LogEntry {
	buffer.mu.RLock()
	defer buffer.mu.RUnlock()
	return buffer.snapshotLocked(assignment, since, tailLines)
}

func (buffer *LogBuffer) MatchesAssignment(assignment string) bool {
	buffer.mu.RLock()
	defer buffer.mu.RUnlock()
	return assignment != "" && buffer.assignment == assignment
}

func (buffer *LogBuffer) SnapshotAndSubscribe(assignment string, since time.Time, tailLines int64) ([]LogEntry, uint64, <-chan struct{}, func()) {
	notifications := make(chan struct{}, 1)
	buffer.mu.Lock()
	entries := buffer.snapshotLocked(assignment, since, tailLines)
	cursor := uint64(0)
	if buffer.next > 0 {
		cursor = buffer.next - 1
	}
	if assignment == "" || assignment != buffer.assignment {
		close(notifications)
	} else {
		buffer.subscribers[notifications] = struct{}{}
	}
	buffer.mu.Unlock()
	return entries, cursor, notifications, func() {
		buffer.mu.Lock()
		if _, ok := buffer.subscribers[notifications]; ok {
			delete(buffer.subscribers, notifications)
			close(notifications)
		}
		buffer.mu.Unlock()
	}
}

func (buffer *LogBuffer) EntriesAfter(assignment string, sequence uint64) ([]LogEntry, bool) {
	buffer.mu.RLock()
	defer buffer.mu.RUnlock()
	if assignment == "" || assignment != buffer.assignment || len(buffer.entries) == 0 {
		return nil, false
	}
	gap := sequence+1 < buffer.entries[0].Sequence
	entries := make([]LogEntry, 0, len(buffer.entries))
	for _, entry := range buffer.entries {
		if entry.Sequence > sequence {
			entries = append(entries, entry)
		}
	}
	return entries, gap
}

func (buffer *LogBuffer) snapshotLocked(assignment string, since time.Time, tailLines int64) []LogEntry {
	if assignment == "" || assignment != buffer.assignment {
		return nil
	}
	entries := make([]LogEntry, 0, len(buffer.entries))
	for _, entry := range buffer.entries {
		if since.IsZero() || !entry.Time.Before(since) {
			entries = append(entries, entry)
		}
	}
	if tailLines >= 0 && int64(len(entries)) > tailLines {
		entries = entries[len(entries)-int(tailLines):]
	}
	return entries
}

func (buffer *LogBuffer) appendLocked(entry LogEntry) {
	buffer.entries = append(buffer.entries, entry)
	buffer.bytes += len(entry.Message) + 1
	for buffer.bytes > buffer.maximum && len(buffer.entries) > 1 {
		buffer.bytes -= len(buffer.entries[0].Message) + 1
		buffer.entries = buffer.entries[1:]
	}
}

func (buffer *LogBuffer) load() error {
	if buffer.path == "" {
		return nil
	}
	file, err := os.Open(buffer.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open native container log: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxDiskLogBytes {
		return fmt.Errorf("native container log is too large: %d bytes", info.Size())
	}
	buffer.fileBytes = info.Size()
	reader := bufio.NewReader(io.LimitReader(file, maxDiskLogBytes+1))
	truncatedTail := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF && len(line) > 0 && line[len(line)-1] != '\n' {
			truncatedTail = true
			break
		}
		if len(line) > 0 {
			var record diskRecord
			if err := json.Unmarshal(line, &record); err != nil {
				return fmt.Errorf("decode native container log: %w", err)
			}
			switch record.Type {
			case "reset":
				buffer.assignment = record.Assignment
				buffer.entries = nil
				buffer.bytes = 0
				buffer.next = 1
			case "entry":
				if record.Assignment != buffer.assignment || record.Sequence < buffer.next {
					return fmt.Errorf("native container log sequence is invalid")
				}
				buffer.next = record.Sequence + 1
				buffer.appendLocked(LogEntry{Sequence: record.Sequence, Time: record.Time.UTC(), Message: record.Message})
			default:
				return fmt.Errorf("native container log record type %q is invalid", record.Type)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if truncatedTail {
		return buffer.rewriteLocked()
	}
	return nil
}

func (buffer *LogBuffer) appendDiskLocked(entry LogEntry) error {
	if buffer.path == "" {
		return nil
	}
	record := diskRecord{Type: "entry", Assignment: buffer.assignment, Sequence: entry.Sequence, Time: entry.Time, Message: entry.Message}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if buffer.fileBytes+int64(len(data)) > int64(buffer.maximum*2) {
		return buffer.rewriteLocked()
	}
	if err := os.MkdirAll(filepath.Dir(buffer.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(buffer.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return joinErrors(err, file.Close())
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := joinErrors(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	buffer.fileBytes += int64(len(data))
	return nil
}

func (buffer *LogBuffer) rewriteLocked() error {
	if buffer.path == "" {
		return nil
	}
	var data []byte
	records := make([]diskRecord, 0, len(buffer.entries)+1)
	records = append(records, diskRecord{Type: "reset", Assignment: buffer.assignment})
	for _, entry := range buffer.entries {
		records = append(records, diskRecord{Type: "entry", Assignment: buffer.assignment, Sequence: entry.Sequence, Time: entry.Time, Message: entry.Message})
	}
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := atomicWrite(buffer.path, data, 0o600); err != nil {
		return err
	}
	buffer.fileBytes = int64(len(data))
	return nil
}

func joinErrors(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
