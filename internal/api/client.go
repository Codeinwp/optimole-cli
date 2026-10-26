// Package api is a small client for the parts of the Optimole dashboard API
// the CLI uses: reserving an asset, sending its bytes to the presigned storage
// URL, completing it, and listing folders.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the production dashboard API.
const DefaultBaseURL = "https://dashboard.optimole.com/api"

const (
	// MaxAttempts is how many times a dashboard call is made before giving up.
	MaxAttempts      = 4
	defaultBaseDelay = 500 * time.Millisecond
	defaultMaxDelay  = 8 * time.Second
	// maxRetryAfter caps how long a Retry-After header can make us wait.
	maxRetryAfter = 5 * time.Minute
	// maxResponseBody caps how much of a response is read into memory.
	maxResponseBody  = 1 << 20
	dashboardTimeout = 60 * time.Second
	headerTimeout    = 2 * time.Minute
)

// Operation names, used as the Op of errors.
const (
	OpCreateAsset    = "create asset"
	OpCompleteUpload = "complete upload"
	OpListFolders    = "list folders"
)

var assetKeyPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// Options configures a Client.
type Options struct {
	// BaseURL is the API root, e.g. https://dashboard.optimole.com/api.
	BaseURL string
	APIKey  string
	// UserAgent is sent with every request, storage included.
	UserAgent string
	// Transport carries every request. Nil means a clone of
	// http.DefaultTransport (system proxy settings, TLS verification on).
	Transport http.RoundTripper
	// Sleep waits between retries. Nil means a timer that stops early when
	// the context is done.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client talks to the dashboard API and to presigned storage URLs.
type Client struct {
	base      *url.URL
	apiKey    string
	userAgent string
	dashboard *http.Client
	storage   *http.Client
	sleep     func(context.Context, time.Duration) error
	now       func() time.Time
	baseDelay time.Duration
	maxDelay  time.Duration
}

// New validates the options and returns a Client.
func New(opts Options) (*Client, error) {
	base, err := ParseBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("missing API key")
	}
	transport := opts.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = headerTimeout
		transport = t
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "optimole-cli/dev"
	}
	return &Client{
		base:      base,
		apiKey:    opts.APIKey,
		userAgent: ua,
		dashboard: &http.Client{
			Transport: transport,
			Timeout:   dashboardTimeout,
			// The API never redirects; following one could turn a POST into a
			// GET or send the key somewhere unexpected.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		// No overall timeout: a 100 MB body can take a while. The context and
		// the transport's header timeout bound it instead. Redirects are
		// refused: following one would replay the file to another host.
		storage: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sleep:     sleep,
		now:       time.Now,
		baseDelay: defaultBaseDelay,
		maxDelay:  defaultMaxDelay,
	}, nil
}

// ParseBaseURL validates an API base URL: https, or plain http only for
// localhost and 127.0.0.1. A trailing slash is removed.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := checkURL(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid API URL %q: %w", raw, err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid API URL %q: it must not have a query or fragment", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("not a URL")
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	if u.User != nil {
		return nil, errors.New("it must not contain credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
			return nil, errors.New("it must use https (plain http is allowed only for localhost and 127.0.0.1)")
		}
	default:
		return nil, errors.New("it must use https")
	}
	return u, nil
}

// CreateAsset reserves an asset and returns where to upload its bytes.
func (c *Client) CreateAsset(ctx context.Context, req CreateAssetRequest) (*CreateAssetResponse, error) {
	var out CreateAssetResponse
	if err := c.doJSON(ctx, OpCreateAsset, http.MethodPost, c.endpoint("assets"), req, &out); err != nil {
		return nil, err
	}
	if !assetKeyPattern.MatchString(out.Asset.Key) {
		return nil, errors.New("create asset: the server returned an invalid asset key")
	}
	out.Asset.Key = strings.ToLower(out.Asset.Key)
	return &out, nil
}

// PutObject streams exactly size bytes from r to the presigned URL, with
// every header the server signed. It never sends the API key and never
// follows a redirect.
//
// A network error or a 5xx is retried against the same URL while it is
// unexpired, up to MaxAttempts: the signature pins the key, size and type, so
// sending the bytes again is harmless, whereas a new reservation could publish
// a duplicate. The returned *StorageError says whether an earlier attempt may
// have stored the bytes (Uncertain) and whether retrying stopped because the
// URL expired (Expired).
func (c *Client) PutObject(ctx context.Context, t UploadTarget, r io.ReaderAt, size int64) error {
	u, err := checkURL(t.URL)
	if err != nil {
		return &StorageError{Err: fmt.Errorf("refusing the upload URL: %w", err)}
	}
	if t.Method != "" && t.Method != http.MethodPut {
		return &StorageError{Err: fmt.Errorf("unsupported upload method %q", t.Method)}
	}
	header := http.Header{}
	for k, v := range t.Headers {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length":
			if v != strconv.FormatInt(size, 10) {
				return &StorageError{Err: fmt.Errorf("the upload was signed for %s bytes but the file has %d", v, size)}
			}
		case "Host":
			// The URL names the storage host; a header must not send the bytes elsewhere.
			continue
		default:
			header.Set(k, v)
		}
	}
	header.Set("User-Agent", c.userAgent)

	uncertain := false
	var last *StorageError
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := c.sleep(ctx, c.backoff(attempt-1)); err != nil {
				return err
			}
			if !t.ExpiresAt.IsZero() && !c.now().Before(t.ExpiresAt) {
				last.Expired = true
				break
			}
		}
		err := c.putOnce(ctx, u, header, r, size)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		last = err
		last.Attempts = attempt
		if err.StatusCode != 0 && err.StatusCode < 500 {
			// 403 (expired or mismatched signature), a redirect or another
			// refusal: the same URL will not do better.
			last.Uncertain = uncertain
			return last
		}
		// No answer, or a storage failure: the bytes may or may not be stored.
		uncertain = true
		last.Uncertain = true
	}
	return last
}

