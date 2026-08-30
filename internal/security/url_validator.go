// Package security owns every allow/deny decision about network destinations.
// No other component of the worker is permitted to make destination-policy
// decisions.
//
// Application-level validation implemented here is defense in depth. It does
// not, by itself, eliminate SSRF; production deployments must additionally
// enforce network-level egress controls.
package security

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/groovy-sky/chrome-control/internal/models"
)

// DNSTimeout bounds a single destination DNS resolution.
const DNSTimeout = 3 * time.Second

// AllowedPort is the only destination port permitted by the MVP policy.
const AllowedPort = "443"

// Error is a destination-policy violation carrying a machine-readable code
// from the models package.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// BrowserError converts the policy error into the API error type.
func (e *Error) BrowserError() *models.BrowserError {
	return models.NewError(e.Code, e.Message)
}

func newError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// CodeOf returns the machine-readable code for err, or fallback if err is not
// a destination-policy error.
func CodeOf(err error, fallback string) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return fallback
}

// Resolver resolves a hostname to IP addresses. It is injectable so that unit
// tests never depend on external DNS.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DefaultResolver resolves names using the system resolver.
type DefaultResolver struct {
	r *net.Resolver
}

// NewDefaultResolver returns a Resolver backed by net.DefaultResolver.
func NewDefaultResolver() *DefaultResolver {
	return &DefaultResolver{r: net.DefaultResolver}
}

// LookupNetIP implements Resolver.
func (d *DefaultResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r := d.r
	if r == nil {
		r = net.DefaultResolver
	}
	return r.LookupNetIP(ctx, network, host)
}

// blockedPrefixes enumerates every non-public range rejected by policy.
// IPv4-mapped IPv6 addresses are normalized with Unmap before classification,
// so only the canonical forms need to be listed.
var blockedPrefixes = []netip.Prefix{
	// Loopback.
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	// Private-use.
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	// Link-local (includes 169.254.169.254 cloud metadata).
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	// Unspecified.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::/128"),
	// Multicast.
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("ff00::/8"),
	// Carrier-grade NAT.
	netip.MustParsePrefix("100.64.0.0/10"),
	// Documentation, benchmarking and reserved.
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// AWS IPv6 metadata endpoint.
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// blockedHosts are names that must never be resolved or navigated to.
var blockedHosts = map[string]bool{
	"localhost":                true,
	"metadata.google.internal": true,
}

// isBlockedAddr reports whether addr is a non-public address.
// addr must already be Unmap()-ed.
func isBlockedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	// Belt and braces for anything the explicit prefixes may miss.
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	return false
}

// ValidateAddr validates a single resolved address against the destination
// policy. It is used both after DNS resolution and by CDP request
// interception.
func ValidateAddr(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return newError(models.CodeBlockedDestination, "destination address is not valid")
	}
	if isBlockedAddr(addr) {
		return newError(models.CodeBlockedDestination, "destination resolves to a blocked network")
	}
	return nil
}

// IsBlockedHostname reports whether host (already normalized) is a name that
// policy forbids regardless of how it resolves.
func IsBlockedHostname(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if blockedHosts[host] {
		return true
	}
	if strings.HasSuffix(host, ".localhost") {
		return true
	}
	return false
}

// NormalizeHost trims a trailing dot and converts an internationalized domain
// name to its ASCII form. IP literals are returned unchanged.
func NormalizeHost(host string) (string, error) {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", newError(models.CodeInvalidURL, "URL host is required")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		// IP literal: no IDN processing applies.
		return host, nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", newError(models.CodeInvalidURL, "URL host is not a valid domain name")
	}
	return strings.ToLower(ascii), nil
}

// ParseAndValidateURL performs every check that does not require DNS and
// returns the parsed URL together with the normalized ASCII hostname.
func ParseAndValidateURL(rawURL string) (*url.URL, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", newError(models.CodeInvalidURL, "URL could not be parsed")
	}
	if u.Scheme != "https" {
		return nil, "", newError(models.CodeInvalidURL, "only HTTPS URLs are allowed")
	}
	if u.User != nil {
		return nil, "", newError(models.CodeInvalidURL, "URLs with embedded credentials are not allowed")
	}
	if u.Hostname() == "" {
		return nil, "", newError(models.CodeInvalidURL, "URL host is required")
	}
	host, err := NormalizeHost(u.Hostname())
	if err != nil {
		return nil, "", err
	}
	if IsBlockedHostname(host) {
		return nil, "", newError(models.CodeBlockedDestination, "destination is not permitted")
	}
	if port := u.Port(); port != "" && port != AllowedPort {
		return nil, "", newError(models.CodeInvalidURL, "only port 443 is permitted for MVP")
	}
	return u, host, nil
}

// ResolveAndValidateHost resolves host and rejects it when resolution yields
// no address or when any resolved address is blocked.
func ResolveAndValidateHost(ctx context.Context, host string, resolver Resolver) error {
	if IsBlockedHostname(host) {
		return newError(models.CodeBlockedDestination, "destination is not permitted")
	}
	// An IP literal needs no DNS resolution.
	if addr, err := netip.ParseAddr(host); err == nil {
		return ValidateAddr(addr)
	}
	if resolver == nil {
		resolver = NewDefaultResolver()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, DNSTimeout)
	defer cancel()

	addrs, err := resolver.LookupNetIP(lookupCtx, "ip", host)
	if err != nil {
		return newError(models.CodeDNSFailure, "DNS resolution failed for destination host")
	}
	if len(addrs) == 0 {
		return newError(models.CodeDNSFailure, "DNS resolution returned no addresses")
	}
	for _, addr := range addrs {
		if err := ValidateAddr(addr); err != nil {
			return err
		}
	}
	return nil
}

// ValidateURLContext applies the complete destination policy to rawURL,
// including DNS resolution, honouring cancellation of ctx.
func ValidateURLContext(ctx context.Context, rawURL string, resolver Resolver) error {
	_, host, err := ParseAndValidateURL(rawURL)
	if err != nil {
		return err
	}
	return ResolveAndValidateHost(ctx, host, resolver)
}

// ValidateURL applies the complete destination policy to rawURL.
//
//  1. Parse URL.
//  2. Require the https scheme.
//  3. Reject embedded credentials.
//  4. Require a non-empty host.
//  5. Trim a trailing dot and convert IDN hosts to ASCII.
//  6. Reject blocked hostnames (localhost, .localhost, metadata.google.internal).
//  7. Require an empty port or port 443.
//  8. Resolve DNS with a 3s timeout.
//  9. Reject when resolution returned no addresses.
//  10. Reject when any resolved address is blocked.
func ValidateURL(rawURL string, resolver Resolver) error {
	return ValidateURLContext(context.Background(), rawURL, resolver)
}
