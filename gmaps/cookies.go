package gmaps

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gosom/scrapemate"
	"github.com/playwright-community/playwright-go"
)

// Signed-out traffic can get Google's "limited view" of Maps (no photo
// gallery, ~10 reviews); a signed-in session gets the full place page.
var (
	googleCookiesMu sync.RWMutex
	googleCookies   []playwright.OptionalCookie
)

// SetGoogleCookies installs the cookies every browser page gets before opening Maps.
func SetGoogleCookies(cookies []playwright.OptionalCookie) {
	googleCookiesMu.Lock()
	defer googleCookiesMu.Unlock()

	googleCookies = cookies
}

func getGoogleCookies() []playwright.OptionalCookie {
	googleCookiesMu.RLock()
	defer googleCookiesMu.RUnlock()

	return googleCookies
}

// injectGoogleCookies adds the signed-in cookies to the page's browser context.
// Context-level, so they also cover pages the photo/review extractors open.
func injectGoogleCookies(page scrapemate.BrowserPage) error {
	cookies := getGoogleCookies()
	if len(cookies) == 0 {
		return nil
	}

	pwPage, ok := page.Unwrap().(playwright.Page)
	if !ok {
		return fmt.Errorf("unexpected browser page type: %T", page.Unwrap())
	}

	return pwPage.Context().AddCookies(cookies)
}

// MissingAuthCookies lists Google's sign-in cookies absent from an export;
// without them Maps still treats the session as signed out.
func MissingAuthCookies(cookies []playwright.OptionalCookie) []string {
	have := make(map[string]bool, len(cookies))
	for _, c := range cookies {
		have[c.Name] = true
	}

	var missing []string

	for _, name := range []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSID", "__Secure-3PSID"} {
		if !have[name] {
			missing = append(missing, name)
		}
	}

	return missing
}

// LoadGoogleCookies reads a cookie export: a Cookie-Editor/EditThisCookie JSON
// array, a Playwright storageState file, or a Netscape cookies.txt.
func LoadGoogleCookies(path string) ([]playwright.OptionalCookie, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	trimmed := bytes.TrimSpace(data)

	var cookies []playwright.OptionalCookie

	switch {
	case bytes.HasPrefix(trimmed, []byte("[")):
		var raw []exportedCookie
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return nil, fmt.Errorf("parse cookie JSON array: %w", err)
		}

		cookies = convertExported(raw)
	case bytes.HasPrefix(trimmed, []byte("{")):
		var state struct {
			Cookies []exportedCookie `json:"cookies"`
		}
		if err := json.Unmarshal(trimmed, &state); err != nil {
			return nil, fmt.Errorf("parse storage state: %w", err)
		}

		cookies = convertExported(state.Cookies)
	default:
		cookies, err = parseNetscapeCookies(trimmed)
		if err != nil {
			return nil, err
		}
	}

	if len(cookies) == 0 {
		return nil, fmt.Errorf("no google.com cookies found in %s", path)
	}

	return cookies, nil
}

type exportedCookie struct {
	Name           string   `json:"name"`
	Value          string   `json:"value"`
	Domain         string   `json:"domain"`
	Path           string   `json:"path"`
	Secure         bool     `json:"secure"`
	HTTPOnly       bool     `json:"httpOnly"`
	SameSite       string   `json:"sameSite"`
	Expires        *float64 `json:"expires"`
	ExpirationDate *float64 `json:"expirationDate"`
}

func convertExported(raw []exportedCookie) []playwright.OptionalCookie {
	out := make([]playwright.OptionalCookie, 0, len(raw))

	for _, c := range raw {
		expires := c.Expires
		if expires == nil {
			expires = c.ExpirationDate
		}

		if cookie, ok := newGoogleCookie(c.Name, c.Value, c.Domain, c.Path, c.Secure, c.HTTPOnly, c.SameSite, expires); ok {
			out = append(out, cookie)
		}
	}

	return out
}

func parseNetscapeCookies(data []byte) ([]playwright.OptionalCookie, error) {
	var out []playwright.OptionalCookie

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		httpOnly := strings.HasPrefix(line, "#HttpOnly_")
		line = strings.TrimPrefix(line, "#HttpOnly_")

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}

		var expires *float64
		if ts, err := strconv.ParseFloat(fields[4], 64); err == nil && ts > 0 {
			expires = &ts
		}

		secure := strings.EqualFold(fields[3], "TRUE")
		if cookie, ok := newGoogleCookie(fields[5], fields[6], fields[0], fields[2], secure, httpOnly, "", expires); ok {
			out = append(out, cookie)
		}
	}

	return out, scanner.Err()
}

// newGoogleCookie keeps only google.com cookies; other sites' cookies in a full
// browser export are never sent to Maps anyway.
func newGoogleCookie(name, value, domain, path string, secure, httpOnly bool, sameSite string, expires *float64) (playwright.OptionalCookie, bool) {
	if name == "" || !strings.HasSuffix(strings.TrimPrefix(domain, "."), "google.com") {
		return playwright.OptionalCookie{}, false
	}

	if path == "" {
		path = "/"
	}

	cookie := playwright.OptionalCookie{
		Name:     name,
		Value:    value,
		Domain:   playwright.String(domain),
		Path:     playwright.String(path),
		Secure:   playwright.Bool(secure),
		HttpOnly: playwright.Bool(httpOnly),
	}

	// Session cookies (no expiry) must omit the field rather than send 0.
	if expires != nil && *expires > 0 {
		cookie.Expires = expires
	}

	switch strings.ToLower(sameSite) {
	case "strict":
		cookie.SameSite = playwright.SameSiteAttributeStrict
	case "lax":
		cookie.SameSite = playwright.SameSiteAttributeLax
	case "none", "no_restriction":
		cookie.SameSite = playwright.SameSiteAttributeNone
	}

	return cookie, true
}
