// Package aitunmgr is a minimal client for the aitun.cc management REST API.
//
// It handles subdomain registration / listing so samvnc can obtain a
// per-subdomain ait_ tunnel token that aitun-client consumes via -k.
//
// This is the same pattern used by samcommand (github.com/samaidev/samcommand).
// Ported here so samvnc can auto-generate a subdomain on first run without
// requiring the user to manually register one at aitun.cc/dashboard.
package aitunmgr

import (
        "bytes"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "strings"
        "time"
)

// BaseURL is the aitun.cc management API root.
const BaseURL = "https://aitun.cc"

// DefaultAgentToken is the built-in aitun.cc agent token. Override with
// --aitun-agent-token or SAMVNC_AITUN_AGENT_TOKEN.
const DefaultAgentToken = "aitagt_2nE7n3GBERBWzOzL0C8nmMdViXJ-nABi"

// Client talks to https://aitun.cc/aitun-api/* using an agent token.
type Client struct {
        BaseURL string
        Token   string // agent token, aitagt_...
        HTTP    *http.Client
}

// New returns a client with a sane default HTTP timeout.
func New(agentToken string) *Client {
        if agentToken == "" {
                agentToken = DefaultAgentToken
        }
        return &Client{
                BaseURL: BaseURL,
                Token:   agentToken,
                HTTP:    &http.Client{Timeout: 30 * time.Second},
        }
}

// Subdomain describes one registered subdomain (subset of the API fields).
type Subdomain struct {
        ID         string `json:"id"`
        Subdomain  string `json:"subdomain"`
        DomainBase string `json:"domain_base"`
        Token      string `json:"token"`       // ait_... tunnel token
        Status     string `json:"status"`
        Online     bool   `json:"online"`
        DomainURL  string `json:"domain_url"`
}

type listResponse struct {
        Subdomains []Subdomain `json:"subdomains"`
}

type checkResponse struct {
        Available bool   `json:"available"`
        Exists    bool   `json:"exists"`
        Message   string `json:"message"`
}

// List returns every subdomain owned by the agent token's account.
func (c *Client) List(ctx context.Context) ([]Subdomain, error) {
        var out listResponse
        if err := c.do(ctx, http.MethodGet, "/aitun-api/subdomain/list", nil, &out); err != nil {
                return nil, err
        }
        return out.Subdomains, nil
}

// Register creates a new subdomain. domainBase may be "" (defaults to t.aitun.cc).
func (c *Client) Register(ctx context.Context, name, domainBase string) (*Subdomain, error) {
        if domainBase == "" {
                domainBase = "t.aitun.cc"
        }
        body := map[string]string{"name": name, "domain_base": domainBase}
        var raw map[string]json.RawMessage
        if err := c.do(ctx, http.MethodPost, "/aitun-api/subdomain/register", body, &raw); err != nil {
                return nil, err
        }
        var sub Subdomain
        if r, ok := raw["subdomain"]; ok {
                _ = json.Unmarshal(r, &sub)
        }
        if sub.Subdomain == "" {
                sub.Subdomain = name
        }
        if sub.DomainBase == "" {
                sub.DomainBase = domainBase
        }
        if sub.Token == "" {
                if r, ok := raw["token"]; ok {
                        _ = json.Unmarshal(r, &sub.Token)
                }
        }
        if sub.ID == "" {
                if r, ok := raw["id"]; ok {
                        _ = json.Unmarshal(r, &sub.ID)
                }
        }
        if sub.DomainURL == "" {
                if r, ok := raw["domain_url"]; ok {
                        _ = json.Unmarshal(r, &sub.DomainURL)
                }
        }
        if sub.DomainURL == "" {
                sub.DomainURL = name + "." + domainBase
        }
        return &sub, nil
}

// RenewToken resets the subdomain's ait_ token and returns the new one.
func (c *Client) RenewToken(ctx context.Context, id string) (string, error) {
        var raw map[string]json.RawMessage
        if err := c.do(ctx, http.MethodPost, "/aitun-api/subdomain/"+id+"/renew-token", nil, &raw); err != nil {
                return "", err
        }
        var tok string
        if r, ok := raw["token"]; ok {
                _ = json.Unmarshal(r, &tok)
        }
        return tok, nil
}

// EnsureSubdomain returns a subdomain with a usable ait_ tunnel token.
//
// Strategy (simplified — relies on server-side "create-or-get" semantics):
//   Just call Register(name). The server now returns the existing subdomain
//   (with its tunnel token) if a subdomain with the same name was previously
//   registered by the same user account, so we don't need to List + RenewToken
//   any more.
//
// Why no List/RenewToken:
//   The embedded agent token in samvnc only has `subdomain:create` scope.
//   `subdomain:read` would expose every subdomain's tunnel token (which an
//   attacker could use to hijack tunnels), and `subdomain:renew` would let
//   an attacker DOS or hijack any subdomain owned by the account. Both are
//   too dangerous to grant to an embedded token, so we route everything
//   through Register.
//
// On 409 (name taken by a DIFFERENT user), the caller is expected to retry
// with a fresh generated name.
func (c *Client) EnsureSubdomain(ctx context.Context, name, domainBase string, reuse bool) (*Subdomain, error) {
        return c.Register(ctx, name, domainBase)
}

// IsConflict reports whether the error is a 409 (subdomain already taken).
func IsConflict(err error) bool {
        if ae, ok := err.(*APIError); ok {
                return ae.Status == http.StatusConflict
        }
        return false
}

// APIError carries HTTP status + a human-readable message from aitun.cc.
type APIError struct {
        Status  int
        Path    string
        Message string
}

func (e *APIError) Error() string {
        return fmt.Sprintf("aitun API %s -> %d: %s", e.Path, e.Status, e.Message)
}

// do is the single HTTP round-trip helper used by every method.
func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
        var bodyReader io.Reader
        if body != nil {
                b, err := json.Marshal(body)
                if err != nil {
                        return fmt.Errorf("marshal body: %w", err)
                }
                bodyReader = bytes.NewReader(b)
        }

        req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
        if err != nil {
                return err
        }
        if body != nil {
                req.Header.Set("Content-Type", "application/json")
        }
        if c.Token != "" {
                req.Header.Set("Authorization", "Bearer "+c.Token)
        }

        resp, err := c.HTTP.Do(req)
        if err != nil {
                return err
        }
        defer resp.Body.Close()

        raw, err := io.ReadAll(resp.Body)
        if err != nil {
                return fmt.Errorf("read response: %w", err)
        }

        if resp.StatusCode >= 400 {
                var errBody struct {
                        Error   string `json:"error"`
                        Message string `json:"message"`
                }
                msg := strings.TrimSpace(string(raw))
                if json.Unmarshal(raw, &errBody) == nil {
                        if errBody.Error != "" {
                                msg = errBody.Error
                        } else if errBody.Message != "" {
                                msg = errBody.Message
                        }
                }
                return &APIError{Status: resp.StatusCode, Path: path, Message: msg}
        }

        if out == nil || len(raw) == 0 {
                return nil
        }
        if err := json.Unmarshal(raw, out); err != nil {
                return fmt.Errorf("decode response from %s: %w (body=%q)", path, err, string(raw))
        }
        return nil
}
