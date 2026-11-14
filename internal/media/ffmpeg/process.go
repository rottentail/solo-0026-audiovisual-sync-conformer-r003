package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// operationTimeout bounds one probe or decode invocation. A tool that cannot
// finish inside this window is treated like any other decode failure so the
// service can persist a terminal state instead of hanging a request.
const operationTimeout = 2 * time.Minute

// drainTimeout bounds how long Wait may spend copying output after the child
// exits or is killed. Without it a helper process could hold the output pipe
// open and stall the request.
const drainTimeout = 5 * time.Second

// stderrLimit keeps diagnostics bounded while still naming the first failure.
const stderrLimit = 8 << 10

type runResult struct {
	Stdout []byte
	Stderr []byte
}

// errOutputLimit reports that a tool produced more output than the caller's
// cap allows. It aborts the copy so the child is killed instead of streaming
// unbounded data into memory.
var errOutputLimit = errors.New("tool output exceeded the allowed limit")

type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(data []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if len(data) > remaining {
			c.buf.Write(data[:remaining])
		} else {
			c.buf.Write(data)
		}
	}
	return len(data), nil
}

type limitedWriter struct {
	limit    int
	buf      bytes.Buffer
	overflow bool
	onLimit  func()
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.buf.Len()+len(data) > w.limit {
		if !w.overflow {
			w.overflow = true
			if w.onLimit != nil {
				w.onLimit()
			}
		}
		return 0, errOutputLimit
	}
	return w.buf.Write(data)
}

// run executes one tool invocation against the supplied stdin bytes. It owns
// the child process: on cancellation, timeout, read failure, or output
// overflow the whole process group is killed and reaped before the function
// returns, so no decoder child can outlive the request that started it.
//
// stdoutLimit is the largest output the caller considers meaningful. Anything
// beyond it aborts the child instead of buffering unbounded data.
func (t *Toolchain) run(
	ctx context.Context,
	program string,
	args []string,
	stdin []byte,
	stdoutLimit int,
) (runResult, error) {
	path := t.binary(program)
	if path == "" {
		return runResult{}, fmt.Errorf("unknown ffmpeg program %q", program)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return runResult{}, fmt.Errorf("%s was cancelled: %w", program, err)
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	command := exec.CommandContext(operationCtx, path, args...)
	// The child leads its own process group so cancellation and cleanup also
	// reach any helper process, not just the direct child. The cancelled context
	// also kills the group when output overflows.
	isolateProcessGroup(command)
	command.WaitDelay = drainTimeout

	command.Stdin = bytes.NewReader(stdin)
	stdout := &limitedWriter{limit: stdoutLimit}
	command.Stdout = stdout
	stderr := &cappedBuffer{limit: stderrLimit}
	command.Stderr = stderr

	// An output overflow must stop the child immediately. Cancelling the
	// operation context here triggers the process-group kill instead of waiting
	// for a child that would fill the unread pipe forever.
	stdout.onLimit = cancel
	waitErr := command.Run()
	// Run has reaped the child, so its process group is gone; do not signal a
	// PID that the kernel may already have reused. Cancellation and overflow are
	// handled through command.Cancel while the child is still alive.

	if operationCtx.Err() != nil {
		// A cancel we initiated because of overflow is not a caller cancellation.
		if stdout.overflow {
			return runResult{}, fmt.Errorf("%s produced more output than the input can describe", program)
		}
		if errors.Is(operationCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return runResult{}, fmt.Errorf("%s timed out after %s", program, operationTimeout)
		}
		return runResult{}, fmt.Errorf("%s was cancelled: %w", program, ctx.Err())
	}
	if errors.Is(waitErr, errOutputLimit) || stdout.overflow {
		return runResult{}, fmt.Errorf("%s produced more output than the input can describe", program)
	}
	if waitErr != nil {
		return runResult{}, toolFailure(program, stderr.buf.Bytes(), waitErr)
	}
	if stderr.buf.Len() > 0 {
		// FFmpeg reports recoverable corruption only on stderr; accepting the
		// exit status alone would silently change sample or frame values.
		return runResult{}, toolFailure(program, stderr.buf.Bytes(), nil)
	}
	return runResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}, nil
}

func toolFailure(program string, diagnostics []byte, waitErr error) error {
	switch message := firstDiagnostic(diagnostics); {
	case message != "":
		return fmt.Errorf("%s rejected the payload: %s", program, message)
	case errors.Is(waitErr, exec.ErrWaitDelay):
		return fmt.Errorf("%s did not release its output pipe", program)
	case waitErr != nil:
		return fmt.Errorf("%s failed: %w", program, waitErr)
	default:
		return fmt.Errorf("%s reported a payload error", program)
	}
}

func firstDiagnostic(diagnostics []byte) string {
	for _, line := range bytes.Split(diagnostics, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			return string(trimmed)
		}
	}
	return ""
}
