package testserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// handleDeleteAssets answers DELETE /api/assets like the dashboard. Only
// full-access keys may delete. The body names the assets with exactly one of
// "keys" (1 to 100, every one the account's, deleted at once: 204), "folder"
// (every asset of its tree and, for a website's folder, the website's assets
// in no folder) or "domain" (every asset of one of the account's websites).
// The last two answer 202 with how many assets go; the dashboard deletes them
// in the background, the fake at once. A deleted asset's key is refused like
// an unknown one, as staging does.
func (s *Server) handleDeleteAssets(w http.ResponseWriter, r *http.Request) {
	ev, injected := s.begin(RouteDeleteAssets, r)
	status := 0
	var drop func()
	defer func() { s.finishThenDrop(ev, status, drop) }()
	if injected != nil && injected.CommitThenClose {
		w, drop = commitThenClose(w)
		injected = nil
	}

	var body struct {
		Keys   json.RawMessage `json:"keys"`
		Folder json.RawMessage `json:"folder"`
		Domain json.RawMessage `json:"domain"`
	}
	raw, _ := io.ReadAll(r.Body)
	decodeErr := json.Unmarshal(raw, &body)
	present := func(m json.RawMessage) bool { return m != nil && !bytes.Equal(m, []byte("null")) }
	var keys []string
	keysErr := json.Unmarshal(body.Keys, &keys)
	var folder int64
	folderErr := json.Unmarshal(body.Folder, &folder)
	var domain string
	domainErr := json.Unmarshal(body.Domain, &domain)
	ev.Keys, ev.FolderID, ev.Domain = keys, folder, domain

	if injected != nil {
		status, drop = writeInjected(w, injected)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		s.violate("DELETE %s: Content-Type %q; the dashboard reads no body without application/json", r.URL.Path, ct)
	}
	key, ok := s.authenticate(w, r)
	if !ok {
		status = http.StatusUnauthorized
		return
	}
	if key.Scoped {
		status = writeJSON(w, http.StatusForbidden, map[string]string{"message": "Full API access required"})
		return
	}
	switch {
	case decodeErr != nil:
		status = validation(w, "keys", "The request body must be JSON.")
		return
	case present(body.Keys) && (keysErr != nil || len(keys) < 1 || len(keys) > 100):
		status = validation(w, "keys", "The keys field must be an array of 1 to 100 items.")
		return
	case present(body.Folder) && (folderErr != nil || folder < 1):
		status = validation(w, "folder", "The folder field must be an integer of at least 1.")
		return
	case present(body.Domain) && (domainErr != nil || len(domain) > 255):
		status = validation(w, "domain", "The domain field must be a string of at most 255 characters.")
		return
	}
	for _, k := range keys {
		if len(k) != 32 {
			status = validation(w, "keys.0", "The keys.0 field must be 32 characters.")
			return
		}
	}
	named := 0
	for _, filled := range []bool{present(body.Keys), present(body.Folder), domain != ""} {
		if filled {
			named++
		}
	}
	if named != 1 {
		status = validation(w, "keys", `Name the files to delete with exactly one of "keys", "folder" and "domain".`)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case keys != nil:
		var missing []string
		seen := map[string]bool{}
		for _, k := range keys {
			upper := strings.ToUpper(k)
			if seen[upper] {
				continue
			}
			seen[upper] = true
			if a := s.assets[strings.ToLower(k)]; a == nil || a.Deleted {
				missing = append(missing, upper)
			}
		}
		if len(missing) > 0 {
			status = validation(w, "keys", "The following keys are invalid or unauthorized: "+strings.Join(missing, ", "))
			return
		}
		for _, k := range keys {
			s.assets[strings.ToLower(k)].Deleted = true
		}
		w.WriteHeader(http.StatusNoContent)
		status = http.StatusNoContent
	case folder > 0:
		f := s.folderLocked(folder)
		if f == nil {
			status = validation(w, "folder", "The selected folder is invalid")
			return
		}
		tree := s.treeLocked(f.ID)
		status = s.deleteMatchingLocked(w, func(a *Asset) bool {
			return tree[a.FolderID] || (f.IsDefault && a.FolderID == 0 && a.Domain == f.Name)
		})
	default:
		d := normalizeDomain(domain)
		if !s.sites[d] {
			status = validation(w, "domain", "The account has no website with this domain.")
			return
		}
		status = s.deleteMatchingLocked(w, func(a *Asset) bool { return a.Domain == d })
	}
}

// deleteMatchingLocked deletes the assets match selects and answers 202 with
// their count.
func (s *Server) deleteMatchingLocked(w http.ResponseWriter, match func(*Asset) bool) int {
	n := 0
	for _, a := range s.assets {
		if !a.Deleted && match(a) {
			a.Deleted = true
			n++
		}
	}
	return writeJSON(w, http.StatusAccepted, map[string]int{"assets": n})
}

// treeLocked returns the IDs of a folder and of every folder below it.
func (s *Server) treeLocked(root int64) map[int64]bool {
	tree := map[int64]bool{root: true}
	for grew := true; grew; {
		grew = false
		for _, f := range s.folders {
			if !tree[f.ID] && f.ParentID != 0 && tree[f.ParentID] {
				tree[f.ID] = true
				grew = true
			}
		}
	}
	return tree
}

// normalizeDomain reads a domain the way the dashboard does: the host of a
// URL or of a bare domain, in lower case and without "www.".
func normalizeDomain(d string) string {
	d = strings.TrimSpace(d)
	if !strings.Contains(d, "://") {
		d = "https://" + d
	}
	u, err := url.Parse(d)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// Deleted returns the keys of the deleted assets, sorted.
func (s *Server) Deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k, a := range s.assets {
		if a.Deleted {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
