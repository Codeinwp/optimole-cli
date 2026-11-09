package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

func TestDeleteAssets(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	a := srv.AddAsset(testserver.Asset{Filename: "a.jpg"})
	b := srv.AddAsset(testserver.Asset{Filename: "b.jpg"})
	srv.AddAsset(testserver.Asset{Filename: "kept.jpg"})

	if err := c.DeleteAssets(ctx, []string{strings.ToUpper(a), b}); err != nil {
		t.Fatal(err)
	}
	ev := srv.Events()
	if len(ev) != 1 || strings.Join(ev[0].Keys, ",") != a+","+b || ev[0].Status != http.StatusNoContent {
		t.Fatalf("events = %+v", ev)
	}
	if got := strings.Join(srv.Deleted(), ","); got != strings.Join(sortedKeys(a, b), ",") {
		t.Fatalf("deleted = %s", got)
	}

	// One unknown key refuses the request, naming it.
	unknown := strings.Repeat("ab", 16)
	var apiErr *Error
	err := c.DeleteAssets(ctx, []string{a, unknown})
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), strings.ToUpper(unknown)) {
		t.Fatalf("unknown key: %v", err)
	}

	// Checked before anything is sent.
	for _, keys := range [][]string{nil, make([]string, MaxDeleteKeys+1), {"not-a-key"}} {
		for i := range keys {
			if keys[i] == "" {
				keys[i] = a
			}
		}
		if err := c.DeleteAssets(ctx, keys); err == nil {
			t.Errorf("%d keys: no error", len(keys))
		}
	}
	if n := srv.Count(testserver.RouteDeleteAssets); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
}

func sortedKeys(keys ...string) []string {
	if keys[0] > keys[1] {
		keys[0], keys[1] = keys[1], keys[0]
	}
	return keys
}

// TestDeleteAssetsIsNotRepeatedOnceItMayHaveDeleted: a deleted key is
// refused when named again, so a repeat after a lost answer would make a
// deletion that worked look failed. Only a 429 is repeated.
func TestDeleteAssetsIsNotRepeatedOnceItMayHaveDeleted(t *testing.T) {
	srv := testserver.New(t)
	c, sl := newTestClient(t, srv.URL)
	ctx := context.Background()
	a := srv.AddAsset(testserver.Asset{Filename: "a.jpg"})
	srv.Inject(testserver.RouteDeleteAssets, testserver.Response{CommitThenClose: true})
	var netErr *NetworkError
	if err := c.DeleteAssets(ctx, []string{a}); !errors.As(err, &netErr) || !netErr.MaybeCommitted {
		t.Fatalf("lost answer: %v", err)
	}
	if n := srv.Count(testserver.RouteDeleteAssets); n != 1 || len(srv.Deleted()) != 1 {
		t.Fatalf("requests = %d, deleted = %v", n, srv.Deleted())
	}
	var apiErr *Error
	if err := c.DeleteAssets(ctx, []string{a}); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), strings.ToUpper(a)) {
		t.Fatalf("a deleted key named again: %v", err)
	}

	b := srv.AddAsset(testserver.Asset{Filename: "b.jpg"})
	srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 429, Body: `{"message":"Too Many Attempts."}`})
	if err := c.DeleteAssets(ctx, []string{b}); err != nil {
		t.Fatal(err)
	}
	if n := srv.Count(testserver.RouteDeleteAssets); n != 4 || len(sl.all()) != 1 {
		t.Fatalf("requests = %d, sleeps = %d", n, len(sl.all()))
	}
}

