// Package api serves the service's HTTP interface.
//
// Success is the payload alone under "data"; failure carries a message and an
// explicit "retryable". The retryable flag is the one thing a machine cannot
// infer from a status code — 503 is "come back in a minute" for a starting
// environment and "stop" for a misconfiguration — and an agent that guesses
// wrong either gives up on a transient failure or hammers a permanent one.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

type successBody struct {
	Data any `json:"data"`
}

type errorBody struct {
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

// apiError is a failure with a status, whether retrying could help, and an
// optional Retry-After.
type apiError struct {
	Status     int
	Message    string
	CanRetry   bool
	RetryAfter int // seconds; 0 means no header
	cause      error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return e.Message + ": " + e.cause.Error()
	}
	return e.Message
}

// Errorf builds an apiError with a formatted message.
func Errorf(status int, format string, args ...any) *apiError {
	return &apiError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// BadRequest is the standard malformed-request error.
func BadRequest(format string, args ...any) *apiError {
	return Errorf(http.StatusBadRequest, format, args...)
}

// NotFound is the standard missing-resource error.
func NotFound(what string, args ...any) *apiError {
	return Errorf(http.StatusNotFound, what+" not found", args...)
}

// Wrap attaches an underlying cause, which is logged but never returned.
func (e *apiError) Wrap(err error) *apiError { e.cause = err; return e }

// Retryable marks the error as worth retrying.
func (e *apiError) Retryable() *apiError { e.CanRetry = true; return e }

// WithRetryAfter sets the Retry-After header, in seconds.
func (e *apiError) WithRetryAfter(seconds int) *apiError { e.RetryAfter = seconds; return e }

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	// The status line and headers are already out, so a failed encode cannot be
	// replaced with an error response — the best available signal is a
	// truncated body, which the client's own decode will catch.
	_ = json.NewEncoder(w).Encode(payload)
}

func respond(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, successBody{Data: data})
}

// fail sends an error envelope, logging the underlying cause.
//
// Message is always text a handler wrote deliberately; the cause — which can
// name paths or upstream responses — reaches the log through Error() and is
// never encoded into the response.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		apiErr = Errorf(http.StatusInternalServerError, "internal error").Wrap(err)
	}
	if apiErr.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", apiErr.RetryAfter))
	}

	if apiErr.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "status", apiErr.Status, "error", apiErr.Error())
	} else {
		slog.DebugContext(r.Context(), "request rejected",
			"method", r.Method, "path", r.URL.Path, "status", apiErr.Status, "error", apiErr.Error())
	}

	writeJSON(w, apiErr.Status, errorBody{Error: apiErr.Message, Retryable: apiErr.CanRetry})
}

// decodeJSON reads a JSON request body, refusing an oversized one.
func decodeJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(out); err != nil {
		return BadRequest("could not read the request body as JSON: %v", err)
	}
	return nil
}
