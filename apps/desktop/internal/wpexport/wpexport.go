// Package wpexport pulls the day's orders from the store's WP All Export Pro
// saved export, the same flow as tools/orders-export: log into wp-admin,
// rewrite the export's delivery-date filter, trigger it through the cron key
// and download the XLSX. The export is filtered by delivery date, so it sees
// every order for that day no matter when it was created.
package wpexport

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/antibot"
)

// Config are the store credentials and the saved export to run.
type Config struct {
	// Base is the site root, e.g. https://palorosabreakfast.com.
	Base     string
	User     string
	Password string
	// ExportID is the WP All Export saved export id.
	ExportID string
	// CronKey is the WP All Export "Cron Job Key" (Settings).
	CronKey string
	// Token is the download token; empty derives it from the cron key.
	Token string
	// BrowserSession opens a real browser when the store's bot challenge
	// blocks a step: the cookies it grants are adopted and the step is
	// retried once. Nil keeps the plain HTTP behaviour.
	BrowserSession func(ctx context.Context) ([]*http.Cookie, error)
}

// Token is substr(md5(cronKey + exportId), 0, 16), what WP All Export expects.
func Token(cronKey, exportID string) string {
	sum := md5.Sum([]byte(cronKey + exportID))
	return hex.EncodeToString(sum[:])[:16]
}

// UserAgent is the browser the client presents. A browser session created to
// pass the store's bot challenge must announce the same one, since the host
// may tie its whitelist to it.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

var months = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

// StoreDate turns YYYY-MM-DD into the store's meta value, e.g. "4 octubre, 2026".
func StoreDate(iso string) (string, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(iso))
	if err != nil {
		return "", fmt.Errorf("fecha inválida %q", iso)
	}
	return fmt.Sprintf("%d %s, %d", t.Day(), months[t.Month()-1], t.Year()), nil
}

// Client runs exports. One client keeps one wp-admin session.
type Client struct {
	cfg  Config
	http *http.Client
}

// New validates the config and builds a client.
func New(cfg Config) (*Client, error) {
	cfg.Base = strings.TrimRight(strings.TrimSpace(cfg.Base), "/")
	if cfg.ExportID == "" {
		cfg.ExportID = "1"
	}
	switch {
	case cfg.Base == "":
		return nil, errors.New("falta la URL de la tienda (Ajustes)")
	case cfg.User == "" || cfg.Password == "":
		return nil, errors.New("faltan el usuario y la contraseña de WordPress (Ajustes)")
	case cfg.CronKey == "":
		return nil, errors.New("falta la Cron key de WP All Export (Ajustes)")
	}
	if cfg.Token == "" {
		cfg.Token = Token(cfg.CronKey, cfg.ExportID)
	}
	jar, _ := cookiejar.New(nil)
	return &Client{cfg: cfg, http: &http.Client{Timeout: 60 * time.Second, Jar: jar}}, nil
}

func (c *Client) do(ctx context.Context, method, target string, form url.Values) (*http.Response, []byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "es-ES,es;q=0.9")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("no se pudo conectar con la tienda: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp, raw, err
}

func (c *Client) loggedIn() bool {
	for _, path := range []string{"/wp-login.php", "/wp-admin/"} {
		u, _ := url.Parse(c.cfg.Base + path)
		for _, cookie := range c.http.Jar.Cookies(u) {
			if strings.HasPrefix(cookie.Name, "wordpress_logged_in") {
				return true
			}
		}
	}
	return false
}

// withRecovery runs step; when the store's bot challenge blocks it, it mints
// a browser-granted session once, adopts its cookies and retries the step.
func (c *Client) withRecovery(ctx context.Context, step func() error) error {
	err := step()
	var challenge *antibot.ChallengeError
	if err == nil || c.cfg.BrowserSession == nil || !errors.As(err, &challenge) {
		return err
	}
	if mintErr := c.mintSession(ctx); mintErr != nil {
		return errors.Join(err, fmt.Errorf("no se pudo verificar la conexión por navegador: %w", mintErr))
	}
	return step()
}

// mintSession adopts the cookies a verified browser was granted, as if the
// store had just sent them for the site root.
func (c *Client) mintSession(ctx context.Context) error {
	cookies, err := c.cfg.BrowserSession(ctx)
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return nil
	}
	u, err := url.Parse(c.cfg.Base + "/")
	if err != nil {
		return err
	}
	c.http.Jar.SetCookies(u, cookies)
	return nil
}

// Login opens a wp-admin session, passing a bot challenge through the browser
// session when needed.
func (c *Client) Login(ctx context.Context) error {
	return c.withRecovery(ctx, func() error { return c.login(ctx) })
}

