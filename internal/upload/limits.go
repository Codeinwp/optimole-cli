package upload

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Limits from the upload API contract. The server stays the authority; these
// only let the CLI skip files it knows will be refused.
const (
	// MaxFileSize is the largest accepted file: 100 MB.
	MaxFileSize int64 = 100 * 1024 * 1024
	// MaxFilenameLength is the longest accepted file name, in characters.
	MaxFilenameLength = 255
	// MaxFolderDepth is the most segments a folder path may have.
	MaxFolderDepth = 20
	// MaxFolderNameLength is the longest folder name, in characters.
	MaxFolderNameLength = 255
)

// allowedExtensions is the contract's list of extensions the server signs.
var allowedExtensions = toSet(
	// Images
	"gif", "jpg", "jpeg", "jpe", "png", "webp", "avif", "svg", "svgz",
	// Video
	"asf", "asx", "wmv", "wmx", "wm", "avi", "divx", "flv", "mov", "qt", "mpeg", "mpg", "mpe", "mp4", "m4v",
	"ogv", "webm", "mkv", "3gp", "3gpp", "3g2", "3gp2",
	// Audio
	"mp3", "m4a", "m4b", "aac", "ra", "ram", "wav", "ogg", "oga", "flac", "mid", "midi", "wma", "wax", "mka",
	// Text
	"txt", "asc", "c", "cc", "h", "srt", "csv", "tsv", "ics", "rtx", "css", "vtt",
	// Documents and archives
	"rtf", "pdf", "json", "tar", "zip", "gz", "gzip", "rar", "7z", "psd", "xcf", "doc", "pot", "pps", "ppt",
	"wri", "xla", "xls", "xlt", "xlw", "mdb", "mpp", "docx", "docm", "dotx", "dotm", "xlsx", "xlsm", "xlsb",
	"xltx", "xltm", "xlam", "pptx", "pptm", "ppsx", "ppsm", "potx", "potm", "ppam", "sldx", "sldm", "onetoc",
	"onetoc2", "onetmp", "onepkg", "oxps", "xps", "odt", "odp", "ods", "odg", "odc", "odb", "odf", "wp", "wpd",
	"key", "numbers", "pages",
)

// blockedExtensions are refused outright by the server.
var blockedExtensions = toSet("htm", "html", "js", "swf", "exe", "class", "dfxp")

func toSet(values ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(values))
	for _, v := range values {
		m[v] = struct{}{}
	}
	return m
}

// CheckFilename reports why the server would refuse a file name, or nil.
// Extensions are compared case-insensitively.
func CheckFilename(name string) error {
	if utf8.RuneCountInString(name) > MaxFilenameLength {
		return fmt.Errorf("file name longer than %d characters", MaxFilenameLength)
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" {
		return errors.New("no file extension")
	}
	if _, ok := blockedExtensions[ext]; ok {
		return fmt.Errorf("blocked file type .%s", ext)
	}
	if _, ok := allowedExtensions[ext]; !ok {
		return fmt.Errorf("unsupported file type .%s", ext)
	}
	return nil
}

// CheckSize reports why the server would refuse a file size, or nil.
func CheckSize(size int64) error {
	switch {
	case size <= 0:
		return errors.New("empty file")
	case size > MaxFileSize:
		return fmt.Errorf("larger than the 100 MB limit (%s)", HumanSize(size))
	}
	return nil
}

// NormalizeFolderPath cleans a folder path the way the server reads it:
// "/"-separated, leading and trailing slashes dropped, each segment trimmed.
// It rejects empty segments, segments over 255 characters and paths over 20
// segments. "" means the library root.
func NormalizeFolderPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxFolderDepth {
		return "", fmt.Errorf("folder path %q has %d levels; the limit is %d", p, len(segs), MaxFolderDepth)
	}
	for i, s := range segs {
		s = strings.TrimSpace(s)
		if s == "" {
			return "", fmt.Errorf("folder path %q has an empty folder name", p)
		}
		if utf8.RuneCountInString(s) > MaxFolderNameLength {
			return "", fmt.Errorf("folder name %q is longer than %d characters", s, MaxFolderNameLength)
		}
		segs[i] = s
	}
	return strings.Join(segs, "/"), nil
}

// HumanSize formats a byte count, e.g. "1.5 MB".
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
