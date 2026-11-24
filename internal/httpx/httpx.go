// Package httpx holds the JSON API's response and request helpers.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxBody is how big a request body Decode reads.
const MaxBody = 64 << 10

// WriteJSON writes v as JSON with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// WriteError writes the API's error shape: {"error": {"code", "message"}}.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// Decode reads a JSON request body (at most MaxBody, 64 KB; an empty body is
// {}) into v, writing an error and returning false if it can't.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return DecodeLimit(w, r, v, MaxBody)
}

// DecodeLimit is Decode with a body of at most limit bytes, for the few
// routes whose bodies carry lists that outgrow 64 KB by design (an emit to
// 1,000 recipients).
func DecodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("unexpected data after the JSON value")
	}
	var tooBig *http.MaxBytesError
	switch {
	case err == nil || errors.Is(err, io.EOF):
		return true
	case errors.As(err, &tooBig):
		WriteError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body is limited to %d KB", limit>>10))
	default:
		WriteError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
	}
	return false
}
