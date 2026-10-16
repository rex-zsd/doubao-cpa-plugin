package doubao

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
)

// publicDownloadClient resolves at dial time and dials the checked address. A DNS
// preflight alone is insufficient because resolution can change between calls.
func publicDownloadClient(req *http.Request, proxyURL string) (*http.Client, func(), error) {
	if req.URL.Scheme != "https" || req.URL.Port() != "" && req.URL.Port() != "443" || req.URL.User != nil {
		return nil, nil, fmt.Errorf("public-network profile requires HTTPS port 443")
	}
	if proxyURL != "" && proxyURL != "direct" && proxyURL != "direct://" {
		return nil, nil, fmt.Errorf("public-network profile cannot validate proxy-side DNS; use inline media")
	}
	if proxyURL == "" {
		if p, err := http.ProxyFromEnvironment(req); err != nil || p != nil {
			return nil, nil, fmt.Errorf("public-network profile requires a direct connection; use inline media")
		}
	}
	transport := &http.Transport{DialContext: publicNetworkDial, ForceAttemptHTTP2: true}
	return &http.Client{Transport: transport}, transport.CloseIdleConnections, nil
}
func publicNetworkDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("public-network address is unavailable")
	}
	for _, ip := range addresses {
		if !publicNetworkIP(ip) {
			return nil, fmt.Errorf("public-network profile rejected a non-public address")
		}
	}
	var last error
	for _, ip := range addresses {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
func publicNetworkIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	ranges := []string{"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16"}
	for _, value := range ranges {
		if netip.MustParsePrefix(value).Contains(ip) {
			return false
		}
	}
	return ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip)
}

type publicMediaContextKey struct{}

func IsPublicMediaRequest(ctx context.Context) bool {
	value, _ := ctx.Value(publicMediaContextKey{}).(bool)
	return value
}
