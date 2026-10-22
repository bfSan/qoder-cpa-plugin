package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type qoderProxyMode uint8

const (
	qoderProxyInherit qoderProxyMode = iota
	qoderProxyExplicit
	qoderProxyBlocked
)

type qoderProxyRouting struct {
	mode   qoderProxyMode
	client *http.Client
}

var qoderProxyState atomic.Pointer[qoderProxyRouting]

func currentQoderProxy() *qoderProxyRouting {
	if s := qoderProxyState.Load(); s != nil {
		return s
	}
	return &qoderProxyRouting{mode: qoderProxyInherit}
}

// Unknown/invalid explicit routes fail closed. Nothing in an error exposes proxy
// userinfo or request URLs (OAuth URLs may carry nonce/verifier credentials).
func configureQoderProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		qoderProxyState.Store(&qoderProxyRouting{mode: qoderProxyInherit})
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return blockQoderProxy()
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return blockQoderProxy()
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return blockQoderProxy()
		}
	}
	transport := &http.Transport{Proxy: http.ProxyURL(u), MaxIdleConns: 20, MaxIdleConnsPerHost: 5, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	qoderProxyState.Store(&qoderProxyRouting{mode: qoderProxyExplicit, client: &http.Client{Timeout: 120 * time.Second, Transport: qoderSafeTransport{transport}, CheckRedirect: qoderRejectRedirect}})
	return nil
}

func blockQoderProxy() error {
	qoderProxyState.Store(&qoderProxyRouting{mode: qoderProxyBlocked})
	return errors.New("Qoder HTTP disabled: invalid proxy-url configuration")
}

// Only a top-level string is accepted; nested text and comments are not settings.
func configureQoderProxyYAML(raw []byte) error {
	if strings.TrimSpace(string(raw)) == "" {
		return configureQoderProxy("")
	}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return blockQoderProxy()
	}
	root := doc.Content[0]
	seen := false
	value := ""
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != "proxy-url" {
			continue
		}
		if seen || k.Tag != "!!str" || v.Kind != yaml.ScalarNode || v.Tag != "!!str" && v.Tag != "!!null" {
			return blockQoderProxy()
		}
		seen = true
		if v.Tag == "!!str" {
			value = v.Value
		}
	}
	return configureQoderProxy(value)
}

func qoderRejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

type qoderSafeTransport struct{ base http.RoundTripper }

func (t qoderSafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, qoderTransportFailure(req.Context(), err)
	}
	return r, nil
}
func (t qoderSafeTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type qoderHTTPError struct {
	cause   error
	timeout bool
}

func (e *qoderHTTPError) Error() string {
	if e.timeout {
		return "Qoder HTTP request timed out"
	}
	return "Qoder HTTP request failed; no alternate-route replay"
}
func (e *qoderHTTPError) Unwrap() error   { return e.cause }
func (e *qoderHTTPError) Timeout() bool   { return e.timeout }
func (e *qoderHTTPError) Temporary() bool { return e.timeout }
func qoderTransportFailure(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	var te interface{ Timeout() bool }
	return &qoderHTTPError{cause: err, timeout: errors.As(err, &te) && te.Timeout()}
}

// Used by DeviceFlow, refresh and all other buffered upstream requests. It uses
// precisely the same bridge/proxy policy as chat and billing, never a private
// HTTP client whose routing differs from the caller's configured route.
func qoderRoutedHTTPDo(req *http.Request) (*hostHTTPResponse, error) { return hostHTTPDo(req) }

// A missing proxy setting inherits host policy. A malformed supplied setting
// blocks HTTP without unloading the entire plugin or touching other settings.
func configureQoderProxyRequest(raw []byte) error {
	if len(raw) == 0 {
		return configureQoderProxy("")
	}
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return blockQoderProxy()
	}
	return configureQoderProxyYAML(req.ConfigYAML)
}
