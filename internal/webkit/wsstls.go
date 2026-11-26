package webkit

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"golang.org/x/net/proxy"
)

// A wss:// connection must have its cutConn above TLS: writeFrame writes
// the WebSocket frames itself, so the connection it writes to has to
// encrypt them. gorilla puts tls.Client above the connection that
// NetDialContext returns, but takes a NetDialTLSContext connection as
// already encrypted. So for wss:// dialCuttable installs tlsDial as the
// NetDialTLSContext, and dialCuttable wraps the tls.Conn.
//
// gorilla uses NetDialTLSContext also as the dial to a proxy, below its
// CONNECT, which would put TLS between us and the proxy instead of through
// the tunnel. So tlsDial runs the proxy step itself, as gorilla's dial did
// without a NetDialTLSContext: an http proxy through CONNECT, a socks5
// proxy through golang.org/x/net/proxy; gorilla supported no other scheme.

// tlsDial returns a NetDialTLSContext for wsURL that dials with dial,
// through the proxy that proxyFor picks for wsURL (nil: none), and runs
// the TLS handshake with a clone of cfg, as gorilla does: ServerName is the
// host when cfg has none, and the host name is verified unless
// InsecureSkipVerify. ctx bounds the proxy step and the handshake.
func tlsDial(wsURL string, dial func(context.Context, string, string) (net.Conn, error), proxyFor func(*http.Request) (*url.URL, error), cfg *tls.Config) (func(context.Context, string, string) (net.Conn, error), error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", wsURL, err)
	}
	pu := *u
	pu.Scheme = "https" // as gorilla asks the proxy function
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var p *url.URL
		if proxyFor != nil {
			var err error
			if p, err = proxyFor(&http.Request{URL: &pu, Header: http.Header{}}); err != nil {
				return nil, err
			}
		}
		raw, err := dialVia(ctx, dial, p, network, addr)
		if err != nil {
			return nil, err
		}
		c := cfg.Clone()
		if c == nil {
			c = &tls.Config{}
		}
		if c.ServerName == "" {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				_ = raw.Close()
				return nil, fmt.Errorf("TLS server name from %q: %w", addr, err)
			}
			c.ServerName = host
		}
		tc := tls.Client(raw, c)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		if !c.InsecureSkipVerify {
			if err := tc.VerifyHostname(c.ServerName); err != nil {
				_ = raw.Close()
				return nil, err
			}
		}
		return tc, nil
	}, nil
}

// dialVia dials addr with dial, through the proxy p when p is not nil.
func dialVia(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), p *url.URL, network, addr string) (net.Conn, error) {
	switch {
	case p == nil:
		return dial(ctx, network, addr)
	case p.Scheme == "http":
		return dialHTTPProxy(ctx, dial, p, addr)
	}
	d, err := proxy.FromURL(p, ctxDialer(dial))
	if err != nil {
		return nil, err
	}
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return d.Dial(network, addr)
}

// ctxDialer is a dial function as a golang.org/x/net/proxy forward dialer.
type ctxDialer func(context.Context, string, string) (net.Conn, error)

func (d ctxDialer) Dial(network, addr string) (net.Conn, error) {
	return d(context.Background(), network, addr)
}

func (d ctxDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d(ctx, network, addr)
}

// dialHTTPProxy opens a CONNECT tunnel to addr through the http proxy p,
// as gorilla's httpProxyDialer does (Basic proxy authorization from p's
// user info), bounded by ctx.
func dialHTTPProxy(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), p *url.URL, addr string) (net.Conn, error) {
	hostPort := p.Host
	if p.Port() == "" {
		hostPort = net.JoinHostPort(p.Hostname(), "80")
	}
	conn, err := dial(ctx, "tcp", hostPort)
	if err != nil {
		return nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(pastDeadline) })
	defer stop()
	header := make(http.Header)
	if user := p.User; user != nil {
		if pw, ok := user.Password(); ok {
			header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+pw)))
		}
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: header}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT to %s: %w", addr, err)
	}
	// The server does not speak before the TLS handshake, so the reader
	// holds no more than the reply.
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT to %s: %w", addr, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT to %s: %s", addr, resp.Status)
	}
	if !stop() {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT to %s: %w", addr, ctx.Err())
	}
	// ctx's deadline stays for the TLS handshake; gorilla sets the
	// connection's deadlines after it.
	return conn, nil
}
