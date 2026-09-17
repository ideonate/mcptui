package mcp

import (
	"context"
	"errors"
	"fmt"
)

// Transport moves raw JSON-RPC messages between client and server.
type Transport interface {
	// Start prepares the transport (e.g. launches a process).
	Start(ctx context.Context) error
	// Send delivers one JSON-RPC message. Replies and server-initiated
	// messages arrive on Messages.
	Send(ctx context.Context, msg []byte) error
	// Messages is closed when the transport shuts down; Err then explains why.
	Messages() <-chan []byte
	// Err returns the reason the transport stopped, if any.
	Err() error
	// Close terminates the session.
	Close() error
}

// ProtocolVersionSetter is implemented by transports that must echo the
// negotiated protocol version (streamable HTTP).
type ProtocolVersionSetter interface {
	SetProtocolVersion(v string)
}

// SessionStarter is implemented by transports that can open a standalone
// server->client stream after initialization.
type SessionStarter interface {
	StartListening()
}

// SessionResetter is implemented by transports that track a session id.
type SessionResetter interface {
	ResetSession()
}

// ErrSessionExpired means the server no longer knows our session (HTTP 404).
var ErrSessionExpired = errors.New("mcp session expired")

// HTTPError is returned for unexpected HTTP statuses.
type HTTPError struct {
	StatusCode      int
	Status          string
	Body            string
	WWWAuthenticate string
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("HTTP %s", e.Status)
	if e.Body != "" {
		b := e.Body
		if len(b) > 300 {
			b = b[:300] + "…"
		}
		msg += ": " + b
	}
	return msg
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// Event is a transport/protocol log line surfaced to the UI.
type Event struct {
	Source string // "transport", "stderr", "server", "auth", "client"
	Level  string // "debug", "info", "warn", "error"
	Text   string
}

// Logf is a sink for Events.
type Logf func(Event)

func (l Logf) emit(source, level, format string, args ...any) {
	if l != nil {
		l(Event{Source: source, Level: level, Text: fmt.Sprintf(format, args...)})
	}
}
