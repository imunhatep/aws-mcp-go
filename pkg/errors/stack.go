package errors

import (
	stderrors "errors"
	"fmt"
	"io"
	"runtime"
)

// stack is a captured program-counter stack, most-recent-call first (runtime.Callers order).
type stack []uintptr

// callers records the stack of the caller of a public API function. skip is the
// number of additional in-package frames (helpers) between the public entry point
// and this call, so the recorded stack starts at the user's call site.
func callers(skip int) *stack {
	const depth = 32
	var pcs [depth]uintptr
	// 2 = skip runtime.Callers + callers itself; +1 for the public API frame; +skip for extra hops.
	n := runtime.Callers(3+skip, pcs[:])
	s := stack(pcs[:n])
	return &s
}

// format writes the stack in the pkg/errors "%+v" style: for each frame a newline,
// the fully-qualified function name, then a tab-indented file:line.
func (s *stack) format(w io.Writer) {
	if s == nil {
		return
	}
	frames := runtime.CallersFrames(*s)
	for {
		fr, more := frames.Next()
		fmt.Fprintf(w, "\n%s\n\t%s:%d", fr.Function, fr.File, fr.Line)
		if !more {
			break
		}
	}
}

// Frame is a single resolved stack frame. Field names match the subset of
// sentry.Frame (via cockroachdb's ReportableStackTrace) that this project reads.
type Frame struct {
	Filename string
	Lineno   int
	Function string
}

// ReportableStackTrace mirrors the shape of cockroachdb/errors' ReportableStackTrace
// (a sentry stack trace) for the fields helpers.MarshalStack consumes. Frames are
// ordered oldest-call first, matching that library's output.
type ReportableStackTrace struct {
	Frames []Frame
}

// findStack walks err's chain and returns the first non-empty captured stack, or
// nil. Passthrough wrappers (a withStack with a nil stack) are skipped.
func findStack(err error) *stack {
	for e := err; e != nil; e = stderrors.Unwrap(e) {
		if t, ok := e.(stackTracer); ok {
			if st := t.stackTrace(); st != nil && len(*st) > 0 {
				return st
			}
		}
	}
	return nil
}

// GetReportableStackTrace returns the stack trace carried by err (searching the
// chain), or nil if none. Frames are returned oldest-call first.
func GetReportableStackTrace(err error) *ReportableStackTrace {
	st := findStack(err)
	if st == nil {
		return nil
	}

	// runtime yields most-recent first; collect then reverse to oldest first.
	var recent []Frame
	frames := runtime.CallersFrames(*st)
	for {
		fr, more := frames.Next()
		recent = append(recent, Frame{Filename: fr.File, Lineno: fr.Line, Function: fr.Function})
		if !more {
			break
		}
	}

	out := make([]Frame, len(recent))
	for i, f := range recent {
		out[len(recent)-1-i] = f
	}
	return &ReportableStackTrace{Frames: out}
}
