package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// OpDeleteAssets names DELETE /assets in errors.
const OpDeleteAssets = "delete assets"

// MaxDeleteKeys is how many asset keys one DELETE /assets takes.
const MaxDeleteKeys = 100

// DeleteAssets deletes up to MaxDeleteKeys assets, named by their keys, at
// once. The server checks every key first: one that is not the account's, or
// is deleted already, refuses the whole request with a 422 naming it, and
// nothing is deleted.
//
// It is retried like asset creation, only when the server cannot have
// received it: a repeat after a lost answer would be refused for the keys it
// deleted, and a deletion that worked would look failed.
func (c *Client) DeleteAssets(ctx context.Context, keys []string) error {
	if len(keys) == 0 || len(keys) > MaxDeleteKeys {
		return fmt.Errorf("%s: %d keys given; one request takes 1 to %d", OpDeleteAssets, len(keys), MaxDeleteKeys)
	}
	lower := make([]string, len(keys))
	for i, k := range keys {
		if !assetKeyPattern.MatchString(k) {
			return fmt.Errorf("%s: invalid asset key %q", OpDeleteAssets, k)
		}
		lower[i] = strings.ToLower(k)
	}
	body := struct {
		Keys []string `json:"keys"`
	}{lower}
	return c.doJSON(ctx, OpDeleteAssets, http.MethodDelete, c.endpoint("assets"), body, nil, retryUncommitted)
}

// DeleteFolderAssets starts deleting every asset of a folder and its
// subfolders in the background; for a website's folder, also the website's
// assets that are in no folder. The folders are kept. It returns how many
// assets the server deletes (unset when it did not say).
//
// It is retried like asset creation, only when the server cannot have
// received it: a repeat would start a second background deletion.
func (c *Client) DeleteFolderAssets(ctx context.Context, folderID int64) (FlexInt, error) {
	if folderID <= 0 {
		return FlexInt{}, fmt.Errorf("%s: invalid folder ID %d", OpDeleteAssets, folderID)
	}
	return c.deleteInBackground(ctx, struct {
		Folder int64 `json:"folder"`
	}{folderID})
}

// DeleteSiteAssets starts deleting every asset of one of the account's
// websites, in any folder, in the background, and returns how many assets
// the server deletes (unset when it did not say). It is retried like
// DeleteFolderAssets.
func (c *Client) DeleteSiteAssets(ctx context.Context, domain string) (FlexInt, error) {
	if strings.TrimSpace(domain) == "" {
		return FlexInt{}, fmt.Errorf("%s: no domain", OpDeleteAssets)
	}
	return c.deleteInBackground(ctx, struct {
		Domain string `json:"domain"`
	}{domain})
}

func (c *Client) deleteInBackground(ctx context.Context, body any) (FlexInt, error) {
	var out struct {
		Assets FlexInt `json:"assets"`
	}
	if err := c.doJSON(ctx, OpDeleteAssets, http.MethodDelete, c.endpoint("assets"), body, &out, retryUncommitted); err != nil {
		return FlexInt{}, err
	}
	return out.Assets, nil
}
