// Package fault gives errors a kind (invalid input, not found, conflict,
// failed operation) and a stable machine code, so the API can answer with
// the right HTTP status and error code without knowing every package.
package fault

import (
	"errors"
	"fmt"
)

// Kind says what went wrong, independent of the message.
type Kind int

// Kinds.
const (
	// Internal is anything without a kind: a bug or an I/O failure.
	Internal Kind = iota
	// BadRequest means the request could not be read at all.
	BadRequest
	// Invalid means a value was read but is not acceptable.
	Invalid
	// NotFound means the named thing does not exist.
	NotFound
	// Conflict means the current state does not allow the request (already
	// running, in use, already exists).
	Conflict
	// Failed means the request was carried out but the result is a failure
	// (a server that never became ready, an update that was rolled back).
	Failed
)

// Error is an error with a kind and a machine code.
type Error struct {
	Kind Kind
	Code string // snake_case, stable; empty means the kind's default
	Msg  string
	Err  error // optional cause
}

func (e *Error) Error() string {
	if e.Err != nil && e.Msg == "" {
		return e.Err.Error()
	}
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// New makes an error of kind with a code and message.
func New(kind Kind, code, format string, args ...any) error {
	return &Error{Kind: kind, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Wrap gives err a kind and code, keeping it as the cause.
func Wrap(kind Kind, code string, err error, format string, args ...any) error {
	return &Error{Kind: kind, Code: code, Msg: fmt.Sprintf(format, args...), Err: err}
}

// Invalidf is an Invalid error with code "invalid".
func Invalidf(format string, args ...any) error {
	return New(Invalid, "invalid", format, args...)
}

// NotFoundf is a NotFound error with code "not_found".
func NotFoundf(format string, args ...any) error {
	return New(NotFound, "not_found", format, args...)
}

// Conflictf is a Conflict error with code "conflict".
func Conflictf(format string, args ...any) error {
	return New(Conflict, "conflict", format, args...)
}

// BadRequestf is a BadRequest error with code "bad_request".
func BadRequestf(format string, args ...any) error {
	return New(BadRequest, "bad_request", format, args...)
}

// KindOf returns the kind of the first *Error in err's chain, or Internal.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Internal
}

// CodeOf returns the machine code of err: the code of the first *Error in
// its chain, else the default code of its kind.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Code != "" {
		return e.Code
	}
	return KindOf(err).DefaultCode()
}

// DefaultCode is the code used for a kind when none is given.
func (k Kind) DefaultCode() string {
	switch k {
	case BadRequest:
		return "bad_request"
	case Invalid:
		return "invalid"
	case NotFound:
		return "not_found"
	case Conflict:
		return "conflict"
	case Failed:
		return "failed"
	}
	return "internal"
}

// Is reports whether err has kind k.
func Is(err error, k Kind) bool { return err != nil && KindOf(err) == k }
