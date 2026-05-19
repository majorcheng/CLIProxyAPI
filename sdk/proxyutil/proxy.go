package proxyutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// Mode 描述 proxy-url 配置在运行时应采用的网络模式。
type Mode int

const (
	// ModeInherit 表示未显式配置代理，沿用调用方默认行为。
	ModeInherit Mode = iota
	// ModeDirect 表示显式绕过代理。
	ModeDirect
	// ModeProxy 表示使用具体代理 URL。
	ModeProxy
	// ModeInvalid 表示配置存在但格式或协议不受支持。
	ModeInvalid
)

// Setting 是 proxy-url 解析后的规范化配置。
type Setting struct {
	Raw  string
	Mode Mode
	URL  *url.URL
}

// Parse 将 proxy-url 归一化为 inherit、direct 或 proxy 三类模式。
func Parse(raw string) (Setting, error) {
	trimmed := strings.TrimSpace(raw)
	setting := Setting{Raw: trimmed}

	if trimmed == "" {
		setting.Mode = ModeInherit
		return setting, nil
	}

	if strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
		setting.Mode = ModeDirect
		return setting, nil
	}

	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("parse proxy URL failed")
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("proxy URL missing scheme/host")
	}

	switch parsedURL.Scheme {
	case "socks5", "socks5h", "http", "https":
		setting.Mode = ModeProxy
		setting.URL = parsedURL
		return setting, nil
	default:
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
	}
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport.Clone()
	}
	return &http.Transport{}
}

// NewDirectTransport 返回显式不读取环境代理的 HTTP transport。
func NewDirectTransport() *http.Transport {
	clone := cloneDefaultTransport()
	clone.Proxy = nil
	return clone
}

// BuildHTTPTransport 构造普通 HTTP 请求使用的 transport。
func BuildHTTPTransport(raw string) (*http.Transport, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return NewDirectTransport(), setting.Mode, nil
	case ModeProxy:
		if setting.URL.Scheme == "socks5" || setting.URL.Scheme == "socks5h" {
			var proxyAuth *proxy.Auth
			if setting.URL.User != nil {
				username := setting.URL.User.Username()
				password, _ := setting.URL.User.Password()
				proxyAuth = &proxy.Auth{User: username, Password: password}
			}
			dialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, proxy.Direct)
			if errSOCKS5 != nil {
				return nil, setting.Mode, fmt.Errorf("create SOCKS5 dialer failed: %w", errSOCKS5)
			}
			transport := cloneDefaultTransport()
			transport.Proxy = nil
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
			return transport, setting.Mode, nil
		}
		transport := cloneDefaultTransport()
		transport.Proxy = http.ProxyURL(setting.URL)
		return transport, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

// BuildDialer 构造连接层使用的拨号器；uTLS/HTTP2 直连路径会走这里。
func BuildDialer(raw string) (proxy.Dialer, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return proxy.Direct, setting.Mode, nil
	case ModeProxy:
		if setting.URL.Scheme == "http" || setting.URL.Scheme == "https" {
			return &httpConnectDialer{proxyURL: setting.URL, dialer: proxy.Direct}, setting.Mode, nil
		}
		dialer, errDialer := proxy.FromURL(setting.URL, proxy.Direct)
		if errDialer != nil {
			return nil, setting.Mode, fmt.Errorf("create proxy dialer failed: %w", errDialer)
		}
		return dialer, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

type httpConnectDialer struct {
	proxyURL *url.URL
	dialer   proxy.Dialer
}

// Dial 先连接 HTTP/HTTPS 代理，再通过 CONNECT 建立目标 TCP 隧道。
func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	conn, errDial := d.dialer.Dial(network, proxyDialAddr(d.proxyURL))
	if errDial != nil {
		return nil, fmt.Errorf("dial HTTP proxy failed: %w", errDial)
	}

	wrappedConn, errTLS := d.wrapTLSIfNeeded(conn)
	if errTLS != nil {
		return nil, errTLS
	}

	req := d.connectRequest(addr)
	if errWrite := req.Write(wrappedConn); errWrite != nil {
		return nil, closeProxyConnWithError(wrappedConn, "write CONNECT request failed", errWrite)
	}
	return readCONNECTResponse(wrappedConn, req)
}

// wrapTLSIfNeeded 在 HTTPS 代理场景先建立到代理的 TLS 连接。
func (d *httpConnectDialer) wrapTLSIfNeeded(conn net.Conn) (net.Conn, error) {
	if d.proxyURL.Scheme != "https" {
		return conn, nil
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: d.proxyURL.Hostname()})
	if errHandshake := tlsConn.Handshake(); errHandshake != nil {
		return nil, closeProxyConnWithError(conn, "HTTPS proxy TLS handshake failed", errHandshake)
	}
	return tlsConn, nil
}

// connectRequest 构造发送给 HTTP 代理的 CONNECT 请求。
func (d *httpConnectDialer) connectRequest(addr string) *http.Request {
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if d.proxyURL.User != nil {
		req.Header.Set("Proxy-Authorization", proxyAuthorization(d.proxyURL.User))
	}
	return req
}

// readCONNECTResponse 校验代理响应，并把已经预读的隧道字节放回连接。
func readCONNECTResponse(conn net.Conn, req *http.Request) (net.Conn, error) {
	reader := bufio.NewReader(conn)
	resp, errRead := http.ReadResponse(reader, req)
	if errRead != nil {
		return nil, closeProxyConnWithError(conn, "read CONNECT response failed", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, closeFailedCONNECT(conn, resp)
	}

	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

// closeFailedCONNECT 关闭非 200 CONNECT 响应及底层连接。
func closeFailedCONNECT(conn net.Conn, resp *http.Response) error {
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	return closeProxyConnWithError(conn, fmt.Sprintf("proxy CONNECT returned status %s", resp.Status), nil)
}

// closeProxyConnWithError 关闭代理连接，并把关闭错误拼进原始错误上下文。
func closeProxyConnWithError(conn net.Conn, message string, cause error) error {
	if errClose := conn.Close(); errClose != nil {
		if cause != nil {
			return fmt.Errorf("%s: %w; close failed: %v", message, cause, errClose)
		}
		return fmt.Errorf("%s; close failed: %v", message, errClose)
	}
	if cause != nil {
		return fmt.Errorf("%s: %w", message, cause)
	}
	return fmt.Errorf("%s", message)
}

func proxyDialAddr(proxyURL *url.URL) string {
	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

func proxyAuthorization(user *url.Userinfo) string {
	username := user.Username()
	password, _ := user.Password()
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + encoded
}

// Redact 返回可安全写入日志的代理 URL，移除凭据和路径类信息。
func Redact(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return "<invalid proxy URL>"
	}

	redacted := &url.URL{Scheme: parsedURL.Scheme, Host: parsedURL.Host}
	if parsedURL.User != nil {
		redacted.User = url.User("redacted")
	}
	return redacted.String()
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader.Buffered() > 0 {
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
}
