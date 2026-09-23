package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// AdminReport is one abuse report as the admin API lists it.
type AdminReport struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Category         string  `json:"category"`
	Details          *string `json:"details"`
	ReporterEmail    *string `json:"reporter_email"`
	ReporterLoggedIn int     `json:"reporter_logged_in"`
	ReporterPrefix   string  `json:"reporter_prefix"`
	Status           string  `json:"status"`
	AlertSent        int     `json:"alert_sent"`
	CreatedAt        int64   `json:"created_at"`
	Owner            *string `json:"owner"`
	NameStatus       *string `json:"name_status"`
}

// AdminReports lists abuse reports ("open", "actioned", "dismissed" or "all").
func (c *Client) AdminReports(ctx context.Context, status string) ([]AdminReport, error) {
	var out struct {
		Reports []AdminReport `json:"reports"`
	}
	err := c.do(ctx, http.MethodGet, "/admin/reports?status="+url.QueryEscape(status), nil, &out)
	return out.Reports, err
}

// AdminPost calls an admin mutation (path relative to /admin) and returns the raw JSON answer.
func (c *Client) AdminPost(ctx context.Context, path string, in any) (json.RawMessage, error) {
	if in == nil {
		in = map[string]any{}
	}
	var out json.RawMessage
	err := c.do(ctx, http.MethodPost, "/admin"+path, in, &out)
	return out, err
}

// AdminGet calls an admin read endpoint (path relative to /admin).
func (c *Client) AdminGet(ctx context.Context, path string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/admin"+path, nil, &out)
	return out, err
}
