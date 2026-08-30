package unit

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/groovy-sky/chrome-control/internal/artifacts"
	"github.com/groovy-sky/chrome-control/internal/browser"
	"github.com/groovy-sky/chrome-control/internal/models"
	"github.com/groovy-sky/chrome-control/internal/security"
)

// mockResolver returns canned answers so unit tests never touch real DNS.
type mockResolver struct {
	addrs     map[string][]netip.Addr
	err       error
	lastHost  string
	lookupCnt int
}

func (m *mockResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	m.lastHost = host
	m.lookupCnt++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.err != nil {
		return nil, m.err
	}
	if addrs, ok := m.addrs[host]; ok {
		return addrs, nil
	}
	return nil, errors.New("no such host")
}

func resolverFor(host string, ips ...string) *mockResolver {
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, netip.MustParseAddr(ip))
	}
	return &mockResolver{addrs: map[string][]netip.Addr{host: addrs}}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var serr *security.Error
	if !errors.As(err, &serr) {
		t.Fatalf("error %v is not a *security.Error", err)
	}
	return serr.Code
}

func TestValidateURLRejectsNonHTTPSSchemes(t *testing.T) {
	r := resolverFor("example.com", "93.184.216.34")
	for _, raw := range []string{
		"http://example.com/",
		"ftp://example.com/",
		"file:///etc/passwd",
		"gopher://example.com/",
		"javascript:alert(1)",
		"data:text/html,hello",
		"//example.com/",
		"example.com",
	} {
		if err := security.ValidateURL(raw, r); err == nil {
			t.Errorf("%q: expected rejection, got nil", raw)
		}
	}
}

func TestValidateURLRejectsEmbeddedCredentials(t *testing.T) {
	r := resolverFor("example.com", "93.184.216.34")
	const at = "@"
	for _, raw := range []string{
		"https://user" + at + "example.com/",
		"https://user:secret" + at + "example.com/",
		"https://" + at + "example.com/",
	} {
		err := security.ValidateURL(raw, r)
		if got := codeOf(t, err); got != models.CodeInvalidURL {
			t.Errorf("%q: got code %q, want %q", raw, got, models.CodeInvalidURL)
		}
	}
}

func TestValidateURLRejectsNon443Ports(t *testing.T) {
	r := resolverFor("example.com", "93.184.216.34")
	for _, raw := range []string{
		"https://example.com:8443/",
		"https://example.com:80/",
		"https://example.com:22/",
	} {
		err := security.ValidateURL(raw, r)
		if got := codeOf(t, err); got != models.CodeInvalidURL {
			t.Errorf("%q: got code %q, want %q", raw, got, models.CodeInvalidURL)
		}
	}
	if err := security.ValidateURL("https://example.com:443/", r); err != nil {
		t.Errorf("port 443 must be accepted, got %v", err)
	}
}

func TestValidateURLRejectsBlockedHostnames(t *testing.T) {
	r := &mockResolver{addrs: map[string][]netip.Addr{}}
	for _, raw := range []string{
		"https://localhost/",
		"https://localhost./",
		"https://LOCALHOST/",
		"https://api.localhost/",
		"https://metadata.google.internal/",
		"https://metadata.google.internal./",
	} {
		err := security.ValidateURL(raw, r)
		if got := codeOf(t, err); got != models.CodeBlockedDestination {
			t.Errorf("%q: got code %q, want %q", raw, got, models.CodeBlockedDestination)
		}
	}
	if r.lookupCnt != 0 {
		t.Errorf("blocked hostnames must not be resolved, got %d lookups", r.lookupCnt)
	}
}

