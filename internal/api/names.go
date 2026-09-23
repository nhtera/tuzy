package api

import (
	"context"
	"net/http"
	"net/url"
)

// Name is one pinned name.
type Name struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Default    bool   `json:"default"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt *int64 `json:"last_seen_at"`
	State      string `json:"state,omitempty"` // with live=1: online | offline | never_connected | unknown
}

// HeldName is a name the caller released that is held for them (reclaim with names add).
type HeldName struct {
	Name      string  `json:"name"`
	Until     int64   `json:"until"`
	RenamedTo *string `json:"renamed_to"`
}

// NameList is the answer to ListNames.
type NameList struct {
	Names []Name     `json:"names"`
	Used  int        `json:"used"`
	Limit int        `json:"limit"`
	Held  []HeldName `json:"held"`
}

// ListNames lists the caller's names (live adds the online/offline state).
func (c *Client) ListNames(ctx context.Context, live bool) (NameList, error) {
	path := "/names"
	if live {
		path += "?live=1"
	}
	var out NameList
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// SuggestName returns a currently free auto name.
func (c *Client) SuggestName(ctx context.Context) (string, error) {
	var out struct {
		Name string `json:"name"`
	}
	err := c.do(ctx, http.MethodGet, "/names/suggest", nil, &out)
	return out.Name, err
}

// AddName reserves name ("" = pick an auto name).
func (c *Client) AddName(ctx context.Context, name string) (Name, error) {
	in := map[string]any{"name": name}
	if name == "" {
		in = map[string]any{"auto": true}
	}
	var out Name
	err := c.do(ctx, http.MethodPost, "/names", in, &out)
	return out, err
}

// RenameName renames old to new (the running agent follows automatically).
func (c *Client) RenameName(ctx context.Context, oldName, newName string) (Name, error) {
	var out Name
	err := c.do(ctx, http.MethodPatch, "/names/"+url.PathEscape(oldName), map[string]string{"rename": newName}, &out)
	return out, err
}

// SetDefaultName makes name the default for `tuzy http`.
func (c *Client) SetDefaultName(ctx context.Context, name string) (Name, error) {
	var out Name
	err := c.do(ctx, http.MethodPatch, "/names/"+url.PathEscape(name), map[string]bool{"default": true}, &out)
	return out, err
}

// RemoveName releases name (held for you for 12 months).
func (c *Client) RemoveName(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/names/"+url.PathEscape(name), nil, nil)
}