func (c *Client) login(ctx context.Context) error {
	loginURL := c.cfg.Base + "/wp-login.php"
	if _, _, err := c.do(ctx, http.MethodGet, loginURL, nil); err != nil {
		return err
	}
	u, _ := url.Parse(loginURL)
	c.http.Jar.SetCookies(u, []*http.Cookie{{Name: "wordpress_test_cookie", Value: "WP Cookie check"}})
	form := url.Values{
		"log":         {c.cfg.User},
		"pwd":         {c.cfg.Password},
		"wp-submit":   {"Acceder"},
		"redirect_to": {c.cfg.Base + "/wp-admin/"},
		"testcookie":  {"1"},
	}
	resp, body, err := c.do(ctx, http.MethodPost, loginURL, form)
	if err != nil {
		return err
	}
	if !c.loggedIn() {
		if err := antibot.Error(resp, body); err != nil {
			return err
		}
		return fmt.Errorf("WordPress no inició sesión (HTTP %d); revisa el usuario y la contraseña", resp.StatusCode)
	}
	return nil
}

type field struct{ name, value string }

var (
	formPattern     = regexp.MustCompile(`(?is)<form\b.*?</form>`)
	inputPattern    = regexp.MustCompile(`(?i)<input\b[^>]*>`)
	selectPattern   = regexp.MustCompile(`(?is)<select\b[^>]*name\s*=\s*"([^"]+)"[^>]*>(.*?)</select>`)
	textareaPattern = regexp.MustCompile(`(?is)<textarea\b[^>]*name\s*=\s*"([^"]+)"[^>]*>(.*?)</textarea>`)
	optionSelected  = regexp.MustCompile(`(?i)<option\b[^>]*\bselected\b[^>]*>`)
	checkedPattern  = regexp.MustCompile(`(?i)\bchecked\b`)
	whereDate       = regexp.MustCompile(`meta\.meta_value = '[^']*'`)
)

