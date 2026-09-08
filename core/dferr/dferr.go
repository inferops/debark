// Package dferr is debark's error taxonomy and the single place where an
// error becomes a process exit code. Frozen by ADR-012.
//
// Every command maps its failures onto one of the eight classes below. Core
// packages return errors wrapped with a Class; the CLI is the only caller that
// turns a Class into an exit status.
package dferr

import (
	"errors"
	"fmt"
)

// Class is the error taxonomy. Its numeric value is the process exit code.
type Class int

const (
	// Success: the bundle matches the request.
	Success Class = 0
	// Usage: usage or configuration error (bad flags, malformed config,
	// contradictory options, unreadable input path).
	Usage Class = 1
	// Environment: the machine cannot do the job — no apt, no container
	// runtime, no disk space, no permission.
	Environment Class = 2
	// Incomplete: a bundle was built but is incomplete — a URL failed, or an
	// external .deb has unsatisfiable dependencies. Details land in
	// BuildResult.Unresolved / BuildResult.FetchFailed.
	Incomplete Class = 3
	// Verification: signature, digest or repository metadata mismatch.
	Verification Class = 4
	// Resolution: apt could not satisfy the request.
	Resolution Class = 5
	// Policy: local policy or approved-keys violation.
	Policy Class = 6
	// TargetMismatch: architecture or release mismatch on install.
	TargetMismatch Class = 7
)

// String returns the short stable name used in --json output and evidence.
func (c Class) String() string {
	switch c {
	case Success:
		return "success"
	case Usage:
		return "usage"
	case Environment:
		return "environment"
	case Incomplete:
		return "incomplete"
	case Verification:
		return "verification"
	case Resolution:
		return "resolution"
	case Policy:
		return "policy"
	case TargetMismatch:
		return "target-mismatch"
	default:
		return fmt.Sprintf("unknown(%d)", int(c))
	}
}

// Description is the one-line human meaning of the class, used in help output
// and in the documented exit-code table.
func (c Class) Description() string {
	switch c {
	case Success:
		return "success; bundle matches the request"
	case Usage:
		return "usage or configuration error"
	case Environment:
		return "environment: no apt, no container runtime, no disk, no permission"
	case Incomplete:
		return "bundle built but incomplete: a URL failed or an external .deb has unsatisfiable dependencies"
	case Verification:
		return "verification failed: signature, digest or repository metadata mismatch"
	case Resolution:
		return "resolution failed: apt could not satisfy the request"
	case Policy:
		return "policy violation (local policy or approved-keys)"
	case TargetMismatch:
		return "target mismatch on install: architecture or release"
	default:
		return "unknown"
	}
}

// Classes lists every class in exit-code order, for documentation generation.
func Classes() []Class {
	return []Class{Success, Usage, Environment, Incomplete, Verification, Resolution, Policy, TargetMismatch}
}

// Error carries a class, a human message and an optional wrapped cause. Hint,
// when set, is a concrete next action printed after the message ("install
// docker or podman, or pass --backend=local").
type Error struct {
	Class Class
	Msg   string
	Hint  string
	Err   error
}

func (e *Error) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return e.Msg + ": " + e.Err.Error()
	case e.Msg != "":
		return e.Msg
	case e.Err != nil:
		return e.Err.Error()
	default:
		return e.Class.String()
	}
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a classified error.
func New(c Class, format string, args ...any) *Error {
	return &Error{Class: c, Msg: fmt.Sprintf(format, args...)}
}

// Wrap classifies an existing error. It returns nil when err is nil, so it can
// be used directly in a return statement.
func Wrap(c Class, err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return &Error{Class: c, Msg: fmt.Sprintf(format, args...), Err: err}
}

// WithHint returns a copy of e carrying an actionable hint.
func (e *Error) WithHint(format string, args ...any) *Error {
	c := *e
	c.Hint = fmt.Sprintf(format, args...)
	return &c
}

// ClassOf reports the class of err: the class of the OUTERMOST *Error in its
// chain (errors.As walks outwards-in and stops at the first match), or Usage
// when err is non-nil but carries no *Error at all. A nil error is Success.
//
// Outermost, not innermost, is the answer callers want: the last package to
// classify an error is the one closest to the operator, and it is the one
// that decided this cause means that class *here*. It is also why a caller
// re-wrapping an already-classified cause silently overrides its class —
// wrap conditionally (core/engine's classify) when the cause's own class
// should survive.
func ClassOf(err error) Class {
	if err == nil {
		return Success
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return Usage
}

// ExitCode is ClassOf as an int, for main().
func ExitCode(err error) int { return int(ClassOf(err)) }

// HintOf returns the hint attached to err, if any.
func HintOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Hint
	}
	return ""
}

// Is reports whether err carries class c.
func Is(err error, c Class) bool { return err != nil && ClassOf(err) == c }

// Convenience constructors for the classes used most often.

func Usagef(format string, args ...any) *Error      { return New(Usage, format, args...) }
func Envf(format string, args ...any) *Error        { return New(Environment, format, args...) }
func Incompletef(format string, args ...any) *Error { return New(Incomplete, format, args...) }
func Verifyf(format string, args ...any) *Error     { return New(Verification, format, args...) }
func Resolvef(format string, args ...any) *Error    { return New(Resolution, format, args...) }
func Policyf(format string, args ...any) *Error     { return New(Policy, format, args...) }
func Targetf(format string, args ...any) *Error     { return New(TargetMismatch, format, args...) }
