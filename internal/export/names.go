package export

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxFolderBytes keeps folder names, suffix included, well inside the 255
// bytes (or UTF-16 units) a path component may have.
const maxFolderBytes = 100

// windowsReserved are device names Windows refuses as a file name, with or
// without an extension.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// folderMap maps each domain of an export to the local folder its archives
// go into. Domain names come from the server, so each becomes one safe path
// segment (see safeSegment). Names that clash case-insensitively, as they do
// on macOS and Windows, get "-2", "-3"... in sorted domain order, so the same
// export maps the same way on every run.
type folderMap struct {
	names map[string]string
	taken map[string]bool
}

// add maps the domains not mapped yet.
func (m *folderMap) add(domains []string) {
	if m.names == nil {
		m.names, m.taken = map[string]string{}, map[string]bool{}
	}
	sorted := append([]string(nil), domains...)
	sort.Strings(sorted)
	for _, d := range sorted {
		if _, done := m.names[d]; done {
			continue
		}
		base := safeSegment(d)
		name := base
		for n := 2; m.taken[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		m.taken[strings.ToLower(name)] = true
		m.names[d] = name
	}
}

// safeSegment turns a domain name into a file name that is valid and inert on
// every platform: letters, digits, ".", "-" and "_" only (anything else
// becomes "_"), at most maxFolderBytes long, no leading or trailing dot, and
// no Windows device name.
func safeSegment(domain string) string {
	var b strings.Builder
	for _, r := range domain {
		if !(r == '.' || r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			r = '_'
		}
		if b.Len()+utf8.RuneLen(r) > maxFolderBytes {
			break
		}
		b.WriteRune(r)
	}
	name := strings.Trim(b.String(), ".")
	if name == "" {
		return "domain"
	}
	stem, _, _ := strings.Cut(name, ".")
	if windowsReserved[strings.ToUpper(stem)] {
		name = "_" + name
	}
	return name
}
