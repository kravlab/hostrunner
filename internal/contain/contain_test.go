package contain_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kravlab/hostrunner/internal/contain"
)

// waitTimeout bounds every wait for a containment to end.
const waitTimeout = 5 * time.Second

// waitDone fails t unless ctx is done within waitTimeout.
func waitDone(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(waitTimeout):
		t.Fatal("context not cancelled")
	}
}

// panicOf returns the *PanicError err is, failing t when it is not one.
func panicOf(t *testing.T, err error) *contain.PanicError {
	t.Helper()
	var pe *contain.PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *PanicError", err)
	}
	return pe
}

// A panic of Go's f cancels the containment, and every context derived
// from it, with the panic's *PanicError, which Panic returns.
func TestGoPanicCancelsContainmentWithPanicError(t *testing.T) {
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()
	derived, cancelDerived := context.WithCancel(ctx)
	defer cancelDerived()

	contain.Go(derived, func() { panic("boom") })

	waitDone(t, ctx)
	waitDone(t, derived)
	for _, c := range []context.Context{ctx, derived} {
		pe := panicOf(t, context.Cause(c))
		if pe.Value != "boom" {
			t.Errorf("Value = %v, want boom", pe.Value)
		}
		if !bytes.Contains(pe.Stack, []byte("contain_test.go")) {
			t.Errorf("Stack does not name contain_test.go:\n%s", pe.Stack)
		}
	}
	if got := contain.Panic(derived); got != panicOf(t, context.Cause(ctx)) {
		t.Errorf("Panic = %v, want the cause of ctx", got)
	}
}

// Run contains a panic of f and returns normally.
func TestRunRecoversAndReturns(t *testing.T) {
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()

	contain.Run(ctx, func() { panic("run") })

	waitDone(t, ctx)
	if pe := contain.Panic(ctx); pe == nil || pe.Value != "run" {
		t.Errorf("Panic = %v, want the panic run", pe)
	}
}

// Call returns f's error when no panic was kept, and the panic kept by
// the time f returns, of f or of a goroutine it waited for, otherwise.
func TestCallReturnsTheKeptPanicOrTheError(t *testing.T) {
	fails := errors.New("f failed")
	cases := []struct {
		name string
		f    func(ctx context.Context) error
		want any // the panic value Call returns; nil: fails
	}{
		{"f returns", func(context.Context) error { return fails }, nil},
		{"f panics", func(context.Context) error { panic("call") }, "call"},
		{"its goroutine panics", func(ctx context.Context) error {
			var wg sync.WaitGroup
			contain.GoIn(ctx, &wg, func() { panic("goroutine") })
			wg.Wait()
			return fails
		}, "goroutine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := contain.Within(context.Background(), nil)
			defer cancel()

			err := contain.Call(ctx, func() error { return tc.f(ctx) })

			if tc.want == nil {
				if err != fails {
					t.Errorf("Call = %v, want f's error", err)
				}
				return
			}
			if pe := panicOf(t, err); pe != contain.Panic(ctx) || pe.Value != tc.want {
				t.Errorf("Call = %v, want the panic %v kept", err, tc.want)
			}
		})
	}
}

// Without a containment, Call is f().
func TestCallWithoutContainment(t *testing.T) {
	fails := errors.New("f failed")
	if err := contain.Call(context.Background(), func() error { return fails }); err != fails {
		t.Errorf("Call = %v, want f's error", err)
	}
}

// A panic of AfterFunc's f, run once its context ends, is contained.
func TestAfterFuncPanicIsContained(t *testing.T) {
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()
	trigger, fire := context.WithCancel(ctx)

	contain.AfterFunc(trigger, func() { panic("after") })
	fire()

	waitDone(t, ctx)
	if pe := contain.Panic(ctx); pe == nil || pe.Value != "after" {
		t.Errorf("Panic = %v, want the panic after", pe)
	}
}

// panicWriter is an io.Writer whose Write panics.
type panicWriter struct{}

// Write panics with "write".
func (panicWriter) Write([]byte) (int, error) { panic("write") }

// A Write that panics returns 0 and an error, and the panic is contained.
func TestWriterPanicReturnsErrorAndIsContained(t *testing.T) {
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()

	n, err := contain.Writer(ctx, panicWriter{}).Write([]byte("x"))

	if n != 0 || err == nil {
		t.Errorf("Write = %d, %v, want 0 and an error", n, err)
	}
	waitDone(t, ctx)
	if pe := contain.Panic(ctx); pe == nil || pe.Value != "write" {
		t.Errorf("Panic = %v, want the panic write", pe)
	}
}

