package transport

import (
	"net/http"
	"net/netip"
	"strings"
)

// cloudflareRanges are Cloudflare's published edge ranges, from
// https://www.cloudflare.com/ips/ (checked 2026-10-13; the same list as
// parallax-stats). They change rarely and Cloudflare announces changes in
// advance.
var cloudflareRanges = mustPrefixes(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// fromCloudflare returns the visitor address Cloudflare reports in
// CF-Connecting-IP, when the hop that reached this deployment is one of
// Cloudflare's edges. A request from anywhere else cannot set the client
// address this way.
func fromCloudflare(r *http.Request, hop netip.Addr) (netip.Addr, bool) {
	if !inPrefixes(hop, cloudflareRanges) {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP")))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}