func TestDeleteFolderAndSiteAssets(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	campaigns := srv.AddFolder("Campaigns")
	spring := srv.AddFolder("Campaigns/2026/Spring")
	site := srv.AddSite("example.com")
	srv.AddAsset(testserver.Asset{Filename: "a.jpg", FolderID: campaigns})
	srv.AddAsset(testserver.Asset{Filename: "b.jpg", FolderID: spring})
	srv.AddAsset(testserver.Asset{Filename: "root.jpg"})
	srv.AddAsset(testserver.Asset{Filename: "offloaded.jpg", Domain: "example.com"})
	srv.AddAsset(testserver.Asset{Filename: "filed.jpg", Domain: "example.com", FolderID: campaigns})

	n, err := c.DeleteFolderAssets(ctx, spring)
	if err != nil || !n.Valid || n.Value != 1 {
		t.Fatalf("spring: %+v, %v", n, err)
	}
	// A website's folder takes the website's images that are in no folder.
	if n, err = c.DeleteFolderAssets(ctx, site); err != nil || n.Value != 1 {
		t.Fatalf("site folder: %+v, %v", n, err)
	}
	if n, err = c.DeleteSiteAssets(ctx, "https://WWW.Example.com/blog"); err != nil || n.Value != 1 {
		t.Fatalf("site: %+v, %v", n, err)
	}
	if n, err = c.DeleteFolderAssets(ctx, campaigns); err != nil || n.Value != 1 {
		t.Fatalf("campaigns: %+v, %v", n, err)
	}
	ev := srv.Events()
	if ev[0].FolderID != spring || ev[2].Domain != "https://WWW.Example.com/blog" || ev[0].Status != http.StatusAccepted {
		t.Fatalf("events = %+v", ev)
	}
	if n := len(srv.Deleted()); n != 4 {
		t.Fatalf("deleted %d, want 4 (root.jpg stays)", n)
	}

	var apiErr *Error
	if _, err := c.DeleteSiteAssets(ctx, "other.com"); !errors.As(err, &apiErr) || !apiErr.HasField("domain") {
		t.Fatalf("unknown site: %v", err)
	}
	if _, err := c.DeleteFolderAssets(ctx, 999); !errors.As(err, &apiErr) || !apiErr.HasField("folder") {
		t.Fatalf("unknown folder: %v", err)
	}
	if _, err := c.DeleteFolderAssets(ctx, 0); err == nil {
		t.Fatal("folder 0: no error")
	}
	if _, err := c.DeleteSiteAssets(ctx, " "); err == nil {
		t.Fatal("blank domain: no error")
	}
}

// TestDeleteFolderAssetsIsNotRepeatedOnceItMayHaveStarted: a repeat would
// start a second background deletion, so only a refusal before the action
// (429) is retried.
func TestDeleteFolderAssetsIsNotRepeatedOnceItMayHaveStarted(t *testing.T) {
	for name, resp := range map[string]testserver.Response{
		"lost answer": {CommitThenClose: true},
		"502":         {Status: 502, Body: `{"message":"Bad Gateway"}`},
	} {
		srv := testserver.New(t)
		folder := srv.AddFolder("A")
		srv.Inject(testserver.RouteDeleteAssets, resp)
		c, _ := newTestClient(t, srv.URL)
		_, err := c.DeleteFolderAssets(context.Background(), folder)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		var netErr *NetworkError
		if resp.CommitThenClose && (!errors.As(err, &netErr) || !netErr.MaybeCommitted) {
			t.Fatalf("%s: %v", name, err)
		}
		if n := srv.Count(testserver.RouteDeleteAssets); n != 1 {
			t.Fatalf("%s: requests = %d, want 1", name, n)
		}
	}

	srv := testserver.New(t)
	srv.AddSite("example.com")
	srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 429, Body: `{"message":"Too Many Attempts."}`})
	c, _ := newTestClient(t, srv.URL)
	if _, err := c.DeleteSiteAssets(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if n := srv.Count(testserver.RouteDeleteAssets); n != 2 {
		t.Fatalf("requests after a 429 = %d, want 2", n)
	}
}

func TestDeleteInBackgroundWithoutACount(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 202, Body: `{}`})
	c, _ := newTestClient(t, srv.URL)
	n, err := c.DeleteSiteAssets(context.Background(), "example.com")
	if err != nil || n.Valid {
		t.Fatalf("%+v, %v", n, err)
	}
}

func TestDeleteNeedsAFullAccessKey(t *testing.T) {
	srv := testserver.New(t)
	const scoped = "optml-sScopedKeyForDeletes0123"
	srv.AddKey(scoped, testserver.Key{Scoped: true})
	a := srv.AddAsset(testserver.Asset{Filename: "a.jpg"})
	c, err := New(Options{BaseURL: srv.URL, APIKey: scoped, UserAgent: "optimole-cli/test"})
	if err != nil {
		t.Fatal(err)
	}
	var apiErr *Error
	if err := c.DeleteAssets(context.Background(), []string{a}); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("got %v", err)
	}
	if len(srv.Deleted()) != 0 {
		t.Fatal("a scoped key deleted")
	}
}
