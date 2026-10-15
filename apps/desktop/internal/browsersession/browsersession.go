// Package browsersession passes the store's hosting bot challenge when the
// plain HTTP client cannot: the challenge is JavaScript (SiteGround
// SG-Captcha), so only a real browser can answer it — automatically through
// proof of work, or by the operator when the host escalates to the
// interactive captcha. The cookies the browser is granted are handed back, so
// the HTTP client can keep working as if it had always been a browser.
package browsersession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	// defaultHumanTimeout is how long the visible window waits for the
	// operator to complete an interactive captcha.
	defaultHumanTimeout = 5 * time.Minute
	// headlessBudget is how long the invisible attempt waits for the
	// automatic challenge before asking for the visible window.
	headlessBudget = 25 * time.Second
	// pollInterval is how often the page is inspected.
	pollInterval = 1250 * time.Millisecond
	// portBudget is how long the browser gets to expose its debugger.
	portBudget = 30 * time.Second
)

// errNeedsHuman asks Ensure to retry with a visible window.
var errNeedsHuman = errors.New("el captcha necesita completarse en la ventana del navegador")

// Options are the settings for one verification.
type Options struct {
	// URL is the store page to open, e.g. https://tienda/wp-login.php.
	URL string
	// ProfileDir persists the browser profile between runs, so a passed
	// challenge is reused until it expires.
	ProfileDir string
	// UserAgent, when set, is the agent the browser presents, so the cookie
	// whitelist the challenge grants matches the HTTP client that reuses it.
	UserAgent string
	// HumanTimeout bounds the visible-window wait; zero is five minutes.
	HumanTimeout time.Duration
	// Logf, when set, receives progress notes for the operator.
	Logf func(format string, args ...any)
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// ForStore builds the wpexport browser-session hook for one store: it opens
// base/wp-login.php through Edge and keeps the browser profile under dataDir,
// so a passed challenge lasts until it expires. userAgent must be the one the
// HTTP client sends, so both sides match the whitelist.
func ForStore(dataDir, base, userAgent string) func(ctx context.Context) ([]*http.Cookie, error) {
	profile := filepath.Join(dataDir, "web-session")
	target := strings.TrimRight(base, "/") + "/wp-login.php"
	return func(ctx context.Context) ([]*http.Cookie, error) {
		return Ensure(ctx, Options{URL: target, ProfileDir: profile, UserAgent: userAgent})
	}
}

// Ensure visits the store through Edge to pass the challenge and returns the
// granted cookies. It first tries the invisible browser; when the host asks
// for the interactive captcha (or the invisible attempt stalls), it reopens
// the same profile in a visible window and waits for the operator.
func Ensure(ctx context.Context, opts Options) ([]*http.Cookie, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return nil, errors.New("falta la URL de la tienda")
	}
	if opts.ProfileDir == "" {
		return nil, errors.New("falta la carpeta del perfil del navegador")
	}
	if opts.HumanTimeout <= 0 {
		opts.HumanTimeout = defaultHumanTimeout
	}
	opts.logf("verificando la tienda en un navegador sin ventana…")
	cookies, err := visit(ctx, opts, true)
	if err == nil {
		return cookies, nil
	}
	if !errors.Is(err, errNeedsHuman) {
		return nil, err
	}
	opts.logf("SiteGround pidió un captcha; abriendo la ventana del navegador")
	return visit(ctx, opts, false)
}

