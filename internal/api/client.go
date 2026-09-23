// Package api is the typed client for the edge REST API (/api/v1).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Error is an API failure: `{"error": {"code", "message", ...}}`.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("server error %d (%s)", e.Status, e.Code)
}

// IsCode reports whether err is an API error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client talks to one server.
type Client struct {
	base      *url.URL
	token     string
	userAgent string
	http      *http.Client
}

// New returns a client for base (e.g. https://tuzy.dev). token may be empty for login calls.
func New(base *url.URL, token, userAgent string) *Client {
	return &Client{
		base: base, token: token, userAgent: userAgent,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
			// Never follow redirects: Go forwards Authorization to subdomains, and *.tuzy.dev are
			// user-controlled tunnels.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// WithToken returns a copy using token.
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.token = token
	return &cp
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	u := *c.base
	p, query, _ := strings.Cut(path, "?")
	u.Path = "/api/v1" + p
	u.RawQuery = query
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", c.base.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return decodeError(resp, b)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("unexpected response from %s: %w", c.base.Host, err)
	}
	return nil
}

func decodeError(resp *http.Response, b []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &env)
	e := &Error{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	if e.Code == "" {
		e.Code = "http_" + strconv.Itoa(resp.StatusCode)
	}
	return e
}

// User is the account behind a token.
type User struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role,omitempty"`
	Trusted   bool   `json:"trusted,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
	MaxNames  int    `json:"max_names,omitempty"`
}

// TokenInfo describes one token.
type TokenInfo struct {
	ID         string `json:"id"`
	Scope      string `json:"scope"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  *int64 `json:"expires_at"`
	LastUsedAt *int64 `json:"last_used_at"`
	Current    bool   `json:"current"`
}

// LoginStart is the answer to StartLogin.
type LoginStart struct {
	LoginID   string `json:"login_id"`
	ExpiresIn int    `json:"expires_in"`
}

// StartLogin emails a code to email.
func (c *Client) StartLogin(ctx context.Context, email string) (LoginStart, error) {
	var out LoginStart
	err := c.do(ctx, http.MethodPost, "/auth/email/start", map[string]string{"email": email}, &out)
	return out, err
}

// VerifyLogin exchanges a code for a new full-scope token.
func (c *Client) VerifyLogin(ctx context.Context, loginID, code, label string) (string, User, error) {
	var out struct {
		Token string `json:"token"`
		User  User   `json:"user"`
	}
	err := c.do(ctx, http.MethodPost, "/auth/email/verify", map[string]string{"login_id": loginID, "code": code, "label": label}, &out)
	return out.Token, out.User, err
}

// Me describes the caller.
type Me struct {
	User  User      `json:"user"`
	Token TokenInfo `json:"token"`
}

// Me returns the current user and token.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var out Me
	err := c.do(ctx, http.MethodGet, "/me", nil, &out)
	return out, err
}

// ListTokens lists the caller's active tokens.
func (c *Client) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	var out struct {
		Tokens []TokenInfo `json:"tokens"`
	}
	err := c.do(ctx, http.MethodGet, "/tokens", nil, &out)
	return out.Tokens, err
}

// NewToken is a freshly created token (the secret is shown once).
type NewToken struct {
	Token     string `json:"token"`
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Label     string `json:"label"`
	ExpiresAt *int64 `json:"expires_at"`
}

// CreateToken creates a token (scope "connect" or "full"; expiresDays 0 = no absolute expiry).
func (c *Client) CreateToken(ctx context.Context, label, scope string, expiresDays int) (NewToken, error) {
	in := map[string]any{"label": label, "scope": scope}
	if expiresDays > 0 {
		in["expires_in_days"] = expiresDays
	}
	var out NewToken
	err := c.do(ctx, http.MethodPost, "/tokens", in, &out)
	return out, err
}

// RevokeToken revokes a token by id ("current" = the caller's own token).
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/tokens/"+url.PathEscape(id), nil, nil)
}

// StartAccountDeletion emails a deletion code.
func (c *Client) StartAccountDeletion(ctx context.Context) (LoginStart, error) {
	var out LoginStart
	err := c.do(ctx, http.MethodPost, "/account/delete/start", map[string]string{}, &out)
	return out, err
}

// ConfirmAccountDeletion deletes the account.
func (c *Client) ConfirmAccountDeletion(ctx context.Context, loginID, code string) error {
	return c.do(ctx, http.MethodPost, "/account/delete/confirm", map[string]string{"login_id": loginID, "code": code}, nil)
}