// A Write that does not panic is the Write of the writer wrapped.
func TestWriterPassesWriteThrough(t *testing.T) {
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()
	var b strings.Builder

	n, err := contain.Writer(ctx, &b).Write([]byte("out"))

	if n != 3 || err != nil || b.String() != "out" {
		t.Errorf("Write = %d, %v, wrote %q, want 3, nil, out", n, err, b.String())
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("ctx ended: %v", err)
	}
}

// Only the first panic is kept and cancels the containment.
func TestOnlyFirstPanicIsKept(t *testing.T) {
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()
	var wg sync.WaitGroup

	contain.Run(ctx, func() { panic("first") })
	contain.GoIn(ctx, &wg, func() { panic("second") })
	wg.Wait()

	if pe := contain.Panic(ctx); pe == nil || pe.Value != "first" {
		t.Errorf("Panic = %v, want the panic first", pe)
	}
	if pe := panicOf(t, context.Cause(ctx)); pe.Value != "first" {
		t.Errorf("cause Value = %v, want first", pe.Value)
	}
}

// Every panic contained but the first, a later one or one after cancel,
// is passed to others; the first is kept, not passed.
func TestOthersGetsEveryPanicButTheFirst(t *testing.T) {
	var got []any
	ctx, cancel := contain.Within(context.Background(), func(pe *contain.PanicError) { got = append(got, pe.Value) })
	defer cancel()

	contain.Run(ctx, func() { panic("first") })
	contain.Run(ctx, func() { panic("second") })
	cancel()
	contain.Run(ctx, func() { panic("after cancel") })

	if want := []any{"second", "after cancel"}; !slices.Equal(got, want) {
		t.Errorf("others got %v, want %v", got, want)
	}
	if pe := contain.Panic(ctx); pe == nil || pe.Value != "first" {
		t.Errorf("Panic = %v, want the panic first", pe)
	}
}

// A panic contained after cancel, none before it, is passed to others,
// not kept.
func TestPanicAfterCancelGoesToOthers(t *testing.T) {
	var got []any
	ctx, cancel := contain.Within(context.Background(), func(pe *contain.PanicError) { got = append(got, pe.Value) })
	cancel()

	contain.Run(ctx, func() { panic("late") })

	if want := []any{"late"}; !slices.Equal(got, want) {
		t.Errorf("others got %v, want %v", got, want)
	}
	if pe := contain.Panic(ctx); pe != nil {
		t.Errorf("Panic = %v, want nil", pe)
	}
}

// A panic ends its own containment, neither a sibling nor the parent.
func TestPanicDoesNotEndSiblingContainment(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	a, cancelA := contain.Within(parent, nil)
	defer cancelA()
	b, cancelB := contain.Within(parent, nil)
	defer cancelB()

	contain.Run(a, func() { panic("a") })

	waitDone(t, a)
	if err := b.Err(); err != nil {
		t.Errorf("sibling ended: %v", err)
	}
	if pe := contain.Panic(b); pe != nil {
		t.Errorf("sibling Panic = %v, want nil", pe)
	}
	if err := parent.Err(); err != nil {
		t.Errorf("parent ended: %v", err)
	}
}

// GoIn's goroutine is counted in the WaitGroup given.
func TestGoInIsCountedInWaitGroup(t *testing.T) {
	ctx, cancel := contain.Within(context.Background(), nil)
	defer cancel()
	var wg sync.WaitGroup
	ran := false

	contain.GoIn(ctx, &wg, func() { ran = true })
	wg.Wait()

	if !ran {
		t.Error("wg.Wait returned before GoIn's f ran")
	}
}

// Without a containment, Writer returns the writer and Panic nil.
func TestWithoutContainment(t *testing.T) {
	ctx := context.Background()
	var b strings.Builder

	if w := contain.Writer(ctx, &b); w != &b {
		t.Errorf("Writer = %v, want w itself", w)
	}
	if pe := contain.Panic(ctx); pe != nil {
		t.Errorf("Panic = %v, want nil", pe)
	}
}

// A PanicError's Error is "panic: <value>", a blank line and the stack.
func TestPanicErrorFormat(t *testing.T) {
	pe := &contain.PanicError{Value: "boom", Stack: []byte("stack")}

	if got, want := pe.Error(), "panic: boom\n\nstack"; got != want {
		t.Errorf("Error = %q, want %q", got, want)
	}
}
