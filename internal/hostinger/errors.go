package hostinger

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalidToken means Hostinger refused the API token (HTTP 401).
// Retrying will not help until the token is replaced.
var ErrInvalidToken = errors.New("hostinger: the API token was refused (HTTP 401)")

// APIError is a non-2xx answer from the Hostinger API.
type APIError struct {
	Operation  string
	StatusCode int
	// Code is the Hostinger error code from the message, like "DNS:4005".
	Code    string
	Message string
	// Fields holds the per-field messages of an HTTP 422 answer.
	Fields map[string][]string
	// CorrelationID identifies the request at Hostinger. Quote it when you
	// ask Hostinger support about a failed call.
	CorrelationID string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "hostinger %s: HTTP %d: %s", e.Operation, e.StatusCode, msg)
	if len(e.Fields) > 0 {
		keys := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+": "+strings.Join(e.Fields[k], " "))
		}
		// The message already holds the first field error; list all of
		// them only when there are several.
		if len(parts) > 1 {
			fmt.Fprintf(&b, " [%s]", truncate(strings.Join(parts, "; ")))
		}
	}
	if e.CorrelationID != "" {
		fmt.Fprintf(&b, " (correlation_id %s)", e.CorrelationID)
	}
	return b.String()
}

// Temporary reports whether the same request may succeed later.
func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// IsValidation reports whether err is an HTTP 422: Hostinger refused the
// content of the request. A batch with one bad RRset fails as a whole.
func IsValidation(err error) bool { return hasStatus(err, http.StatusUnprocessableEntity) }

// IsNotOwned reports whether err means the domain is not in this Hostinger
// account (HTTP 403 "[DNS:4002] Customer does not own ... domain").
func IsNotOwned(err error) bool { return hasStatus(err, http.StatusForbidden) }

// IsNotFound reports whether err is an HTTP 404.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsTemporary reports whether err may go away on its own: rate limits,
// server errors and network problems. Invalid input and a refused token
// are not temporary.
func IsTemporary(err error) bool {
	if err == nil || errors.Is(err, ErrInvalidToken) {
		return false
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Temporary()
	}
	// Anything else (timeouts, connection resets, DNS failures) is treated
	// as temporary so that external-dns tries again on its next loop.
	return true
}

func hasStatus(err error, code int) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.StatusCode == code
}

var errorCodeRE = regexp.MustCompile(`^\[([A-Za-z]+:\d+)\]\s*`)

// parseError builds an APIError from an error answer. It never keeps more
// than 512 bytes of text, so a huge HTML error page cannot flood the logs.
func parseError(op string, status int, body []byte) *APIError {
	e := &APIError{Operation: op, StatusCode: status}
	var parsed apiErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil && (parsed.Message != "" || parsed.CorrelationID != "") {
		msg := strings.TrimSpace(parsed.Message)
		if m := errorCodeRE.FindStringSubmatch(msg); m != nil {
			e.Code = m[1]
			msg = msg[len(m[0]):]
		}
		e.Message = truncate(msg)
		e.Fields = parsed.Errors
		e.CorrelationID = parsed.CorrelationID
		return e
	}
	e.Message = truncate(strings.TrimSpace(string(body)))
	return e
}

func truncate(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