// putOnce makes one PUT attempt.
func (c *Client) putOnce(ctx context.Context, u *url.URL, header http.Header, r io.ReaderAt, size int64) *StorageError {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.NewSectionReader(r, 0, size))
	if err != nil {
		return &StorageError{Err: stripURL(err)}
	}
	req.ContentLength = size
	// Lets the transport resend the body on a stale pooled connection to the
	// same host; redirects are refused by the storage client.
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(r, 0, size)), nil
	}
	req.Header = header.Clone()

	resp, err := c.storage.Do(req)
	if err != nil {
		return &StorageError{Err: fmt.Errorf("%s: %w", u.Host, stripURL(err))}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return &StorageError{StatusCode: resp.StatusCode, Message: "storage answered with a redirect; refusing to send the file elsewhere"}
	}
	return parseStorageError(resp.StatusCode, body)
}

// CompleteAsset asks the server to check the uploaded bytes and publish the
// asset. dims, when known, are sent as hints.
func (c *Client) CompleteAsset(ctx context.Context, key string, dims *Dimensions) (*Asset, error) {
	if !assetKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("complete upload: invalid asset key %q", key)
	}
	var body any = struct{}{}
	if dims != nil {
		body = dims
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, OpCompleteUpload, http.MethodPost, c.endpoint("assets", strings.ToLower(key), "completion"), body, &raw); err != nil {
		return nil, err
	}
	return decodeAsset(raw)
}

// decodeAsset accepts a bare AssetResource and, defensively, one wrapped in
// "asset" or "data".
func decodeAsset(raw json.RawMessage) (*Asset, error) {
	var wrapped struct {
		Asset *Asset `json:"asset"`
		Data  *Asset `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("complete upload: invalid JSON response: %w", err)
	}
	if wrapped.Asset != nil {
		return wrapped.Asset, nil
	}
	if wrapped.Data != nil {
		return wrapped.Data, nil
	}
	var a Asset
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("complete upload: invalid JSON response: %w", err)
	}
	return &a, nil
}

// ListFolders returns one page of GET /folders.
func (c *Client) ListFolders(ctx context.Context, q FolderQuery) (*FolderPage, error) {
	u := c.endpoint("folders")
	v := url.Values{}
	if q.PerPage > 0 {
		v.Set("per_page", strconv.Itoa(q.PerPage))
	}
	if q.ParentID != "" {
		v.Set("parent_id", q.ParentID)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	u.RawQuery = v.Encode()
	var page FolderPage
	if err := c.doJSON(ctx, OpListFolders, http.MethodGet, u, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *Client) endpoint(parts ...string) *url.URL {
	return c.base.JoinPath(parts...)
}

// retryableStatus lists the dashboard answers that are worth another try.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// doJSON sends a JSON request to the dashboard and decodes a 2xx answer into
// out. It retries 429 (honouring Retry-After), 502/503/504 and network errors
// with capped, jittered exponential backoff, up to MaxAttempts calls.
func (c *Client) doJSON(ctx context.Context, op, method string, u *url.URL, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		wait, retry, err := c.roundTrip(ctx, op, req, attempt, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || attempt == MaxAttempts {
			break
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
	return lastErr
}

// roundTrip makes one attempt. It returns how long to wait before the next
// attempt and whether one should be made.
func (c *Client) roundTrip(ctx context.Context, op string, req *http.Request, attempt int, out any) (time.Duration, bool, error) {
	resp, err := c.dashboard.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, false, ctxErr
		}
		return c.backoff(attempt), true, &NetworkError{Op: op, Err: stripURL(err), Attempts: attempt}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, false, ctxErr
		}
		return c.backoff(attempt), true, &NetworkError{Op: op, Err: fmt.Errorf("reading the response: %w", err), Attempts: attempt}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return 0, false, nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return 0, false, &Error{Op: op, StatusCode: resp.StatusCode, Message: "invalid JSON response: " + err.Error(), Attempts: attempt}
		}
		return 0, false, nil
	}
	apiErr := parseError(op, resp, body, c.now())
	apiErr.Attempts = attempt
	if !retryableStatus(resp.StatusCode) {
		return 0, false, apiErr
	}
	wait := apiErr.RetryAfter
	if wait <= 0 {
		wait = c.backoff(attempt)
	}
	return wait, true, apiErr
}

// backoff returns the wait after the given failed attempt: exponential from
// baseDelay, capped at maxDelay, with "equal jitter" (between half and all of
// it) so parallel workers do not retry in lockstep.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseDelay << (attempt - 1)
	if d <= 0 || d > c.maxDelay {
		d = c.maxDelay
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
