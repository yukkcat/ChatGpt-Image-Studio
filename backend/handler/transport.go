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
// and properly supports HTTP/2.
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

	// Each request gets its own TLS + HTTP/2 connection.
	// This avoids hitting the server's per-connection concurrent stream limit
	// which causes EOF errors under high concurrency.
	conn, err := t.dialTLS(req.Context(), addr, host)
	if err != nil {
		return nil, err
	}

	tr := &http2.Transport{}
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

	// Wrap response body to close the underlying connection when done reading.
	resp.Body = &connClosingBody{ReadCloser: resp.Body, conn: conn}
	return resp, nil
}

// connClosingBody wraps a response body and closes the underlying TCP connection
// when the body is closed.
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
	conn, err := t.tunnelDial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConn := utls.UClient(conn, &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2", "http/1.1"},
	}, utls.HelloChrome_Auto)

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
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