// visit launches Edge, opens opts.URL, waits for the challenge to clear and
// returns the cookies the browser was granted. headless runs the invisible
// browser, where only the automatic challenge can be passed.
func visit(ctx context.Context, opts Options, headless bool) ([]*http.Cookie, error) {
	target, err := url.Parse(opts.URL)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("URL de tienda inválida %q", opts.URL)
	}
	edge, err := findEdge()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("no se pudo preparar el perfil del navegador: %w", err)
	}
	portFile := filepath.Join(opts.ProfileDir, "DevToolsActivePort")
	// A stale file from an earlier run would be read as this browser's.
	_ = os.Remove(portFile)

	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + opts.ProfileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
		"about:blank",
	}
	if headless {
		args = append([]string{
			"--headless=new", "--disable-gpu",
			// The challenge solves in JavaScript; without these the page is
			// throttled like a hidden tab and the proof of work can stall.
			"--disable-background-timer-throttling",
			"--disable-backgrounding-occluded-windows",
			"--disable-renderer-backgrounding",
		}, args...)
	} else {
		args = append([]string{"--window-size=1100,780"}, args...)
	}
	cmd := exec.Command(edge, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("no se pudo abrir el navegador: %w", err)
	}
	defer killTree(cmd.Process.Pid)

	wsURL, err := waitDebugger(ctx, portFile)
	if err != nil {
		return nil, err
	}
	conn, err := dialCDP(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	session, err := conn.openPage(ctx)
	if err != nil {
		return nil, err
	}
	if opts.UserAgent != "" {
		if err := conn.call(ctx, session, "Emulation.setUserAgentOverride", map[string]any{
			"userAgent":      opts.UserAgent,
			"acceptLanguage": "es-ES,es;q=0.9",
		}, nil); err != nil {
			return nil, err
		}
	}
	if err := conn.call(ctx, session, "Page.navigate", map[string]any{"url": opts.URL}, nil); err != nil {
		return nil, fmt.Errorf("no se pudo abrir la tienda en el navegador: %w", err)
	}

	origin := target.Scheme + "://" + target.Host
	budget := headlessBudget
	if !headless {
		budget = opts.HumanTimeout
	}
	deadline := time.Now().Add(budget)
	toldHuman := false
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
		state, err := readState(ctx, conn, session)
		if err != nil {
			if errors.Is(err, errBrowserGone) {
				return nil, errors.New("la ventana del navegador se cerró antes de pasar la verificación")
			}
			state = pageState{} // mid-navigation; retry until the deadline
		}
		if !headless && state.interactive() && !toldHuman {
			opts.logf("completa el captcha en la ventana del navegador para continuar")
			toldHuman = true
		}
		if state.settled(origin) {
			cookies, err := cookiesOf(ctx, conn, session, target.Hostname())
			if err != nil {
				return nil, err
			}
			// Close the window politely; killTree is just the backstop.
			closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			conn.quit(closeCtx)
			cancel()
			return cookies, nil
		}
		if headless && (state.interactive() || time.Now().After(deadline)) {
			return nil, errNeedsHuman
		}
		if time.Now().After(deadline) {
			return nil, errors.New("no se completó la verificación en el navegador a tiempo; vuelve a intentar el sync y completa el captcha en la ventana")
		}
	}
}

