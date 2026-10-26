package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CreateAssetRequest is the body of POST /assets.
type CreateAssetRequest struct {
	Filename      string `json:"filename"`
	Size          int64  `json:"size"`
	FolderID      int64  `json:"folder_id,omitempty"`
	FolderPath    string `json:"folder_path,omitempty"`
	CreateFolders bool   `json:"create_folders,omitempty"`
}

// Asset is the part of the dashboard's AssetResource the CLI uses.
type Asset struct {
	Key      string  `json:"resourceS3"`
	FolderID FlexInt `json:"folder_id"`
	Status   string  `json:"itemStatus"`
	MimeType string  `json:"mimeType"`
	URL      string  `json:"url"`
}

// UploadInstructions tells the client how to send the file bytes to storage.
type UploadInstructions struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt string            `json:"expires_at"`
}

// CreateAssetResponse is the 201 body of POST /assets.
type CreateAssetResponse struct {
	Asset     Asset               `json:"asset"`
	UploadURL string              `json:"uploadUrl"`
	Upload    *UploadInstructions `json:"upload"`
}

// UploadTarget is a validated presigned storage request.
type UploadTarget struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}

// Target returns where and how to send the file bytes: upload.url (falling back
// to uploadUrl), upload.method (PUT when absent) and upload.headers.
func (r *CreateAssetResponse) Target() (UploadTarget, error) {
	t := UploadTarget{URL: r.UploadURL, Method: http.MethodPut}
	if r.Upload != nil {
		if r.Upload.URL != "" {
			t.URL = r.Upload.URL
		}
		if r.Upload.Method != "" {
			t.Method = strings.ToUpper(r.Upload.Method)
		}
		t.Headers = r.Upload.Headers
		if r.Upload.ExpiresAt != "" {
			if ts, err := time.Parse(time.RFC3339, r.Upload.ExpiresAt); err == nil {
				t.ExpiresAt = ts
			}
		}
	}
	if t.URL == "" {
		return UploadTarget{}, errors.New("create asset: the server did not return an upload URL")
	}
	if t.Method != http.MethodPut {
		return UploadTarget{}, fmt.Errorf("create asset: unsupported upload method %q", t.Method)
	}
	return t, nil
}

// Dimensions are the optional width/height hints sent on completion.
type Dimensions struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// FolderQuery holds the query parameters of GET /folders.
type FolderQuery struct {
	// ParentID is "root", a folder ID, or empty for every folder.
	ParentID string
	PerPage  int
	Cursor   string
}

// Folder is one folder returned by GET /folders.
type Folder struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	ParentID  FlexInt `json:"parent_id"`
	Status    string  `json:"status"`
	IsDefault bool    `json:"is_default"`
}

// FolderPage is one cursor page of GET /folders.
type FolderPage struct {
	Data []Folder `json:"data"`
	Meta struct {
		PerPage    int     `json:"per_page"`
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"meta"`
}

// FlexInt decodes an integer sent as a JSON number or a numeric string. Null,
// absent and malformed values leave it unset.
type FlexInt struct {
	Value int64
	Valid bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = FlexInt{}
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*f = FlexInt{}
		return nil
	}
	*f = FlexInt{Value: n, Valid: true}
	return nil
}

// MarshalJSON implements json.Marshaler.
func (f FlexInt) MarshalJSON() ([]byte, error) {
	if !f.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(f.Value)
}
