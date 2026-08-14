package errors

import (
	stderrors "errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// countStacks walks the error chain and counts nodes carrying a non-empty captured
// stack. The package's central invariant is that this is never more than 1, no
// matter how many times an error is wrapped or re-stacked.
func countStacks(err error) int {
	n := 0
	for e := err; e != nil; e = stderrors.Unwrap(e) {
		if t, ok := e.(stackTracer); ok {
			if st := t.stackTrace(); st != nil && len(*st) > 0 {
				n++
			}
		}
	}
	return n
}

// origin lives in its own function so its frame appears in captured stacks and we
// can assert the full origin trace is recorded (and printed) exactly once.
func origin() error { return New("boom") }

// TestSingleStackCapture is the reason this package exists instead of pkg/errors:
// annotating an error at several layers must record the origin stack only once.
func TestSingleStackCapture(t *testing.T) {
	err := origin()
	err = WithStack(err)
	err = Wrap(err, "outer")

	full := fmt.Sprintf("%+v", err)
	const frame = "errors.origin"
	if got := strings.Count(full, frame); got != 1 {
		t.Errorf("expected origin frame %q exactly once, got %d:\n%s", frame, got, full)
	}
	if !strings.Contains(full, "outer") || !strings.Contains(full, "boom") {
		t.Errorf("expected both messages in output:\n%s", full)
	}
}

// TestNoDuplicateStackOnReWrap is the direct guard the user asked for: repeatedly
// applying WithStack/Wrap/Wrapf/Errorf to an already-stacked error must never add a
// second stack, so the chain always holds exactly one and %+v prints the origin
// trace exactly once. Each case builds a chain then asserts both invariants.
func TestNoDuplicateStackOnReWrap(t *testing.T) {
	cases := map[string]func() error{
		"double WithStack": func() error {
			return WithStack(WithStack(origin()))
		},
		"triple WithStack": func() error {
			return WithStack(WithStack(WithStack(origin())))
		},
		"double Wrap": func() error {
			return Wrap(Wrap(origin(), "a"), "b")
		},
		"double Wrapf": func() error {
			return Wrapf(Wrapf(origin(), "a=%d", 1), "b=%d", 2)
		},
		"WithStack then Wrap then WithStack": func() error {
			return WithStack(Wrap(WithStack(origin()), "mid"))
		},
		"Wrap then WithStack then Wrap": func() error {
			return Wrap(WithStack(Wrap(origin(), "inner")), "outer")
		},
		"Errorf(%w) over stacked, then WithStack": func() error {
			return WithStack(Errorf("ctx: %w", origin()))
		},
		"deeply re-stacked (10x)": func() error {
			err := origin()
			for range 10 {
				err = WithStack(err)
				err = Wrap(err, "layer")
			}
			return err
		},
		"WithStack over a foreign wrapper of a stacked error": func() error {
			// fmt.Errorf(%w) is a foreign (non-Format) wrapper; re-stacking through it
			// must still not add a second stack.
			return WithStack(fmt.Errorf("foreign: %w", WithStack(origin())))
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			err := build()

			if n := countStacks(err); n != 1 {
				t.Errorf("chain holds %d stacks, want exactly 1", n)
			}

			full := fmt.Sprintf("%+v", err)
			if got := strings.Count(full, "errors.origin"); got != 1 {
				t.Errorf("origin frame printed %d times under %%+v, want 1:\n%s", got, full)
			}

			// The reportable trace (used by zerolog) must likewise reflect a single
			// capture — the origin frame appears once, not concatenated per layer.
			st := GetReportableStackTrace(err)
			if st == nil {
				t.Fatal("expected a reportable stack trace")
			}
			originFrames := 0
			for _, f := range st.Frames {
				if strings.Contains(f.Function, "errors.origin") {
					originFrames++
				}
			}
			if originFrames != 1 {
				t.Errorf("reportable trace has %d origin frames, want 1", originFrames)
			}
		})
	}
}

// TestErrorMessage checks the plain-message chaining of Wrap/Wrapf.
func TestErrorMessage(t *testing.T) {
	err := Wrapf(New("boom"), "context %d", 7)
	if got, want := err.Error(), "context 7: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestGetReportableStackTrace verifies a stacked error yields oldest-first frames
// with the fields helpers.MarshalStack reads, and a stackless error yields nil.
func TestGetReportableStackTrace(t *testing.T) {
	st := GetReportableStackTrace(WithStack(origin()))
	if st == nil || len(st.Frames) == 0 {
		t.Fatalf("expected frames for a stacked error, got %#v", st)
	}
	f := st.Frames[len(st.Frames)-1] // most-recent frame, oldest-first ordering
	if f.Function == "" || f.Filename == "" || f.Lineno == 0 {
		t.Errorf("incomplete frame: %#v", f)
	}
	if got := GetReportableStackTrace(fmt.Errorf("plain")); got != nil {
		t.Errorf("expected nil for a stackless error, got %#v", got)
	}
}

// TestIsAs confirms the stdlib passthroughs traverse this package's wrappers.
func TestIsAs(t *testing.T) {
	err := Wrap(WithStack(fmt.Errorf("open: %w", fs.ErrNotExist)), "loading")
	if !Is(err, fs.ErrNotExist) {
		t.Error("Is failed to find wrapped sentinel through package wrappers")
	}

	var perr *fs.PathError
	wrapped := Wrap(&fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}, "ctx")
	if !As(wrapped, &perr) || perr.Path != "/x" {
		t.Errorf("As failed to extract *fs.PathError through wrappers: %v", perr)
	}
}

// TestErrorfWrapKeepsSingleStack ensures Errorf with %w over an already-stacked
// error does not add a second stack.
func TestErrorfWrapKeepsSingleStack(t *testing.T) {
	base := origin()
	err := Errorf("wrapping: %w", base)
	if got := strings.Count(fmt.Sprintf("%+v", err), "errors.origin"); got != 1 {
		t.Errorf("expected single origin frame after Errorf(%%w), got %d", got)
	}
	if !Is(err, base) {
		t.Error("Errorf(%w) broke Is chain")
	}
}

// TestNilPassthrough: wrapping nil returns nil.
func TestNilPassthrough(t *testing.T) {
	if WithStack(nil) != nil || Wrap(nil, "x") != nil || Wrapf(nil, "x") != nil {
		t.Error("wrapping nil must return nil")
	}
}
