package errors

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is the business error code returned in the API envelope {code, data, message}.
type Code int

const (
	CodeOK               Code = 0
	CodeBadRequest       Code = 40000
	CodeUnauthorized     Code = 40100
	CodeForbidden        Code = 40300
	CodeNotFound         Code = 40400
	CodeGone             Code = 41000
	CodeConflict         Code = 40900
	CodeRateLimited      Code = 42900
	CodeInternal         Code = 50000
	CodeServiceUnavailable Code = 50300
)

// Error carries a business code, an HTTP status, and a human message.
type Error struct {
	Code   Code
	Status int
	Msg    string
	Cause  error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Cause)
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code Code, status int, msg string) *Error {
	return &Error{Code: code, Status: status, Msg: msg}
}

func Wrap(code Code, status int, msg string, cause error) *Error {
	return &Error{Code: code, Status: status, Msg: msg, Cause: cause}
}

func BadRequest(msg string) *Error    { return New(CodeBadRequest, http.StatusBadRequest, msg) }
func Unauthorized(msg string) *Error  { return New(CodeUnauthorized, http.StatusUnauthorized, msg) }
func Forbidden(msg string) *Error     { return New(CodeForbidden, http.StatusForbidden, msg) }
func NotFound(msg string) *Error      { return New(CodeNotFound, http.StatusNotFound, msg) }
func Gone(msg string) *Error          { return New(CodeGone, http.StatusGone, msg) }
func Conflict(msg string) *Error      { return New(CodeConflict, http.StatusConflict, msg) }
func RateLimited(msg string) *Error   { return New(CodeRateLimited, http.StatusTooManyRequests, msg) }
func Internal(msg string) *Error      { return New(CodeInternal, http.StatusInternalServerError, msg) }
func ServiceUnavailable(msg string) *Error {
	return New(CodeServiceUnavailable, http.StatusServiceUnavailable, msg)
}

// As extracts an *Error from any error; returns nil if none.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// Sentinel domain errors shared across the monolith. Transport layers (HTTP,
// MCP JSON-RPC) check these via errors.Is to map uniformly to status codes.
// Kept distinct from the typed *Error above; both coexist in package errors.
var (
	ErrUnauthorized   = errors.New("unauthorized")
	ErrForbidden      = errors.New("forbidden")
	ErrNotFound       = errors.New("not found")
	ErrRateLimited    = errors.New("rate limited")
	ErrInvalidParams  = errors.New("invalid params")
	ErrMethodNotFound = errors.New("method not found")
	ErrScopeDenied    = errors.New("scope denied")
)

// Is reports whether any error in err's chain matches target.
func Is(err, target error) bool { return errors.Is(err, target) }
