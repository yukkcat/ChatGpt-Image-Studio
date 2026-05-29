package handler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"chatgpt2api/internal/outboundproxy"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// newChromeTransport returns an http.RoundTripper that mimics Chrome's TLS fingerprint
// and HTTP/2 behavior to pass Cloudflare's bot detection.
func newChromeTransport(proxyURL ...string) http.RoundTripper {
	configuredProxyURL := firstProxyURL(proxyURL...)
	fallbackTransport, err := outboundproxy.NewHTTPTransport(configuredProxyURL)
	if err != nil {
		panic(err)
	}
	tunnelDialContext, err := outboundproxy.NewTunnelDialContext(configuredProxyURL)
	if err != nil {
		panic(err)
	}

	return &chromeTransport{
		fallback:   fallbackTransport,
		tunnelDial: tunnelDialContext,
	}
}

type chromeTransport struct {
	fallback   http.RoundTripper
	tunnelDial func(context.Context, string, string) (net.Conn, error)
}

func (t *chromeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return t.fallback.RoundTrip(req)
	}

	addr := req.URL.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = addr + ":443"
	}
	host := req.URL.Hostname()

	// Each request gets its own TLS connection with Chrome fingerprint.
	conn, err := t.dialTLS(req.Context(), addr, host)
	if err != nil {
		return nil, err
	}

	// Use http2.Transport with custom settings matching Chrome's SETTINGS frame.
	tr := &http2.Transport{
		// Chrome sends these initial settings:
		// HEADER_TABLE_SIZE = 65536
		// ENABLE_PUSH = 0
		// INITIAL_WINDOW_SIZE = 6291456
		// MAX_HEADER_LIST_SIZE = 262144
		MaxHeaderListSize:          262144,
		MaxDecoderHeaderTableSize:  65536,
		MaxEncoderHeaderTableSize:  65536,
		StrictMaxConcurrentStreams: false,
	}

	cc, err := tr.NewClientConn(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("http2 client conn: %w", err)
	}

	resp, err := cc.RoundTrip(req)
	if err != nil {
		conn.Close()
		return nil, err
	}

	resp.Body = &connClosingBody{ReadCloser: resp.Body, conn: conn}
	return resp, nil
}

type connClosingBody struct {
	ReadCloser interface {
		Read([]byte) (int, error)
		Close() error
	}
	conn net.Conn
}

func (b *connClosingBody) Read(p []byte) (int, error) {
	return b.ReadCloser.Read(p)
}

func (b *connClosingBody) Close() error {
	err := b.ReadCloser.Close()
	if b.conn != nil {
		b.conn.Close()
	}
	return err
}

func (t *chromeTransport) dialTLS(ctx context.Context, addr, host string) (net.Conn, error) {
	rawConn, err := t.tunnelDial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	// Use latest Chrome fingerprint for realistic TLS ClientHello.
	// HelloChrome_131 includes proper cipher suites, extensions, key shares,
	// and ALPN that match a real Chrome browser.
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_131)
	if err != nil {
		// Fallback to auto if specific version not available
		spec, _ = utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	}

	tlsConn := utls.UClient(rawConn, &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2", "http/1.1"},
	}, utls.HelloCustom)

	if err := tlsConn.ApplyPreset(&spec); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("apply chrome tls preset: %w", err)
	}

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}

	return tlsConn, nil
}

func firstProxyURL(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
