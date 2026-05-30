package handler

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// newChromeTransport returns an http.RoundTripper that uses tls-client
// to fully impersonate a Chrome browser (TLS + HTTP/2 fingerprint).
// This passes Cloudflare's bot detection without needing a proxy.
func newChromeTransport(proxyURL ...string) http.RoundTripper {
	configuredProxy := firstProxyURL(proxyURL...)
	return &chromeTransport{
		proxyURL: configuredProxy,
	}
}

type chromeTransport struct {
	proxyURL string
}

func (t *chromeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Create a new tls-client for each request to avoid connection sharing issues
	// under high concurrency.
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(120),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithInsecureSkipVerify(),
	}
	if t.proxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(t.proxyURL))
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("create tls client: %w", err)
	}

	// Convert standard http.Request to fhttp.Request
	fReq, err := convertRequest(req)
	if err != nil {
		return nil, fmt.Errorf("convert request: %w", err)
	}

	// Execute request
	fResp, err := client.Do(fReq)
	if err != nil {
		return nil, err
	}

	// Convert fhttp.Response back to standard http.Response
	return convertResponse(fResp), nil
}

// convertRequest converts a standard net/http Request to fhttp Request.
func convertRequest(req *http.Request) (*fhttp.Request, error) {
	var body io.Reader
	if req.Body != nil {
		body = req.Body
	}

	fReq, err := fhttp.NewRequest(req.Method, req.URL.String(), body)
	if err != nil {
		return nil, err
	}

	// Copy headers preserving order
	for key, values := range req.Header {
		for _, value := range values {
			fReq.Header.Add(key, value)
		}
	}

	// Set context for cancellation support
	fReq = fReq.WithContext(req.Context())

	return fReq, nil
}

// convertResponse converts an fhttp Response to standard net/http Response.
func convertResponse(fResp *fhttp.Response) *http.Response {
	resp := &http.Response{
		Status:     fResp.Status,
		StatusCode: fResp.StatusCode,
		Proto:      fResp.Proto,
		ProtoMajor: fResp.ProtoMajor,
		ProtoMinor: fResp.ProtoMinor,
		Header:     http.Header{},
		Body:       fResp.Body,
	}

	for key, values := range fResp.Header {
		for _, value := range values {
			resp.Header.Add(key, value)
		}
	}

	return resp
}

func firstProxyURL(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// Ensure imports are used
var (
	_ = context.Background
	_ = net.Dial
	_ = sync.Once{}
	_ = http2.ErrCodeNo
)
