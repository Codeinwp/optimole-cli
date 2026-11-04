package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// Operation names of the export calls, used as the Op of errors.
const (
	OpCreateExport    = "create export"
	OpGetExport       = "get export"
	OpListExports     = "list exports"
	OpDeleteExport    = "delete export"
	OpDownloadArchive = "download archive"
)

// Export statuses. The last three are final.
const (
	ExportQueued    = "queued"
	ExportRunning   = "running"
	ExportCompleted = "completed"
	ExportFailed    = "failed"
	ExportCancelled = "cancelled"
)

// IsFinalStatus reports whether an export or domain status will not change
// any more.
func IsFinalStatus(status string) bool {
	switch status {
	case ExportCompleted, ExportFailed, ExportCancelled:
		return true
	}
	return false
}

// Export is an asset export. Listing and creating answer with the domain
// names only; GET /asset-exports/{id} adds each domain's status and archives.
type Export struct {
	ID        int64          `json:"id"`
	Status    string         `json:"status"`
	CreatedAt string         `json:"created_at"`
	Domains   []ExportDomain `json:"domains"`
}

// ExportDomain is one domain of an export ("directUpload" holds the files
// uploaded straight to the library).
type ExportDomain struct {
	Domain string `json:"domain"`
	// Status is empty when the answer carried only the name.
	Status string       `json:"status"`
	Parts  []ExportPart `json:"zip_files"`
}

// UnmarshalJSON accepts a bare domain name as well as a domain object.
func (d *ExportDomain) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err == nil {
		*d = ExportDomain{Domain: name}
		return nil
	}
	type plain ExportDomain
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*d = ExportDomain(p)
	return nil
}

// ExportPart is one finished ZIP archive of a domain.
type ExportPart struct {
	Number FlexInt `json:"part_number"`
	// URL is a presigned storage URL, valid for a day. It is a credential
	// for that time: never print it.
	URL  string  `json:"url"`
	Size FlexInt `json:"size"`
}

// ExportQuery holds the query parameters of GET /asset-exports.
type ExportQuery struct {
	PerPage int
	Cursor  string
}

// ExportPage is one cursor page of GET /asset-exports, newest first.
type ExportPage struct {
	Data []Export `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"meta"`
}

// CreateExport starts an export of the whole library. A conflict (one
// already running, or the account keeps the most exports it may) is an
// *Error with status 409 and, on current servers, CodeExportInProgress or
// CodeExportLimitReached.
//
// It is retried like asset creation, only when nothing can have been
// created: a second export would take one of the account's few slots.
func (c *Client) CreateExport(ctx context.Context) (*Export, error) {
	var out Export
	if err := c.doJSON(ctx, OpCreateExport, http.MethodPost, c.endpoint("asset-exports"), struct{}{}, &out, retryUncommitted); err != nil {
		return nil, err
	}
	if out.ID <= 0 {
		return nil, fmt.Errorf("%s: the server returned no export ID", OpCreateExport)
	}
	return &out, nil
}

// GetExport returns an export with its domains and finished archives.
func (c *Client) GetExport(ctx context.Context, id int64) (*Export, error) {
	var out Export
	if err := c.doJSON(ctx, OpGetExport, http.MethodGet, c.exportEndpoint(id), nil, &out, retryIdempotent); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListExports returns one page of the account's exports.
func (c *Client) ListExports(ctx context.Context, q ExportQuery) (*ExportPage, error) {
	u := c.endpoint("asset-exports")
	v := url.Values{}
	if q.PerPage > 0 {
		v.Set("per_page", strconv.Itoa(q.PerPage))
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	u.RawQuery = v.Encode()
	var page ExportPage
	if err := c.doJSON(ctx, OpListExports, http.MethodGet, u, nil, &page, retryIdempotent); err != nil {
		return nil, err
	}
	return &page, nil
}

// DeleteExport deletes a finished export and its archives. A 404 on a
// repeated call counts as success: an earlier attempt that got no answer
// deleted it.
func (c *Client) DeleteExport(ctx context.Context, id int64) error {
	err := c.doJSON(ctx, OpDeleteExport, http.MethodDelete, c.exportEndpoint(id), nil, nil, retryIdempotent)
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && apiErr.Attempts > 1 {
		return nil
	}
	return err
}

func (c *Client) exportEndpoint(id int64) *url.URL {
	return c.endpoint("asset-exports", strconv.FormatInt(id, 10))
}

// Object is the answer to a presigned storage GET.
type Object struct {
	Body io.ReadCloser
	// Offset is the byte the body starts at: the requested offset when
	// storage honoured the range, 0 when it sent the whole object.
	Offset int64
	// Size is the object's total size, or -1 when storage did not say.
	Size int64
}

var contentRangePattern = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+|\*)$`)

// GetObject makes one GET of a presigned storage URL, from byte offset on.
// Like PutObject it never sends the API key and never follows a redirect.
// Failures are *StorageError values; the caller decides whether to retry,
// since it knows how much it already wrote.
func (c *Client) GetObject(ctx context.Context, rawURL string, offset int64) (*Object, error) {
	u, err := checkURL(rawURL)
	if err != nil {
		return nil, &StorageError{Op: OpDownloadArchive, Err: fmt.Errorf("refusing the download URL: %w", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &StorageError{Op: OpDownloadArchive, Err: stripURL(err)}
	}
	req.Header.Set("User-Agent", c.userAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.storage.Do(req)
	if err != nil {
		return nil, &StorageError{Op: OpDownloadArchive, Err: fmt.Errorf("%s: %w", u.Host, stripURL(err))}
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return &Object{Body: resp.Body, Size: resp.ContentLength}, nil
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		m := contentRangePattern.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if m == nil || m[1] != strconv.FormatInt(offset, 10) {
			resp.Body.Close()
			return nil, &StorageError{Op: OpDownloadArchive, StatusCode: resp.StatusCode, Message: "storage answered with an unexpected range"}
		}
		size := int64(-1)
		if total, err := strconv.ParseInt(m[3], 10, 64); err == nil {
			size = total
		}
		return &Object{Body: resp.Body, Offset: offset, Size: size}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &StorageError{Op: OpDownloadArchive, StatusCode: resp.StatusCode, Message: "storage answered with a redirect; refusing to follow it"}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := parseStorageError(resp.StatusCode, body)
	e.Op = OpDownloadArchive
	return nil, e
}