func TestValidateURLRejectsBlockedRanges(t *testing.T) {
	cases := []struct {
		name string
		ip   string
	}{
		{"loopback v4", "127.0.0.1"},
		{"loopback v4 high", "127.255.255.254"},
		{"loopback v6", "::1"},
		{"private 10/8", "10.0.0.1"},
		{"private 172.16/12", "172.16.5.4"},
		{"private 172.31/12", "172.31.255.254"},
		{"private 192.168/16", "192.168.1.1"},
		{"unique local v6", "fc00::1"},
		{"unique local v6 fd", "fd12:3456::1"},
		{"link-local v4", "169.254.1.1"},
		{"cloud metadata v4", "169.254.169.254"},
		{"link-local v6", "fe80::1"},
		{"aws metadata v6", "fd00:ec2::254"},
		{"unspecified v4", "0.0.0.0"},
		{"unspecified v4 range", "0.1.2.3"},
		{"unspecified v6", "::"},
		{"multicast v4", "224.0.0.1"},
		{"multicast v4 high", "239.255.255.255"},
		{"multicast v6", "ff02::1"},
		{"cgnat", "100.64.0.1"},
		{"cgnat high", "100.127.255.255"},
		{"documentation test-net-1", "192.0.2.1"},
		{"documentation test-net-2", "198.51.100.1"},
		{"documentation test-net-3", "203.0.113.1"},
		{"benchmarking", "198.18.0.1"},
		{"reserved 240/4", "240.0.0.1"},
		{"broadcast", "255.255.255.255"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := resolverFor("evil.example", tc.ip)
			err := security.ValidateURL("https://evil.example/", r)
			if got := codeOf(t, err); got != models.CodeBlockedDestination {
				t.Fatalf("ip %s: got code %q, want %q", tc.ip, got, models.CodeBlockedDestination)
			}
			// The same address must be rejected as a literal host too.
			literal := "https://" + tc.ip + "/"
			if strings.Contains(tc.ip, ":") {
				literal = "https://[" + tc.ip + "]/"
			}
			if err := security.ValidateURL(literal, r); err == nil {
				t.Fatalf("literal %s: expected rejection, got nil", literal)
			}
		})
	}
}

func TestValidateAddrRejectsIPv4MappedIPv6Forms(t *testing.T) {
	for _, mapped := range []string{
		"::ffff:127.0.0.1",
		"::ffff:10.0.0.1",
		"::ffff:172.16.0.1",
		"::ffff:192.168.0.1",
		"::ffff:169.254.169.254",
		"::ffff:100.64.0.1",
		"::ffff:0.0.0.0",
	} {
		addr := netip.MustParseAddr(mapped)
		if err := security.ValidateAddr(addr); err == nil {
			t.Errorf("%s: expected rejection of IPv4-mapped address, got nil", mapped)
		}
	}

	// The same forms must be rejected when they come back from DNS.
	r := resolverFor("rebind.example", "::ffff:10.0.0.1")
	err := security.ValidateURL("https://rebind.example/", r)
	if got := codeOf(t, err); got != models.CodeBlockedDestination {
		t.Errorf("got code %q, want %q", got, models.CodeBlockedDestination)
	}
}

func TestValidateURLRejectsWhenAnyResolvedAddressIsBlocked(t *testing.T) {
	r := resolverFor("mixed.example", "93.184.216.34", "10.1.2.3")
	err := security.ValidateURL("https://mixed.example/", r)
	if got := codeOf(t, err); got != models.CodeBlockedDestination {
		t.Errorf("got code %q, want %q", got, models.CodeBlockedDestination)
	}
}

func TestValidateURLRejectsEmptyDNSAnswer(t *testing.T) {
	r := &mockResolver{addrs: map[string][]netip.Addr{"empty.example": {}}}
	err := security.ValidateURL("https://empty.example/", r)
	if got := codeOf(t, err); got != models.CodeDNSFailure {
		t.Errorf("got code %q, want %q", got, models.CodeDNSFailure)
	}
}

func TestValidateURLRejectsDNSFailure(t *testing.T) {
	r := &mockResolver{err: errors.New("server misbehaving")}
	err := security.ValidateURL("https://broken.example/", r)
	if got := codeOf(t, err); got != models.CodeDNSFailure {
		t.Errorf("got code %q, want %q", got, models.CodeDNSFailure)
	}
}

func TestValidateURLAcceptsPublicHTTPSURL(t *testing.T) {
	r := resolverFor("example.com", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946")
	for _, raw := range []string{
		"https://example.com",
		"https://example.com/",
		"https://example.com/path?q=1#frag",
		"https://example.com.:443/path",
	} {
		if err := security.ValidateURL(raw, r); err != nil {
			t.Errorf("%q: expected acceptance, got %v", raw, err)
		}
	}
}

func TestValidateURLNormalizesInternationalizedHostnames(t *testing.T) {
	// "bücher.example" in ASCII form.
	const ascii = "xn--bcher-kva.example"
	r := resolverFor(ascii, "93.184.216.34")
	if err := security.ValidateURL("https://bücher.example/", r); err != nil {
		t.Fatalf("expected acceptance, got %v", err)
	}
	if r.lastHost != ascii {
		t.Errorf("resolver received host %q, want %q", r.lastHost, ascii)
	}
}

func TestValidateURLNormalizesUppercaseHostnames(t *testing.T) {
	r := resolverFor("example.com", "93.184.216.34")
	if err := security.ValidateURL("https://EXAMPLE.COM/", r); err != nil {
		t.Fatalf("expected acceptance, got %v", err)
	}
	if r.lastHost != "example.com" {
		t.Errorf("resolver received host %q, want %q", r.lastHost, "example.com")
	}
}

func TestValidateURLRejectsMissingHost(t *testing.T) {
	r := resolverFor("example.com", "93.184.216.34")
	if err := security.ValidateURL("https:///path", r); err == nil {
		t.Error("expected rejection of URL without host")
	}
}

func TestValidateAddrAcceptsPublicAddresses(t *testing.T) {
	for _, ip := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"} {
		if err := security.ValidateAddr(netip.MustParseAddr(ip)); err != nil {
			t.Errorf("%s: expected acceptance, got %v", ip, err)
		}
	}
}

func TestValidateURLHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := resolverFor("example.com", "93.184.216.34")
	// A cancelled context must not allow the destination through.
	err := security.ValidateURLContext(ctx, "https://example.com/", r)
	if got := codeOf(t, err); got != models.CodeDNSFailure {
		t.Errorf("got code %q, want %q", got, models.CodeDNSFailure)
	}
}

func TestTruncateToCodePoints(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"zero limit", "hello", 0, ""},
		{"negative limit", "hello", -3, ""},
		{"under limit", "hello", 10, "hello"},
		{"exact limit", "hello", 5, "hello"},
		{"ascii truncated", "hello", 2, "he"},
		{"multibyte kept whole", "héllo", 2, "hé"},
		{"cjk", "日本語テキスト", 3, "日本語"},
		{"emoji", "a😀b", 2, "a😀"},
		{"combining sequence", "e\u0301x", 2, "e\u0301"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := browser.TruncateToCodePoints(tc.in, tc.n)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncation produced invalid UTF-8: %q", got)
			}
		})
	}
}

func TestTruncateToCodePointsNeverSplitsUTF8(t *testing.T) {
	s := "aä中𝄞b😀ü"
	for n := 0; n <= utf8.RuneCountInString(s)+3; n++ {
		got := browser.TruncateToCodePoints(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("n=%d produced invalid UTF-8: %q", n, got)
		}
		want := min(n, utf8.RuneCountInString(s))
		if got := utf8.RuneCountInString(got); got != want {
			t.Fatalf("n=%d: got %d code points, want %d", n, got, want)
		}
	}
}

func TestClampMaxTextChars(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, browser.DefaultMaxTextChars},
		{-1, browser.DefaultMaxTextChars},
		{-100000, browser.DefaultMaxTextChars},
		{1, 1},
		{500, 500},
		{browser.ServerMaxTextChars, browser.ServerMaxTextChars},
		{browser.ServerMaxTextChars + 1, browser.ServerMaxTextChars},
		{1 << 30, browser.ServerMaxTextChars},
	}
	for _, tc := range cases {
		if got := browser.ClampMaxTextChars(tc.in); got != tc.want {
			t.Errorf("ClampMaxTextChars(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDedupeLinksPreservesDocumentOrder(t *testing.T) {
	in := []models.VisibleLink{
		{Text: "first", URL: "https://a.example/1"},
		{Text: "second", URL: "https://b.example/2"},
		{Text: "duplicate of first", URL: "https://a.example/1"},
		{Text: "third", URL: "https://c.example/3"},
		{Text: "empty url", URL: ""},
		{Text: "duplicate of second", URL: "https://b.example/2"},
	}
	got := browser.DedupeLinks(in, browser.MaxLinks)
	want := []string{"https://a.example/1", "https://b.example/2", "https://c.example/3"}
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].URL != w {
			t.Errorf("link %d = %q, want %q", i, got[i].URL, w)
		}
	}
	if got[0].Text != "first" {
		t.Errorf("first occurrence text should win, got %q", got[0].Text)
	}
}

func TestDedupeLinksAppliesLimit(t *testing.T) {
	in := make([]models.VisibleLink, 0, 150)
	for i := 0; i < 150; i++ {
		in = append(in, models.VisibleLink{URL: "https://example.com/" + strings.Repeat("x", i)})
	}
	got := browser.DedupeLinks(in, browser.MaxLinks)
	if len(got) != browser.MaxLinks {
		t.Fatalf("got %d links, want %d", len(got), browser.MaxLinks)
	}
	if browser.DedupeLinks(in, 0) != nil {
		t.Error("a zero limit must yield no links")
	}
}

