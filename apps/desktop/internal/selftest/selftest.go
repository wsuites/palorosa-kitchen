// Package selftest runs the operator-facing health checks shown in Ajustes →
// Avanzado. Every check is read-only: the webhook checks send a signed sample
// order flagged as a self-test, which the hook handler validates end to end and
// then discards without touching the kitchen list.
package selftest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/antibot"
	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/engine"
)

// Header marks a webhook delivery as a self-test so the hook handler stops
// after validation (signature, JSON, delivery date) and changes nothing.
const Header = "X-Palorosa-Selftest"

// Status is the outcome of a check.
type Status string

const (
	OK      Status = "ok"
	Warn    Status = "warn"
	Fail    Status = "fail"
	Skipped Status = "skipped"
)

// Result is one check outcome.
type Result struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Status     Status `json:"status"`
	Detail     string `json:"detail"`
	DurationMs int64  `json:"durationMs"`
	CheckedAt  string `json:"checkedAt"`
}

// BotStatus is the minimal account view the WhatsApp check needs.
type BotStatus struct {
	Name    string
	Enabled bool
	Status  string
}

// Deps are the live values the checks read. Functions are called on every run
// so settings changes apply without restarting.
type Deps struct {
	CatalogPath   func() string
	PanelPort     func() int
	WebhookSecret func() string
	WP            func() WPCredentials
	TunnelURL     func() (url string, status string)
	Bots          func() []BotStatus
	PushCount     func() int
	TargetsCount  func() (enabled, total int)
}

// WPCredentials are the store credentials the WooCommerce checks use.
type WPCredentials struct {
	AdminURL       string
	AdminUser      string
	AdminPassword  string
	ConsumerKey    string
	ConsumerSecret string
}

type check struct {
	id    string
	label string
	run   func(ctx context.Context) (Status, string)
}

// Runner runs the checks and remembers the last result of each.
type Runner struct {
	deps   Deps
	client *http.Client
	checks []check

	mu   sync.Mutex
	last map[string]Result
}

// New builds a runner over deps.
func New(deps Deps) *Runner {
	r := &Runner{
		deps:   deps,
		client: &http.Client{Timeout: 15 * time.Second},
		last:   map[string]Result{},
	}
	r.checks = []check{
		{"catalog", "Catálogo de productos", r.checkCatalog},
		{"whatsapp", "Cuentas de WhatsApp", r.checkWhatsApp},
		{"woo_orders", "Leer pedidos de WooCommerce", r.checkWooOrders},
		{"wp_login", "Inicio de sesión en WordPress", r.checkWPLogin},
		{"webhook_local", "Webhook WooCommerce (local)", r.checkWebhookLocal},
		{"tunnel", "Túnel de Cloudflare", r.checkTunnel},
		{"webhook_tunnel", "Webhook WooCommerce (por el túnel, e2e)", r.checkWebhookTunnel},
		{"push", "Avisos push del navegador", r.checkPush},
		{"targets", "Destinos de avisos", r.checkTargets},
	}
	return r
}

// Last returns every check with its last result; never-run checks come back
// with an empty status.
func (r *Runner) Last() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Result, 0, len(r.checks))
	for _, c := range r.checks {
		if res, ok := r.last[c.id]; ok {
			out = append(out, res)
			continue
		}
		out = append(out, Result{ID: c.id, Label: c.label})
	}
	return out
}

// RunAll runs every check concurrently and returns the results in order.
func (r *Runner) RunAll(ctx context.Context) []Result {
	results := make([]Result, len(r.checks))
	var wg sync.WaitGroup
	for i, c := range r.checks {
		wg.Add(1)
		go func(i int, c check) {
			defer wg.Done()
			results[i] = r.runOne(ctx, c)
		}(i, c)
	}
	wg.Wait()
	return results
}

// Run runs one check by id.
func (r *Runner) Run(ctx context.Context, id string) (Result, bool) {
	for _, c := range r.checks {
		if c.id == id {
			return r.runOne(ctx, c), true
		}
	}
	return Result{}, false
}

