package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cloudAllowLoopbackForTest permits httptest.NewServer loopback targets during tests.
var cloudAllowLoopbackForTest bool

func validateProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("代理地址格式无效")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, errors.New("代理协议仅支持 http、https、socks5 或 socks5h")
	}
}

func normalizeWebDAVPrefix(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("白名单前缀不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("白名单前缀格式无效，需为不带账号密码的 http(s) 地址")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("白名单前缀协议仅支持 http 或 https")
	}
	u.Scheme = scheme
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

func matchesURLPrefix(target, prefix *url.URL) bool {
	if !strings.EqualFold(target.Scheme, prefix.Scheme) || !strings.EqualFold(target.Host, prefix.Host) {
		return false
	}
	p := strings.TrimSuffix(prefix.Path, "/")
	if p == "" {
		return true
	}
	return target.Path == p || strings.HasPrefix(target.Path, p+"/")
}

func isDisallowedCloudIP(ip net.IP, allowPrivate, allowLoopback bool) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if !allowLoopback && ip.IsLoopback() {
		return true
	}
	if !allowPrivate && ip.IsPrivate() {
		return true
	}
	return false
}

func validateWebDAVTargetURL(rawURL string, cfg cloudAdminSettings, isAdmin bool) (*url.URL, bool, error) {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil, false, errors.New("请填写有效的 WebDAV 地址（需含 http:// 或 https://）")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, false, errors.New("WebDAV 地址仅支持 http 或 https 协议")
	}
	if u.User != nil {
		return nil, false, errors.New("请不要把账号密码直接写在 WebDAV 地址中，请使用下方单独输入框")
	}
	u.Scheme = scheme
	u.RawQuery = ""
	u.Fragment = ""

	prefixTrusted := false
	for _, rawPrefix := range cfg.AllowedWebDAVPrefixes {
		p, err := normalizeWebDAVPrefix(rawPrefix)
		if err == nil && matchesURLPrefix(u, p) {
			prefixTrusted = true
			break
		}
	}
	if prefixTrusted || cloudAllowLoopbackForTest {
		return u, true, nil
	}

	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, false, errors.New("安全限制：不允许连接 localhost 环回地址（可由管理员加入信任前缀）")
	}
	allowPrivate := cfg.effectiveAllowPrivateWebDAV() || isAdmin
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
			return nil, false, errors.New("安全限制：不允许连接环回或链路本地地址")
		}
		if !allowPrivate && ip.IsPrivate() {
			return nil, false, errors.New("当前未开启局域网 WebDAV 访问，请联系管理员在「设置 → 云盘接入服务」开启或加入白名单")
		}
	}
	return u, false, nil
}

func isAllowedProviderHost(providerKind, host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if cloudAllowLoopbackForTest && (h == "127.0.0.1" || h == "localhost" || h == "::1") {
		return true
	}
	switch providerKind {
	case "gdrive":
		return h == "accounts.google.com" ||
			h == "oauth2.googleapis.com" ||
			h == "www.googleapis.com" ||
			strings.HasSuffix(h, ".googleapis.com") ||
			strings.HasSuffix(h, ".googleusercontent.com")
	case "baidu":
		return h == "openapi.baidu.com" ||
			h == "pan.baidu.com" ||
			h == "d.pcs.baidu.com" ||
			strings.HasSuffix(h, ".baidupcs.com") ||
			strings.HasSuffix(h, ".baidu.com")
	default:
		return true
	}
}

type validatedCloudTransport struct {
	base           http.RoundTripper
	providerKind   string
	cfg            cloudAdminSettings
	allowPrivateIP bool
}

func (t *validatedCloudTransport) validateReqURL(u *url.URL) error {
	if u == nil {
		return errors.New("无效的目标请求地址")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return errors.New("不支持的请求协议")
	}
	if t.providerKind == "webdav" {
		_, _, err := validateWebDAVTargetURL(u.String(), t.cfg, t.allowPrivateIP)
		return err
	}
	if !cloudAllowLoopbackForTest && scheme != "https" {
		return errors.New("外部云盘接口仅允许 HTTPS 连接")
	}
	if !isAllowedProviderHost(t.providerKind, u.Hostname()) {
		return fmt.Errorf("拒绝访问未授权域名：%s", u.Hostname())
	}
	return nil
}

func (t *validatedCloudTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.validateReqURL(req.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func newCloudHTTPClient(cfg cloudAdminSettings, providerKind string, connUseProxy bool, allowPrivateIP bool, allowTrustedLoopback bool, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	shouldProxy := providerKind == "gdrive" || cfg.effectiveProxyScope() == "all" || connUseProxy
	var proxyFunc func(*http.Request) (*url.URL, error)
	explicitProxy := cfg.effectiveProxyURL()
	if shouldProxy {
		if explicitProxy != "" {
			pu, err := validateProxyURL(explicitProxy)
			if err != nil {
				return nil, err
			}
			if pu != nil {
				proxyFunc = http.ProxyURL(pu)
			}
		} else {
			proxyFunc = http.ProxyFromEnvironment
		}
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 proxyFunc,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	}

	// On direct connections (no proxy), enforce DNS resolution IP checks to block DNS rebinding.
	if proxyFunc == nil && !cloudAllowLoopbackForTest {
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ipAddr := range ips {
				if isDisallowedCloudIP(ipAddr.IP, allowPrivateIP || allowTrustedLoopback, allowTrustedLoopback) {
					return nil, fmt.Errorf("安全限制：目标地址 %s 解析到受限 IP", host)
				}
			}
			var lastErr error
			for _, ipAddr := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("无法解析目标主机：%s", host)
		}
	}

	vt := &validatedCloudTransport{
		base:           tr,
		providerKind:   providerKind,
		cfg:            cfg,
		allowPrivateIP: allowPrivateIP,
	}

	return &http.Client{
		Transport: vt,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("云盘重定向次数过多")
			}
			if err := vt.validateReqURL(req.URL); err != nil {
				return err
			}
			prev := via[len(via)-1].URL
			if !strings.EqualFold(prev.Host, req.URL.Host) {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
			}
			if providerKind == "baidu" {
				req.Header.Set("User-Agent", "pan.baidu.com")
			}
			return nil
		},
	}, nil
}
