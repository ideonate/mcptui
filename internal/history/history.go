// Package history records requests made in a session and optionally
// persists them to history.jsonl.
package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status values.
const (
	StatusOK        = "ok"
	StatusError     = "error"   // transport / JSON-RPC error
	StatusToolError = "isError" // tool returned isError
	StatusCancelled = "cancelled"
)

// Entry is one request.
type Entry struct {
	Time       time.Time       `json:"time"`
	Profile    string          `json:"profile"`
	Kind       string          `json:"kind"` // tool | prompt | resource | template | list | ping | raw
	Name       string          `json:"name"`
	Method     string          `json:"method"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Status     string          `json:"status"`
	DurationMS int64           `json:"duration_ms"`
	Error      string          `json:"error,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
}

// Duration returns the entry duration.
func (e Entry) Duration() time.Duration { return time.Duration(e.DurationMS) * time.Millisecond }

// maxStored caps the size of request/response stored on disk.
const maxStored = 256 * 1024

// Log is an in-memory history with optional persistence.
type Log struct {
	mu      sync.Mutex
	entries []Entry
	path    string
}

// New creates a Log; path "" disables persistence.
func New(path string) *Log { return &Log{path: path} }

// Add appends an entry (and persists it).
func (l *Log) Add(e Entry) {
	l.mu.Lock()
	l.entries = append(l.entries, e)
	path := l.path
	l.mu.Unlock()
	if path == "" {
		return
	}
	stored := e
	if len(stored.Request) > maxStored {
		stored.Request = nil
	}
	if len(stored.Response) > maxStored {
		stored.Response = nil
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Entries returns a copy, oldest first.
func (l *Log) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Len returns the number of entries.
func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// ReadFile loads persisted entries (for inspection or tests).
func ReadFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
