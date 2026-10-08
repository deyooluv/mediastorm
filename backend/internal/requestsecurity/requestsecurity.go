package requestsecurity

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrRestrictedOutboundAddress reports that an outbound host resolved only to
// local or private addresses that the active policy does not allow.
var ErrRestrictedOutboundAddress = errors.New("outbound host resolves to a restricted address")

var embeddedHTTPURL = regexp.MustCompile(`https?://[^\s\"'<>]+`)

const trustedProxiesEnv = "STRMR_TRUSTED_PROXIES"

var (
	trustedProxyOnce sync.Once
	trustedProxies   []*net.IPNet
)

// RemoteIP returns the IP address of the TCP peer. It deliberately ignores
// forwarded headers because those are controlled by the immediate client.
func RemoteIP(r *http.Request) net.IP {
	if r == nil {
		return nil
	}
	host := strings.TrimSpace(r.RemoteAddr)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	return net.ParseIP(host)
}

// IsLoopback reports whether the actual TCP peer is loopback.
func IsLoopback(r *http.Request) bool {
	ip := RemoteIP(r)
	return ip != nil && ip.IsLoopback()
}

// IsSecure reports TLS directly or HTTPS asserted by a trusted reverse proxy.
func IsSecure(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return isTrustedProxy(RemoteIP(r)) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// ClientIP returns a forwarded client address only when the immediate peer is
// trusted. Loopback proxies are trusted by default; additional proxy networks
// can be configured as comma-separated CIDRs in STRMR_TRUSTED_PROXIES.
func ClientIP(r *http.Request) string {
	peer := RemoteIP(r)
	if isTrustedProxy(peer) {
		if forwarded := firstForwardedIP(r.Header.Get("X-Forwarded-For")); forwarded != "" {
			return forwarded
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(realIP) != nil {
			return realIP
		}
	}
	if peer != nil {
		return peer.String()
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func firstForwardedIP(value string) string {
	for _, part := range strings.Split(value, ",") {
		candidate := strings.TrimSpace(part)
		if net.ParseIP(candidate) != nil {
			return candidate
		}
	}
	return ""
}

func isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	trustedProxyOnce.Do(func() {
		for _, raw := range strings.Split(os.Getenv(trustedProxiesEnv), ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if parsed := net.ParseIP(raw); parsed != nil {
				bits := 128
				if parsed.To4() != nil {
					bits = 32
				}
				trustedProxies = append(trustedProxies, &net.IPNet{IP: parsed, Mask: net.CIDRMask(bits, bits)})
				continue
			}
			if _, network, err := net.ParseCIDR(raw); err == nil {
				trustedProxies = append(trustedProxies, network)
			}
		}
	})
	for _, network := range trustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// RestrictedHostPolicy permits private-network destinations for explicitly
// configured host and port pairs. Public destinations should return false.
type RestrictedHostPolicy func(hostname, port string) bool

// URLForLog returns only the scheme and authority of a URL. Paths, user info,
// query strings, and fragments often carry provider tokens or signed media
// credentials and must not be written to logs.
func URLForLog(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[redacted-url]"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// TextForLog removes sensitive URL components from URLs embedded in error
// messages. Standard library and provider errors often include the complete
// request URL, including credentials or signed query parameters.
func TextForLog(value string) string {
	return embeddedHTTPURL.ReplaceAllStringFunc(value, URLForLog)
}

// ValidateOutboundURL verifies an HTTP(S) URL and resolves its host, rejecting
// local, private, link-local, multicast, and unspecified destinations unless
// the host is explicitly allowed by policy.
func ValidateOutboundURL(ctx context.Context, rawURL string, allowRestricted RestrictedHostPolicy) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid outbound URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported outbound URL scheme")
	}
	_, err = resolveAllowedIPs(ctx, parsed.Hostname(), effectiveURLPort(parsed), allowRestricted)
	return err
}

// NewSafeHTTPClient returns a direct HTTP client whose dialer pins DNS results
// after applying the outbound-address policy. Redirects are revalidated and
// sensitive headers retain the standard library's cross-host stripping rules.
func NewSafeHTTPClient(timeout time.Duration, maxRedirects int, allowRestricted RestrictedHostPolicy) *http.Client {
	return NewSafeHTTPClientWithPolicyProvider(timeout, maxRedirects, func() RestrictedHostPolicy {
		return allowRestricted
	})
}

// NewSafeHTTPClientWithPolicyProvider is equivalent to NewSafeHTTPClient, but
// resolves the restricted-host policy for every new connection and redirect.
// This allows long-lived clients to reuse safe public connections while still
// observing runtime configuration changes for explicitly allowed private
// origins.
func NewSafeHTTPClientWithPolicyProvider(timeout time.Duration, maxRedirects int, policyProvider func() RestrictedHostPolicy) *http.Client {
	currentPolicy := func() RestrictedHostPolicy {
		if policyProvider == nil {
			return nil
		}
		return policyProvider()
	}
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Supports deterministic tests that replace the default round tripper.
		// Production uses *http.Transport and always takes the hardened path.
		return &http.Client{
			Timeout:   timeout,
			Transport: http.DefaultTransport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("too many redirects")
				}
				return ValidateOutboundURL(req.Context(), req.URL.String(), currentPolicy())
			},
		}
	}
	transport := baseTransport.Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid outbound address: %w", err)
		}
		ips, err := resolveAllowedIPs(ctx, host, port, currentPolicy())
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects")
			}
			return ValidateOutboundURL(req.Context(), req.URL.String(), currentPolicy())
		},
	}
}

func resolveAllowedIPs(ctx context.Context, hostname, port string, allowRestricted RestrictedHostPolicy) ([]net.IP, error) {
	hostname = strings.Trim(strings.TrimSpace(hostname), "[]")
	allow := allowRestricted != nil && allowRestricted(hostname, port)
	var ips []net.IP
	if parsed := net.ParseIP(hostname); parsed != nil {
		ips = []net.IP{parsed}
	} else {
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, hostname)
		if err != nil {
			return nil, fmt.Errorf("resolve outbound host: %w", err)
		}
		for _, addr := range resolved {
			ips = append(ips, addr.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("outbound host has no addresses")
	}
	allowed := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if allow || !isRestrictedIP(ip) {
			allowed = append(allowed, ip)
		}
	}
	if len(allowed) == 0 {
		return nil, ErrRestrictedOutboundAddress
	}
	return allowed, nil
}

func effectiveURLPort(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	if port := parsed.Port(); port != "" {
		return port
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func isRestrictedIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || !ip.IsGlobalUnicast()
}
