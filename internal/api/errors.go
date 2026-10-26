package api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Error codes the dashboard returns in the "code" field.
const (
	CodeUploadLimitReached       = "upload_limit_reached"
	CodeUploadServiceUnavailable = "upload_service_unavailable"
	CodeUploadNotFound           = "upload_not_found"
	CodeUploadSizeMismatch       = "upload_size_mismatch"
)

// Error is a non-2xx answer from the dashboard API.
type Error struct {
	// Op names the call, e.g. "create asset".
	Op         string
	StatusCode int
	Code       string
	Message    string
	// Fields holds the validation messages of a 422, keyed by field.
	Fields map[string][]string
	// Limit and Count come with upload_limit_reached.
	Limit, Count FlexInt
	// Expected and Actual come with upload_size_mismatch.
	Expected, Actual FlexInt
	// RetryAfter is the parsed Retry-After header, if any.
	RetryAfter time.Duration
	// Attempts is how many times the call was made.
	Attempts int
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	details := []string{fmt.Sprintf("HTTP %d", e.StatusCode)}
	if e.Code != "" {
		details = append(details, e.Code)
	}
	if e.Limit.Valid || e.Count.Valid {
		details = append(details, fmt.Sprintf("limit %s, used %s", flexString(e.Limit), flexString(e.Count)))
	}
	if e.Expected.Valid || e.Actual.Valid {
		details = append(details, fmt.Sprintf("expected %s bytes, storage has %s", flexString(e.Expected), flexString(e.Actual)))
	}
	fields := make([]string, 0, len(e.Fields))
	for f := range e.Fields {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		for _, m := range e.Fields[f] {
			if m != msg {
				details = append(details, f+": "+m)
			}
		}
	}
	if e.Attempts > 1 {
		details = append(details, fmt.Sprintf("after %d attempts", e.Attempts))
	}
	return fmt.Sprintf("%s: %s (%s)", e.Op, msg, strings.Join(details, "; "))
}

// HasField reports whether the 422 body names the given field.
func (e *Error) HasField(name string) bool {
	_, ok := e.Fields[name]
	return ok
}

func flexString(f FlexInt) string {
	if !f.Valid {
		return "?"
	}
	return strconv.FormatInt(f.Value, 10)
}

// NetworkError is a dashboard call that never got an HTTP answer.
type NetworkError struct {
	Op       string
	Err      error
	Attempts int
}

func (e *NetworkError) Error() string {
	if e.Attempts > 1 {
		return fmt.Sprintf("%s: %v (after %d attempts)", e.Op, e.Err, e.Attempts)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *NetworkError) Unwrap() error { return e.Err }

// StorageError is a failed PUT to the presigned storage URL.
type StorageError struct {
	// StatusCode is 0 when no HTTP answer arrived (network error).
	StatusCode int
	Code       string
	Message    string
	Err        error
}

func (e *StorageError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("upload to storage: %v", e.Err)
	}
	details := fmt.Sprintf("HTTP %d", e.StatusCode)
	if e.Code != "" {
		details += "; " + e.Code
	}
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("upload to storage: %s (%s)", msg, details)
}

func (e *StorageError) Unwrap() error { return e.Err }

// Retryable reports whether a fresh asset should be tried: the URL expired or
// the signature did not match (403), storage failed (5xx), or the connection
// broke.
func (e *StorageError) Retryable() bool {
	return e.StatusCode == 0 || e.StatusCode == http.StatusForbidden || e.StatusCode >= 500
}

// parseError builds an *Error from a non-2xx dashboard response.
func parseError(op string, resp *http.Response, body []byte, now time.Time) *Error {
	e := &Error{Op: op, StatusCode: resp.StatusCode}
	e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), now)

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		e.Message = "unexpected redirect"
		if loc, err := resp.Location(); err == nil {
			e.Message += " to " + loc.Scheme + "://" + loc.Host + loc.Path
		}
		e.Message += "; check the API URL"
		return e
	}

	var payload struct {
		Message  string                     `json:"message"`
		Code     string                     `json:"code"`
		Errors   map[string]json.RawMessage `json:"errors"`
		Limit    FlexInt                    `json:"limit"`
		Count    FlexInt                    `json:"count"`
		Expected FlexInt                    `json:"expected"`
		Actual   FlexInt                    `json:"actual"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "no content type"
		}
		e.Message = fmt.Sprintf("unexpected non-JSON response (%s)", ct)
		return e
	}
	e.Message = payload.Message
	e.Code = payload.Code
	e.Limit, e.Count = payload.Limit, payload.Count
	e.Expected, e.Actual = payload.Expected, payload.Actual
	if len(payload.Errors) > 0 {
		e.Fields = make(map[string][]string, len(payload.Errors))
		for field, raw := range payload.Errors {
			var list []string
			if err := json.Unmarshal(raw, &list); err != nil {
				var single string
				if json.Unmarshal(raw, &single) == nil {
					list = []string{single}
				}
			}
			e.Fields[field] = list
		}
	}
	return e
}

// parseRetryAfter reads delta-seconds or an HTTP date. It returns 0 when the
// header is absent or unusable.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	return 0
}

// parseStorageError reads the S3 XML error document, if there is one.
func parseStorageError(status int, body []byte) *StorageError {
	e := &StorageError{StatusCode: status}
	var doc struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &doc) == nil {
		e.Code = strings.TrimSpace(doc.Code)
		e.Message = strings.TrimSpace(doc.Message)
	}
	return e
}

// stripURL drops the request URL from transport errors: for storage it holds
// a presigned signature, and the URL adds nothing to the message.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
