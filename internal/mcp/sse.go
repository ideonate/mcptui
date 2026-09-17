package mcp

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one server-sent event.
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// readSSE parses an event stream, calling fn for each dispatched event.
// It returns when the stream ends or fn returns false.
func readSSE(r io.Reader, fn func(sseEvent) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	var ev sseEvent
	var data []string
	dispatch := func() bool {
		if len(data) == 0 {
			ev = sseEvent{}
			return true
		}
		ev.Data = strings.Join(data, "\n")
		ok := fn(ev)
		ev = sseEvent{}
		data = data[:0]
		return ok
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if !dispatch() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data = append(data, value)
		case "event":
			ev.Event = value
		case "id":
			ev.ID = value
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	dispatch()
	return nil
}
