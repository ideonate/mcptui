package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// StdioOptions configures a StdioTransport.
type StdioOptions struct {
	Command []string
	Dir     string
	Env     map[string]string
	Log     Logf
}

// StdioTransport runs a server as a child process speaking newline-delimited
// JSON-RPC on stdin/stdout.
type StdioTransport struct {
	opts StdioOptions

	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex
	mu      sync.Mutex
	err     error
	stderr  []string // ring of recent stderr lines
	exited  chan struct{}
	closing bool

	msgs chan []byte
}

const stderrKeep = 20

// NewStdioTransport creates a stdio transport; Start launches the process.
func NewStdioTransport(opts StdioOptions) *StdioTransport {
	return &StdioTransport{opts: opts, msgs: make(chan []byte, 64), exited: make(chan struct{})}
}

func (t *StdioTransport) Start(ctx context.Context) error {
	if len(t.opts.Command) == 0 {
		return errors.New("stdio transport: empty command")
	}
	cmd := exec.Command(t.opts.Command[0], t.opts.Command[1:]...)
	cmd.Dir = t.opts.Dir
	cmd.Env = os.Environ()
	for k, v := range t.opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	setProcAttrs(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", strings.Join(t.opts.Command, " "), err)
	}
	t.cmd = cmd
	t.stdin = stdin
	t.opts.Log.emit("transport", "info", "started pid %d: %s", cmd.Process.Pid, strings.Join(t.opts.Command, " "))

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			t.mu.Lock()
			t.stderr = append(t.stderr, line)
			if len(t.stderr) > stderrKeep {
				t.stderr = t.stderr[len(t.stderr)-stderrKeep:]
			}
			t.mu.Unlock()
			t.opts.Log.emit("stderr", "info", "%s", line)
		}
	}()

	go func() {
		rd := bufio.NewReader(stdout)
		for {
			line, err := rd.ReadBytes('\n')
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				t.msgs <- line
			}
			if err != nil {
				break
			}
		}
		waitErr := cmd.Wait()
		select {
		case <-stderrDone:
		case <-time.After(500 * time.Millisecond):
		}
		t.mu.Lock()
		if !t.closing {
			msg := "server process exited"
			if waitErr != nil {
				msg = fmt.Sprintf("server process exited: %v", waitErr)
			}
			if len(t.stderr) > 0 {
				msg += "\nlast stderr:\n  " + strings.Join(t.stderr, "\n  ")
			}
			t.err = errors.New(msg)
			t.opts.Log.emit("transport", "error", "%s", msg)
		} else if t.err == nil {
			t.err = errors.New("transport closed")
		}
		t.mu.Unlock()
		close(t.exited)
		close(t.msgs)
	}()
	return nil
}

func (t *StdioTransport) Send(ctx context.Context, msg []byte) error {
	if t.stdin == nil {
		return errors.New("stdio transport not started")
	}
	select {
	case <-t.exited:
		if err := t.Err(); err != nil {
			return err
		}
		return errors.New("server process exited")
	default:
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	buf := make([]byte, 0, len(msg)+1)
	buf = append(buf, msg...)
	buf = append(buf, '\n')
	_, err := t.stdin.Write(buf)
	return err
}

func (t *StdioTransport) Messages() <-chan []byte { return t.msgs }

func (t *StdioTransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// StderrTail returns the most recent stderr lines.
func (t *StdioTransport) StderrTail() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.stderr...)
}

// Close closes stdin, then escalates to SIGTERM and SIGKILL.
func (t *StdioTransport) Close() error {
	if t.cmd == nil {
		return nil
	}
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return nil
	}
	t.closing = true
	t.mu.Unlock()
	_ = t.stdin.Close()
	select {
	case <-t.exited:
		return nil
	case <-time.After(2 * time.Second):
	}
	terminate(t.cmd)
	select {
	case <-t.exited:
		return nil
	case <-time.After(2 * time.Second):
	}
	kill(t.cmd)
	<-t.exited
	return nil
}
