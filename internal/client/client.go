// Package client implements the authenticated Jocasta transport.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// JSON may escape each source byte into six bytes.
const MaxResponse = 100 << 20

type Config struct {
	URL               string `json:"url"`
	TokenFile         string `json:"token_file"`
	CAFile            string `json:"ca_file,omitempty"`
	AllowLoopbackHTTP bool   `json:"allow_loopback_http,omitempty"`
}
type ErrorDetail struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	CurrentRevision  int64  `json:"current_revision,omitempty"`
	ID               string `json:"id,omitempty"`
	Actor            string `json:"actor,omitempty"`
	// Resolve carries the commands that resolve this refusal. The CLI fills
	// it in locally from the fields above; the server never sends it, because
	// the command line is the client's vocabulary and not the API's.
	Resolve []string `json:"resolve,omitempty"`
}
type Error struct {
	Version string      `json:"version"`
	Detail  ErrorDetail `json:"error"`
}

func (e *Error) Error() string { return e.Detail.Code + ": " + e.Detail.Message }
func LoadConfig(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, fmt.Errorf("invalid configuration: %w", e)
	}
	base := filepath.Dir(path)
	for _, p := range []*string{&c.TokenFile, &c.CAFile} {
		if strings.HasPrefix(*p, "~/") {
			h, e := os.UserHomeDir()
			if e != nil {
				return c, e
			}
			*p = filepath.Join(h, (*p)[2:])
		} else if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, nil
}

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

func New(c Config) (*Client, error) {
	u, e := url.Parse(c.URL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("URL must be an absolute server URL without credentials, query or fragment")
	}
	loop := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loop = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.AllowLoopbackHTTP && loop) {
		return nil, errors.New("HTTPS required (HTTP is allowed only for explicit loopback development)")
	}
	b, e := os.ReadFile(c.TokenFile)
	if e != nil {
		return nil, fmt.Errorf("read token file: %w", e)
	}
	token := strings.TrimSpace(string(b))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("token file must contain one nonempty token")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		b, e := os.ReadFile(c.CAFile)
		if e != nil {
			return nil, e
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("CA file contains no certificates")
		}
		tc.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &Client{base: u, token: token, http: &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func RequestID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func (c *Client) Call(ctx context.Context, method, endpoint string, query url.Values, body any, requestID string, out any) error {
	var payload []byte
	var e error
	if body != nil {
		payload, e = json.Marshal(body)
		if e != nil {
			return e
		}
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
	u.RawQuery = query.Encode()
	for attempt := 0; attempt < 3; attempt++ {
		req, e := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if requestID != "" {
			req.Header.Set("Idempotency-Key", requestID)
		}
		resp, err := c.http.Do(req)
		retry := method == http.MethodGet || requestID != ""
		if err == nil {
			b, re := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
			resp.Body.Close()
			if re != nil {
				err = errors.New("incomplete server response")
			} else if len(b) > MaxResponse {
				return errors.New("server response exceeds limit")
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if out == nil {
					return nil
				}
				if e = json.Unmarshal(b, out); e != nil {
					return errors.New("invalid server JSON response")
				}
				return nil
			} else {
				var ae Error
				if json.Unmarshal(b, &ae) != nil || ae.Detail.Code == "" {
					ae = Error{Version: "jocasta/v1", Detail: ErrorDetail{Code: "unavailable", Message: fmt.Sprintf("server returned HTTP %d", resp.StatusCode)}}
				}
				if resp.StatusCode != 502 && resp.StatusCode != 503 && resp.StatusCode != 504 {
					return &ae
				}
				err = &ae
			}
		} else {
			err = errors.New("request failed (connection, TLS, or timeout)")
		}
		if !retry || attempt == 2 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	return errors.New("request failed")
}