func (r *Runner) runOne(ctx context.Context, c check) (res Result) {
	started := time.Now()
	defer func() {
		if p := recover(); p != nil {
			res.Status, res.Detail = Fail, fmt.Sprint("Error interno: ", p)
		}
		res.ID, res.Label = c.id, c.label
		res.DurationMs = time.Since(started).Milliseconds()
		res.CheckedAt = time.Now().Format(time.RFC3339)
		r.mu.Lock()
		r.last[c.id] = res
		r.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res.Status, res.Detail = c.run(ctx)
	return res
}

func (r *Runner) checkCatalog(_ context.Context) (Status, string) {
	path := r.deps.CatalogPath()
	if path == "" {
		return Fail, "No hay ruta de catálogo configurada"
	}
	catalog, err := engine.LoadCatalog(path)
	if err != nil {
		return Fail, "No se pudo leer " + path + ": " + err.Error()
	}
	if len(catalog.Products) == 0 {
		return Warn, "El catálogo no tiene productos"
	}
	return OK, fmt.Sprintf("%d productos, %d recetas", len(catalog.Products), len(catalog.Recipes))
}

func (r *Runner) checkWhatsApp(_ context.Context) (Status, string) {
	if r.deps.Bots == nil {
		return Skipped, "WhatsApp está desactivado"
	}
	bots := r.deps.Bots()
	if len(bots) == 0 {
		return Warn, "No hay cuentas configuradas"
	}
	connected, enabled := 0, 0
	problems := []string{}
	for _, bot := range bots {
		if !bot.Enabled {
			continue
		}
		enabled++
		if bot.Status == "connected" {
			connected++
		} else {
			problems = append(problems, bot.Name+": "+bot.Status)
		}
	}
	detail := fmt.Sprintf("%d de %d cuentas activas conectadas", connected, enabled)
	if len(problems) > 0 {
		detail += " · " + strings.Join(problems, ", ")
	}
	switch {
	case enabled == 0:
		return Warn, "Todas las cuentas están desactivadas"
	case connected == 0:
		return Fail, detail
	case connected < enabled:
		return Warn, detail
	}
	return OK, detail
}

func (r *Runner) checkWooOrders(ctx context.Context) (Status, string) {
	wp := r.deps.WP()
	if wp.ConsumerKey == "" || wp.ConsumerSecret == "" {
		return Skipped, "Faltan la clave y el secreto de la API de WooCommerce"
	}
	endpoint := strings.TrimRight(wp.AdminURL, "/") + "/wp-json/wc/v3/orders?per_page=5&status=processing&_fields=id,number,status"
	var orders []struct {
		Number string `json:"number"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Fail, err.Error()
	}
	req.SetBasicAuth(wp.ConsumerKey, wp.ConsumerSecret)
	resp, err := r.client.Do(req)
	if err != nil {
		return Fail, "No se pudo conectar con la tienda: " + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return Fail, "La tienda rechazó las credenciales de la API (HTTP " + strconv.Itoa(resp.StatusCode) + ")"
	default:
		if err := antibot.Error(resp, raw); err != nil {
			return Fail, err.Error()
		}
		return Fail, fmt.Sprintf("La tienda respondió HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, &orders); err != nil {
		return Fail, "Respuesta inesperada de la API"
	}
	total := resp.Header.Get("X-WP-Total")
	if total == "" {
		total = strconv.Itoa(len(orders))
	}
	return OK, total + " pedidos en preparación"
}

func (r *Runner) checkWPLogin(ctx context.Context) (Status, string) {
	wp := r.deps.WP()
	if wp.AdminUser == "" || wp.AdminPassword == "" {
		return Skipped, "Faltan el usuario y la contraseña de WordPress"
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 15 * time.Second, Jar: jar}
	base := strings.TrimRight(wp.AdminURL, "/")
	form := url.Values{
		"log":         {wp.AdminUser},
		"pwd":         {wp.AdminPassword},
		"wp-submit":   {"Log In"},
		"redirect_to": {base + "/wp-admin/"},
		"testcookie":  {"1"},
	}
	// WordPress refuses the login unless the test cookie is already set.
	loginURL, _ := url.Parse(base + "/wp-login.php")
	jar.SetCookies(loginURL, []*http.Cookie{{Name: "wordpress_test_cookie", Value: "WP Cookie check"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return Fail, err.Error()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// The store's nginx answers 403 to non-browser user agents on wp-login.php.
	req.Header.Set("User-Agent", browserUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Fail, "No se pudo conectar con WordPress: " + err.Error()
	}
	page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	for _, cookie := range jar.Cookies(loginURL) {
		if strings.HasPrefix(cookie.Name, "wordpress_logged_in") {
			return OK, "Sesión iniciada como " + wp.AdminUser
		}
	}
	adminURL, _ := url.Parse(base + "/wp-admin/")
	for _, cookie := range jar.Cookies(adminURL) {
		if strings.HasPrefix(cookie.Name, "wordpress_logged_in") {
			return OK, "Sesión iniciada como " + wp.AdminUser
		}
	}
	return Fail, loginFailure(resp, string(page), base)
}

// browserUserAgent is sent to WordPress pages that reject scripted clients.
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36"

// loginErrorPattern captures the message WordPress prints in #login_error.
var loginErrorPattern = regexp.MustCompile(`(?s)<div[^>]*id="login_error"[^>]*>(.*?)</div>`)
var htmlTag = regexp.MustCompile(`<[^>]+>`)

// loginFailure explains why no session cookie came back: the WordPress error
// text when there is one, otherwise what the response looked like, so a
// captcha, firewall or redirect is not reported as a wrong password.
func loginFailure(resp *http.Response, page, base string) string {
	if err := antibot.Error(resp, []byte(page)); err != nil {
		return err.Error()
	}
	if m := loginErrorPattern.FindStringSubmatch(page); m != nil {
		text := strings.Join(strings.Fields(html.UnescapeString(htmlTag.ReplaceAllString(m[1], " "))), " ")
		text = strings.TrimPrefix(strings.TrimPrefix(text, "Error:"), "ERROR:")
		if text = strings.TrimSpace(text); text != "" {
			return "WordPress respondió: " + text
		}
	}
	final := resp.Request.URL
	lower := strings.ToLower(page)
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Sprintf("El servidor bloqueó el intento (HTTP %d). Puede ser un firewall o un plugin de seguridad", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return "No hay página de inicio de sesión en " + base + "/wp-login.php. Revisa la URL de WordPress"
	case strings.Contains(lower, "recaptcha") || strings.Contains(lower, "hcaptcha") || strings.Contains(lower, "cf-turnstile"):
		return "La página de inicio de sesión pide un captcha; no se puede comprobar automáticamente"
	case !strings.EqualFold(final.Hostname(), hostOf(base)):
		return "WordPress redirigió a " + final.Host + ". Pon esa dirección como URL de WordPress"
	case !strings.Contains(final.Path, "wp-login.php"):
		return fmt.Sprintf("WordPress redirigió a %s sin iniciar sesión; un plugin puede estar cambiando el inicio de sesión", final.Path)
	case strings.Contains(lower, `name="pwd"`):
		return "WordPress volvió a mostrar el formulario sin explicar el motivo; revisa el usuario y la contraseña"
	}
	return fmt.Sprintf("WordPress no devolvió una sesión (HTTP %d en %s)", resp.StatusCode, final.Path)
}

func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (r *Runner) checkWebhookLocal(ctx context.Context) (Status, string) {
	port := r.deps.PanelPort()
	return r.sendSampleWebhook(ctx, fmt.Sprintf("http://127.0.0.1:%d/hook/woocommerce", port))
}

func (r *Runner) checkTunnel(ctx context.Context) (Status, string) {
	publicURL, status := r.deps.TunnelURL()
	if status == "disabled" {
		return Skipped, "No hay túnel configurado"
	}
	if status != "running" || publicURL == "" {
		return Fail, "El túnel no está activo (" + status + ")"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicURL+"/health", nil)
	if err != nil {
		return Fail, err.Error()
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Fail, "No se alcanza " + publicURL + ": " + err.Error()
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Warn, fmt.Sprintf("%s respondió HTTP %d", publicURL, resp.StatusCode)
	}
	return OK, publicURL + " responde"
}

func (r *Runner) checkWebhookTunnel(ctx context.Context) (Status, string) {
	publicURL, status := r.deps.TunnelURL()
	if status == "disabled" {
		return Skipped, "No hay túnel configurado"
	}
	if publicURL == "" || status != "running" {
		return Fail, "El túnel no está activo; WooCommerce no puede entregar pedidos"
	}
	return r.sendSampleWebhook(ctx, publicURL+"/hook/woocommerce")
}

// sendSampleWebhook posts a signed, self-test flagged sample order exactly as
// WooCommerce would, and checks the handler accepted it.
func (r *Runner) sendSampleWebhook(ctx context.Context, endpoint string) (Status, string) {
	secret := r.deps.WebhookSecret()
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body, _ := json.Marshal(map[string]any{
		"id":     0,
		"number": "SELFTEST",
		"status": "processing",
		"meta_data": []map[string]any{
			{"key": "_orddd_delivery_date", "value": tomorrow},
			{"key": "desayuno_excel", "value": ""},
		},
		"line_items": []any{},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Fail, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WC-Webhook-Topic", "order.created")
	req.Header.Set(Header, "1")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-WC-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Fail, "No se pudo entregar: " + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var reply struct {
		OK     bool   `json:"ok"`
		DryRun bool   `json:"dryRun"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(raw, &reply)
	switch {
	case resp.StatusCode == http.StatusOK && reply.DryRun:
		if secret == "" {
			return Warn, "Funciona, pero no hay secreto de webhook: cualquiera podría enviar pedidos"
		}
		return OK, "Pedido de prueba recibido y validado (firma correcta)"
	case resp.StatusCode == http.StatusOK:
		return Warn, "Respondió OK pero sin modo de prueba; ¿versión antigua?"
	case resp.StatusCode == http.StatusUnauthorized && reply.Error == "Invalid signature":
		return Fail, "Firma rechazada: el secreto no coincide"
	case resp.StatusCode == http.StatusNotFound:
		return Fail, "El receptor de webhooks no está activo (¿WhatsApp desactivado?)"
	case reply.Error != "":
		return Fail, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, reply.Error)
	default:
		return Fail, fmt.Sprintf("HTTP %d (¿Cloudflare Access bloquea /hook/?)", resp.StatusCode)
	}
}

func (r *Runner) checkPush(_ context.Context) (Status, string) {
	if r.deps.PushCount == nil {
		return Skipped, "Avisos push no disponibles"
	}
	count := r.deps.PushCount()
	if count == 0 {
		return Warn, "Ningún navegador suscrito"
	}
	return OK, fmt.Sprintf("%d navegadores suscritos", count)
}

func (r *Runner) checkTargets(_ context.Context) (Status, string) {
	if r.deps.TargetsCount == nil {
		return Skipped, "Gestor de avisos no disponible"
	}
	enabled, total := r.deps.TargetsCount()
	switch {
	case total == 0:
		return Warn, "No hay destinos configurados"
	case enabled == 0:
		return Warn, fmt.Sprintf("Los %d destinos están desactivados", total)
	}
	return OK, fmt.Sprintf("%d de %d destinos activos", enabled, total)
}

func (r *Runner) getJSON(ctx context.Context, endpoint string, header http.Header, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	for key, values := range header {
		req.Header[key] = values
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, nil
}
