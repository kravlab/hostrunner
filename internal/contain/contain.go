// Package contain contains panics: a panic in code run under a
// containment ends the containment, by cancelling its context, rather than
// the process. The shared process runs each daemon under one of its own,
// so a panic anywhere in a daemon ends that daemon alone, as
// `hostrunner stop --kill` would (see docs/adr/0010-shared-process.md).
//
// Go recovers a panic only in the goroutine it happened in, so a
// containment covers only code run through this package: Run and Call in
// the calling goroutine, Go and GoIn in a new one, AfterFunc for a
// callback the context package runs, and Writer for the Writes another goroutine, such as the
// one exec.Cmd copies a command's output with, makes. A containment is
// carried in a context, so code reaches it from any context derived from
// Within's. Without one, nothing is recovered: a panic crashes the
// process, as it does outside the shared process.
package contain

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
)

// PanicError is a contained panic.
type PanicError struct {
	// Value is the value the code panicked with.
	Value any
	// Stack is the stack of the goroutine that panicked, taken when the
	// panic was recovered.
	Stack []byte
}

// Error returns "panic: <value>", a blank line and the stack.
func (e *PanicError) Error() string {
	return fmt.Sprintf("panic: %v\n\n%s", e.Value, e.Stack)
}

// key is the context key a containment is stored under.
type key struct{}

// containment is what a context carries: where its first panic is kept,
// where the others go, and how it ends.
type containment struct {
	// others is passed every panic contained but the one kept; nil: they
	// are dropped.
	others func(*PanicError)
	// cancel cancels the containment's context with a cause.
	cancel context.CancelCauseFunc

	mu sync.Mutex
	// first is the panic kept, nil until one is.
	first *PanicError
	// ended is set once Within's cancel was called: no panic is kept
	// after it.
	ended bool
}

// Within returns ctx, derived from parent, under a new containment, and
// its cancel. The first panic contained under ctx, or any context derived
// from it, before cancel was called is kept (see Panic) and cancels ctx
// with its *PanicError as the cause. Every other panic contained, a later
// one or any after cancel was called, is passed to others, which may be
// called from several goroutines at once; with others nil, it is dropped.
func Within(parent context.Context, others func(*PanicError)) (ctx context.Context, cancel context.CancelFunc) {
	c := &containment{others: others}
	ctx, c.cancel = context.WithCancelCause(parent)
	return context.WithValue(ctx, key{}, c), c.end
}

// end ends c: it cancels c's context, and no panic is kept after it.
func (c *containment) end() {
	c.mu.Lock()
	c.ended = true
	c.mu.Unlock()
	c.cancel(context.Canceled)
}

// Panic returns the panic ctx's containment keeps (see Within), nil when
// it keeps none, or when ctx has no containment.
func Panic(ctx context.Context) *PanicError {
	c := from(ctx)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.first
}

// Run calls f in the calling goroutine under ctx's containment: a panic
// of f is contained, and Run then returns normally. Without a
// containment, it is f().
func Run(ctx context.Context, f func()) {
	under(ctx, f)()
}

// Call calls f in the calling goroutine under ctx's containment, as Run
// does, and returns the panic ctx's containment keeps if it kept one by
// the time f returned, of f or not, f's error otherwise. Without a
// containment, it is f().
func Call(ctx context.Context, f func() error) error {
	var err error
	Run(ctx, func() { err = f() })
	if pe := Panic(ctx); pe != nil {
		return pe
	}
	return err
}

// Go runs f in a new goroutine under ctx's containment. Without a
// containment, it is go f().
func Go(ctx context.Context, f func()) {
	go under(ctx, f)()
}

// GoIn runs f in a new goroutine under ctx's containment, counted in wg.
// Without a containment, it is wg.Go(f).
func GoIn(ctx context.Context, wg *sync.WaitGroup, f func()) {
	wg.Go(under(ctx, f))
}

// AfterFunc is context.AfterFunc(ctx, f) with f run under ctx's
// containment.
func AfterFunc(ctx context.Context, f func()) (stop func() bool) {
	return context.AfterFunc(ctx, under(ctx, f))
}

// from returns ctx's containment, nil when it has none.
func from(ctx context.Context) *containment {
	c, _ := ctx.Value(key{}).(*containment)
	return c
}

// under returns f run under ctx's containment, f itself when ctx has
// none.
func under(ctx context.Context, f func()) func() {
	c := from(ctx)
	if c == nil {
		return f
	}
	return c.wrap(f)
}

// wrap returns f run under c: a panic of f is recovered and contained,
// and the returned function then returns normally.
func (c *containment) wrap(f func()) func() {
	return func() {
		defer c.catch(nil)
		f()
	}
}

// catch, deferred, recovers a panic of the function that deferred it and
// contains it (see contain); with err non-nil, it sets *err to the
// panic's *PanicError. It calls recover itself, as recover works only
// when the deferred function calls it directly.
func (c *containment) catch(err *error) {
	if pe := c.contain(recover()); pe != nil && err != nil {
		*err = pe
	}
}

// contain contains r, what recover returned in the goroutine that
// panicked, and returns it as a *PanicError, nil when r is nil (no
// panic). The first panic under c before c ended is kept and cancels c's
// context with it as the cause; any other is passed to c.others, if set.
func (c *containment) contain(r any) *PanicError {
	if r == nil {
		return nil
	}
	pe := &PanicError{Value: r, Stack: debug.Stack()}
	c.mu.Lock()
	keep := c.first == nil && !c.ended
	if keep {
		c.first = pe
	}
	c.mu.Unlock()
	switch {
	case keep:
		c.cancel(pe)
	case c.others != nil:
		c.others(pe)
	}
	return pe
}

// Writer returns w with every Write under ctx's containment: a panic of
// w.Write is contained, and that Write returns 0 and the panic's
// *PanicError instead of panicking. Without a containment, it returns w.
func Writer(ctx context.Context, w io.Writer) io.Writer {
	c := from(ctx)
	if c == nil {
		return w
	}
	return &writer{c: c, w: w}
}

// writer is an io.Writer whose Writes run under a containment.
type writer struct {
	// c is the containment the Writes run under.
	c *containment
	// w is the io.Writer written to.
	w io.Writer
}

// Write writes p to w.w under w.c, returning 0 and the panic's
// *PanicError when w.w.Write panics.
func (w *writer) Write(p []byte) (n int, err error) {
	defer w.c.catch(&err) // n stays 0: w.w.Write did not return
	return w.w.Write(p)
}