func attr(tag, name string) (string, bool) {
	m := regexp.MustCompile(`(?i)\b` + name + `\s*=\s*"([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1]), true
}

func parseFields(form string) []field {
	var fields []field
	for _, tag := range inputPattern.FindAllString(form, -1) {
		name, ok := attr(tag, "name")
		if !ok || name == "" {
			continue
		}
		kind, _ := attr(tag, "type")
		kind = strings.ToLower(kind)
		switch kind {
		case "submit", "button", "file", "image", "reset":
			continue
		case "checkbox", "radio":
			if checkedPattern.MatchString(tag) {
				value, ok := attr(tag, "value")
				if !ok {
					value = "on"
				}
				fields = append(fields, field{name, value})
			}
			continue
		}
		value, _ := attr(tag, "value")
		fields = append(fields, field{name, value})
	}
	for _, m := range selectPattern.FindAllStringSubmatch(form, -1) {
		value := ""
		if option := optionSelected.FindString(m[2]); option != "" {
			value, _ = attr(option, "value")
		}
		fields = append(fields, field{html.UnescapeString(m[1]), value})
	}
	for _, m := range textareaPattern.FindAllStringSubmatch(form, -1) {
		fields = append(fields, field{html.UnescapeString(m[1]), html.UnescapeString(m[2])})
	}
	return fields
}

// setStoreDate points the export's delivery-date filter at storeDate. It
// reports whether the filter was found, so a changed export is not run blind.
func setStoreDate(fields []field, storeDate string) ([]field, bool) {
	found := false
	out := make([]field, len(fields))
	for i, f := range fields {
		switch f.name {
		case "wp_all_export_value[1]":
			f.value = storeDate
		case "filter_rules_hierarhy":
			var rules []map[string]any
			if err := json.Unmarshal([]byte(f.value), &rules); err == nil {
				for _, rule := range rules {
					if rule["element"] == "cf_Seleccionar una Fecha" {
						rule["value"] = storeDate
						found = true
					}
				}
				if raw, err := json.Marshal(rules); err == nil {
					f.value = string(raw)
				}
			}
		case "whereclause":
			f.value = whereDate.ReplaceAllLiteralString(f.value, "meta.meta_value = '"+storeDate+"'")
		}
		out[i] = f
	}
	return out, found
}

// SetDeliveryDate rewrites the saved export's delivery-date filter, passing a
// bot challenge through the browser session when needed.
func (c *Client) SetDeliveryDate(ctx context.Context, storeDate string) error {
	return c.withRecovery(ctx, func() error { return c.setDeliveryDate(ctx, storeDate) })
}

func (c *Client) setDeliveryDate(ctx context.Context, storeDate string) error {
	resp, page, err := c.do(ctx, http.MethodGet, c.cfg.Base+"/wp-admin/admin.php?page=pmxe-admin-manage", nil)
	if err != nil {
		return err
	}
	if err := antibot.Error(resp, page); err != nil {
		return err
	}
	id := regexp.QuoteMeta(c.cfg.ExportID)
	nonce := regexp.MustCompile(`id=` + id + `(?:&#038;|&amp;|&)action=options(?:&#038;|&amp;|&)_wpnonce_options=([a-f0-9]+)`).FindSubmatch(page)
	if nonce == nil {
		return fmt.Errorf("no se encontró el export %s en WP All Export (¿id incorrecto o usuario sin permisos?)", c.cfg.ExportID)
	}
	optionsURL := fmt.Sprintf("%s/wp-admin/admin.php?page=pmxe-admin-manage&id=%s&action=options&_wpnonce_options=%s", c.cfg.Base, url.QueryEscape(c.cfg.ExportID), nonce[1])
	resp, page, err = c.do(ctx, http.MethodGet, optionsURL, nil)
	if err != nil {
		return err
	}
	if err := antibot.Error(resp, page); err != nil {
		return err
	}
	var form string
	for _, candidate := range formPattern.FindAllString(string(page), -1) {
		if strings.Contains(candidate, "filter_rules_hierarhy") {
			form = candidate
			break
		}
	}
	if form == "" {
		return errors.New("no se encontró el formulario de opciones del export")
	}
	fields, found := setStoreDate(parseFields(form), storeDate)
	if !found {
		return errors.New("el export no tiene el filtro «Seleccionar una Fecha»; revisa el export en WP All Export")
	}
	values := url.Values{}
	for _, f := range fields {
		values.Add(f.name, f.value)
	}
	resp, _, err = c.do(ctx, http.MethodPost, optionsURL, values)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("WordPress rechazó el cambio de fecha del export (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// Run triggers the export and waits until WP All Export reports it complete,
// passing a bot challenge through the browser session when needed.
func (c *Client) Run(ctx context.Context) error {
	return c.withRecovery(ctx, func() error { return c.run(ctx) })
}

func (c *Client) run(ctx context.Context) error {
	query := "export_key=" + url.QueryEscape(c.cfg.CronKey) + "&export_id=" + url.QueryEscape(c.cfg.ExportID)
	resp, body, err := c.do(ctx, http.MethodGet, c.cfg.Base+"/wp-load.php?"+query+"&action=trigger", nil)
	if err != nil {
		return err
	}
	if err := antibot.Error(resp, body); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("WP All Export no aceptó la Cron key (HTTP %d): %s", resp.StatusCode, snippet(body))
	}
	for attempt := 0; attempt < 30; attempt++ {
		resp, body, err := c.do(ctx, http.MethodGet, c.cfg.Base+"/wp-load.php?"+query+"&action=processing", nil)
		if err != nil {
			return err
		}
		if err := antibot.Error(resp, body); err != nil {
			return err
		}
		if strings.Contains(string(body), "complete") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("WP All Export no terminó el export a tiempo")
}

// Download fetches the last generated file, passing a bot challenge through
// the browser session when needed.
func (c *Client) Download(ctx context.Context) ([]byte, error) {
	var data []byte
	err := c.withRecovery(ctx, func() error {
		var downloadErr error
		data, downloadErr = c.download(ctx)
		return downloadErr
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) download(ctx context.Context) ([]byte, error) {
	target := fmt.Sprintf("%s/wp-load.php?security_token=%s&export_id=%s&action=get_data", c.cfg.Base, url.QueryEscape(c.cfg.Token), url.QueryEscape(c.cfg.ExportID))
	resp, body, err := c.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if err := antibot.Error(resp, body); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("no se pudo descargar el export (HTTP %d)", resp.StatusCode)
	}
	return body, nil
}

// Rows runs the export for one delivery date (YYYY-MM-DD) and returns the
// sheet rows, header first. The session is opened on first use; each step
// passes a bot challenge through the browser session when needed.
func (c *Client) Rows(ctx context.Context, isoDate string) ([][]string, error) {
	storeDate, err := StoreDate(isoDate)
	if err != nil {
		return nil, err
	}
	if !c.loggedIn() {
		if err := c.Login(ctx); err != nil {
			return nil, err
		}
	}
	if err := c.SetDeliveryDate(ctx, storeDate); err != nil {
		return nil, err
	}
	if err := c.Run(ctx); err != nil {
		return nil, err
	}
	data, err := c.Download(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := ReadXLSX(data)
	if err != nil {
		return nil, fmt.Errorf("el export no es un Excel válido: %w", err)
	}
	return rows, nil
}

func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return text
}