func TestRedirectTrackerRejectsAtEleventhRedirect(t *testing.T) {
	tr := browser.NewRedirectTracker(browser.MaxRedirects)
	for i := 1; i <= browser.MaxRedirects; i++ {
		if err := tr.Record(); err != nil {
			t.Fatalf("redirect %d must be allowed, got %v", i, err)
		}
	}
	err := tr.Record()
	if err == nil {
		t.Fatal("the 11th redirect must be rejected")
	}
	if got := codeOf(t, err); got != models.CodeRedirectLimitExceeded {
		t.Fatalf("got code %q, want %q", got, models.CodeRedirectLimitExceeded)
	}
	if tr.Count() != browser.MaxRedirects+1 {
		t.Fatalf("got count %d, want %d", tr.Count(), browser.MaxRedirects+1)
	}
}

func TestValidateRequestConstraints(t *testing.T) {
	cases := []struct {
		name string
		req  models.BrowserRequest
		want string
	}{
		{"valid", models.BrowserRequest{TaskID: "task_1", URL: "https://example.com"}, ""},
		{"missing task id", models.BrowserRequest{URL: "https://example.com"}, models.CodeInvalidRequest},
		{"task id too long", models.BrowserRequest{TaskID: strings.Repeat("a", 129), URL: "https://example.com"}, models.CodeInvalidRequest},
		{"task id at limit", models.BrowserRequest{TaskID: strings.Repeat("a", 128), URL: "https://example.com"}, ""},
		{"missing url", models.BrowserRequest{TaskID: "task_1"}, models.CodeInvalidRequest},
		{"negative max text chars", models.BrowserRequest{TaskID: "t", URL: "https://example.com", MaxTextChars: -1}, models.CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			berr := browser.ValidateRequest(&tc.req)
			if tc.want == "" {
				if berr != nil {
					t.Fatalf("expected no error, got %v", berr)
				}
				return
			}
			if berr == nil || berr.Code != tc.want {
				t.Fatalf("got %v, want code %q", berr, tc.want)
			}
		})
	}
}

func TestHTTPStatusForCode(t *testing.T) {
	cases := map[string]int{
		models.CodeInvalidRequest:        400,
		models.CodeInvalidURL:            400,
		models.CodeBlockedDestination:    400,
		models.CodeDNSFailure:            400,
		models.CodeRedirectBlocked:       400,
		models.CodeRedirectLimitExceeded: 400,
		models.CodeBrowserStartFailed:    500,
		models.CodeNavigationTimeout:     504,
		models.CodeExtractionFailed:      500,
		models.CodeScreenshotFailed:      500,
		models.CodeTaskTimeout:           504,
		models.CodeOverloaded:            503,
	}
	for code, want := range cases {
		if got := models.HTTPStatusForCode(code); got != want {
			t.Errorf("%s: got %d, want %d", code, got, want)
		}
	}
}

func TestArtifactStoreUsesOpaqueIDs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := artifacts.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("artifact root mode = %v, want 0700", perm)
	}

	id, err := store.Save([]byte("png-bytes"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(id) != 32 {
		t.Errorf("artifact id %q should be 32 hex characters", id)
	}
	if strings.ContainsAny(id, "/\\.") {
		t.Errorf("artifact id %q must be opaque", id)
	}

	fileInfo, err := os.Stat(filepath.Join(root, id))
	if err != nil {
		t.Fatalf("stat artifact: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("artifact mode = %v, want 0600", perm)
	}

	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
		t.Error("artifact should have been removed")
	}
	if err := store.Delete(id); err != nil {
		t.Errorf("deleting a missing artifact must not fail: %v", err)
	}
}

func TestArtifactStoreRejectsPathTraversal(t *testing.T) {
	store, err := artifacts.New(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, id := range []string{
		"../../etc/passwd",
		"..",
		"",
		"/etc/passwd",
		"a/b",
		strings.Repeat("g", 32),
		strings.Repeat("a", 31),
		strings.Repeat("A", 32),
	} {
		if err := store.Delete(id); err == nil {
			t.Errorf("id %q: expected rejection", id)
		}
	}
}

func TestArtifactStoreRejectsOversizedArtifacts(t *testing.T) {
	store, err := artifacts.New(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Save(make([]byte, artifacts.MaxFileSizeBytes+1)); err == nil {
		t.Error("expected rejection of oversized artifact")
	}
	if _, err := store.Save(make([]byte, 1024)); err != nil {
		t.Errorf("expected small artifact to be stored, got %v", err)
	}
}
