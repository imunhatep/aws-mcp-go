// Package errors is a minimal, dependency-free stand-in for the subset of
// github.com/cockroachdb/errors this project actually uses.
//
// cockroachdb/errors is an excellent library, but it drags in getsentry/sentry-go,
// cockroachdb/redact, cockroachdb/logtags and kr/pretty purely to power features
// (Sentry reporting, redaction, protobuf-encodable errors) that this CLI never
// touches — adding ~20MB to the binary for no benefit. This package reimplements
// only the eight symbols the codebase calls:
//
//	New, Errorf, Wrap, Wrapf, WithStack, Is, As, GetReportableStackTrace
//
// The one behaviour that matters beyond the standard library is single stack-trace
// capture: annotating an error at several layers must not record (and therefore not
// print) the origin stack more than once. That is the reason this repo left
// pkg/errors, and it is preserved here — a stack is captured only when the error
// chain does not already carry one. See errors_test.go / helpers.MarshalStack.
package errors

import (
	stderrors "errors"
	"fmt"
	"io"
)

// stackTracer is implemented by errors in this package that carry a captured stack.
type stackTracer interface {
	stackTrace() *stack
}

// hasStack reports whether any error in the chain already carries a stack, so
// wrapping helpers can avoid capturing (and later printing) a duplicate trace.
func hasStack(err error) bool {
	for err != nil {
		if _, ok := err.(stackTracer); ok {
			return true
		}
		err = stderrors.Unwrap(err)
	}
	return false
}

// New returns an error with the supplied message, recording the call stack at the
// point New was called.
func New(msg string) error {
	return &fundamental{msg: msg, stack: callers(0)}
}

// Errorf formats according to fmt.Errorf (including %w cause chaining) and, unless
// the resulting chain already carries a stack, records the call stack.
func Errorf(format string, args ...any) error {
	return withStackIfMissing(fmt.Errorf(format, args...))
}

// WithStack annotates err with a stack trace at the point WithStack was called,
// unless err already carries one. It returns nil if err is nil.
func WithStack(err error) error {
	if err == nil {
		return nil
	}
	return withStackIfMissing(err)
}

// Wrap returns an error annotating err with msg and, unless err already carries a
// stack, a stack trace at the point Wrap was called. It returns nil if err is nil.
func Wrap(err error, msg string) error {
	if err == nil {
		return nil
	}
	return withStackIfMissing(&withMessage{cause: err, msg: msg})
}

// Wrapf is like Wrap with a formatted message.
func Wrapf(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return withStackIfMissing(&withMessage{cause: err, msg: fmt.Sprintf(format, args...)})
}

// Is reports whether any error in err's chain matches target (stdlib semantics).
func Is(err, target error) bool { return stderrors.Is(err, target) }

// As finds the first error in err's chain that matches target (stdlib semantics).
func As(err error, target any) bool { return stderrors.As(err, target) }

// withStackIfMissing returns a Format-capable error (so %+v is under this package's
// control) carrying a stack. If the chain already has a stack it does not capture a
// new one — but it still ensures the top of the chain formats correctly, wrapping a
// foreign error (e.g. fmt.Errorf's %w wrapper, which has no Format method) so %+v
// still reaches the single underlying stack.
func withStackIfMissing(err error) error {
	if hasStack(err) {
		if isFormatted(err) {
			return err
		}
		return &withStack{cause: err} // no new stack; gain %+v control
	}
	return &withStack{cause: err, stack: callers(1)}
}

// isFormatted reports whether err is one of this package's Format-capable wrappers.
func isFormatted(err error) bool {
	switch err.(type) {
	case *withStack, *withMessage, *fundamental:
		return true
	}
	return false
}

// formatVerbose renders err under "%+v": the full (flattened) message followed by
// the single stack trace found anywhere in the chain. Flattening the message rather
// than recursing keeps output correct even when a foreign error sits in the chain.
func formatVerbose(s fmt.State, err error) {
	io.WriteString(s, err.Error())
	if st := findStack(err); st != nil {
		st.format(s)
	}
}

// fundamental is a leaf error carrying a message and the stack where it originated.
type fundamental struct {
	msg   string
	stack *stack
}

func (f *fundamental) Error() string      { return f.msg }
func (f *fundamental) stackTrace() *stack { return f.stack }
func (f *fundamental) Format(s fmt.State, verb rune) {
	switch verb {
	case 'v':
		if s.Flag('+') {
			formatVerbose(s, f)
			return
		}
		io.WriteString(s, f.msg)
	case 's':
		io.WriteString(s, f.msg)
	case 'q':
		fmt.Fprintf(s, "%q", f.msg)
	}
}

// withStack annotates a cause with a stack trace without changing its message. A
// nil stack means this node only exists to give the chain a Format-capable top.
type withStack struct {
	cause error
	stack *stack
}

func (w *withStack) Error() string      { return w.cause.Error() }
func (w *withStack) Unwrap() error      { return w.cause }
func (w *withStack) stackTrace() *stack { return w.stack }
func (w *withStack) Format(s fmt.State, verb rune) {
	switch verb {
	case 'v':
		if s.Flag('+') {
			formatVerbose(s, w)
			return
		}
		io.WriteString(s, w.Error())
	case 's':
		io.WriteString(s, w.Error())
	case 'q':
		fmt.Fprintf(s, "%q", w.Error())
	}
}

// withMessage prefixes a cause with an explanatory message.
type withMessage struct {
	cause error
	msg   string
}

func (w *withMessage) Error() string { return w.msg + ": " + w.cause.Error() }
func (w *withMessage) Unwrap() error { return w.cause }
func (w *withMessage) Format(s fmt.State, verb rune) {
	switch verb {
	case 'v':
		if s.Flag('+') {
			formatVerbose(s, w)
			return
		}
		io.WriteString(s, w.Error())
	case 's', 'q':
		io.WriteString(s, w.Error())
	}
}
