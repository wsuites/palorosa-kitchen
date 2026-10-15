package browsersession

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestLiveHeadless is a manual smoke check of the invisible path against the
// real store. Run it with:
//
//	SGPROBE=1 go test -run TestLiveHeadless -v ./internal/browsersession
//
// It passes when the store page loads (verification done) or when the host
// asks for the interactive captcha (the visible window would take over). When
// the headless browser passes the challenge, the harvested cookies must also
// lift it for a plain HTTP client, which is what the app uses afterwards.
func TestLiveHeadless(t *testing.T) {
	if os.Getenv("SGPROBE") == "" {
		t.Skip("manual check: set SGPROBE=1 to run")
	}
	const (
		base = "https://palorosabreakfast.com"
		ua   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	)
	profile, err := os.MkdirTemp("", "sgprobe-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cookies, err := visit(ctx, Options{
		URL:        base + "/wp-login.php",
		ProfileDir: profile,
		UserAgent:  ua,
		Logf:       t.Logf,
	}, true)
	t.Logf("cookies = %d, err = %v", len(cookies), err)
	for _, cookie := range cookies {
		t.Logf("cookie %s (domain %s, path %s)", cookie.Name, cookie.Domain, cookie.Path)
	}
	if err != nil {
		if !errors.Is(err, errNeedsHuman) {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}

	// The harvested cookies must lift the challenge for the HTTP client.
	jar, _ := cookiejar.New(nil)
	root, _ := url.Parse(base + "/")
	jar.SetCookies(root, cookies)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/wp-login.php", nil)
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request with the browser cookies: %v", err)
	}
	defer resp.Body.Close()
	t.Logf("wp-login.php with the browser cookies -> %d (SG-Captcha %q)", resp.StatusCode, resp.Header.Get("SG-Captcha"))
	if resp.StatusCode == http.StatusAccepted || resp.Header.Get("SG-Captcha") != "" {
		t.Fatalf("the challenge is still up for the HTTP client")
	}
}