// openPage creates a blank page target and attaches a devtools session to it.
func (c *cdp) openPage(ctx context.Context) (string, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return "", fmt.Errorf("no se pudo abrir una pestaña del navegador: %w", err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil {
		return "", fmt.Errorf("no se pudo controlar la pestaña del navegador: %w", err)
	}
	for _, method := range []string{"Page.enable", "Network.enable"} {
		if err := c.call(ctx, attached.SessionID, method, nil, nil); err != nil {
			return "", err
		}
	}
	return attached.SessionID, nil
}

// pageState is the little of the page that tells whether the bot challenge is
// still up and whether it needs a human.
type pageState struct {
	Href  string `json:"href"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// challenged reports whether the page is still a SiteGround bot challenge:
// the automatic proof of work or the interactive fallback captcha.
func (s pageState) challenged() bool {
	lower := strings.ToLower(s.Href + "\n" + s.Title + "\n" + s.Body)
	return strings.Contains(lower, "sgcaptcha") ||
		strings.Contains(lower, "well-known/captcha") ||
		strings.Contains(lower, "robot challenge screen") ||
		strings.Contains(lower, "complete the captcha")
}

// interactive reports whether the challenge is the fallback captcha only a
// person can pass, as opposed to the automatic proof of work.
func (s pageState) interactive() bool {
	lower := strings.ToLower(s.Href + "\n" + s.Body)
	return strings.Contains(lower, "well-known/captcha") || strings.Contains(lower, "complete the captcha")
}

// settled reports whether the store page itself finally loaded.
func (s pageState) settled(origin string) bool {
	return s.Href != "" && s.Href != "about:blank" &&
		strings.HasPrefix(s.Href, origin) && !s.challenged()
}

const readStateScript = `JSON.stringify({` +
	`href: location.href,` +
	`title: document.title,` +
	`body: ((document.body && document.body.innerText) || "").replace(/\s+/g, " ").slice(0, 400)` +
	`})`

// readState reads where the page is and what it shows.
func readState(ctx context.Context, c *cdp, session string) (pageState, error) {
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := c.call(ctx, session, "Runtime.evaluate", map[string]any{
		"expression":    readStateScript,
		"returnByValue": true,
	}, &res); err != nil {
		return pageState{}, err
	}
	var state pageState
	if err := json.Unmarshal([]byte(res.Result.Value), &state); err != nil {
		return pageState{}, err
	}
	return state, nil
}

// cdpCookie is one cookie as the DevTools protocol reports it.
type cdpCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
}

// cookiesOf reads the cookies the browser holds for the store's domain.
func cookiesOf(ctx context.Context, c *cdp, session, host string) ([]*http.Cookie, error) {
	var list struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	if err := c.call(ctx, session, "Network.getAllCookies", map[string]any{}, &list); err != nil {
		if err := c.call(ctx, session, "Storage.getCookies", map[string]any{}, &list); err != nil {
			return nil, fmt.Errorf("no se pudieron leer las cookies del navegador: %w", err)
		}
	}
	return toCookies(host, list.Cookies), nil
}

// toCookies keeps the cookies that belong to host and converts them for the
// HTTP client.
func toCookies(host string, list []cdpCookie) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(list))
	for _, c := range list {
		if c.Name == "" || !cookieDomainMatch(host, c.Domain) {
			continue
		}
		cookie := &http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   strings.TrimPrefix(c.Domain, "."),
			Path:     c.Path,
			HttpOnly: c.HTTPOnly,
			Secure:   c.Secure,
		}
		if cookie.Path == "" {
			cookie.Path = "/"
		}
		if c.Expires > 0 {
			cookie.Expires = time.Unix(int64(c.Expires), 0)
		}
		out = append(out, cookie)
	}
	return out
}

// cookieDomainMatch reports whether host belongs to the cookie's domain.
func cookieDomainMatch(host, domain string) bool {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	host = strings.ToLower(host)
	return domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

// findEdge locates the Edge browser the way the operating system does.
func findEdge() (string, error) {
	// App Paths tracks Edge updates; the default folders are the fallback.
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		for _, sub := range []string{
			`SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`,
			`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`,
		} {
			key, err := registry.OpenKey(root, sub, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			path, _, err := key.GetStringValue("")
			key.Close()
			if err == nil && path != "" {
				if _, err := os.Stat(path); err == nil {
					return path, nil
				}
			}
		}
	}
	for _, dir := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, "Microsoft", "Edge", "Application", "msedge.exe")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no se encontró Microsoft Edge en este equipo; es necesario para pasar el captcha de SiteGround")
}

// waitDebugger reads the port the browser chose from its profile.
func waitDebugger(ctx context.Context, portFile string) (string, error) {
	deadline := time.Now().Add(portBudget)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(portFile); err == nil {
			lines := strings.Fields(string(raw))
			if len(lines) >= 2 {
				return "ws://127.0.0.1:" + lines[0] + lines[1], nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return "", errors.New("el navegador no abrió su conexión de verificación a tiempo")
}

// killTree ends the browser and its renderer children.
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}
