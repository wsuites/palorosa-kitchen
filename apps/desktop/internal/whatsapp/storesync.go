package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/antibot"
	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/engine"
	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/labels"
	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/wpexport"
)

// ExportRows returns the WP All Export sheet (header first) for one delivery
// date (YYYY-MM-DD).
type ExportRows func(ctx context.Context, date string) ([][]string, error)

// SyncRequest is one store refresh: the days to reconcile, both order sources
// and whether to force the full export.
type SyncRequest struct {
	Dates []string
	// ExportRows runs the WP All Export for one day. Nil when the cron key is
	// missing: the sync then falls back to the creation-range lookup.
	ExportRows ExportRows
	// APIBase, APIKey and APISecret are the WooCommerce REST credentials of
	// the fast creation-range lookup. Empty disables it.
	APIBase   string
	APIKey    string
	APISecret string
	// BrowserSession opens a real browser when the store's bot challenge
	// blocks the lookup: the cookies it grants are adopted and the request
	// retried once. See wpexport.Config.BrowserSession.
	BrowserSession func(ctx context.Context) ([]*http.Cookie, error)
	// CreatedDays is the lookup window in days back from now. Zero means
	// DefaultCreatedDays.
	CreatedDays int
	// Full forces the full export even for days that already have one.
	Full bool
}

// DefaultCreatedDays is how far back the fast lookup looks for created orders.
const DefaultCreatedDays = 45

// maxCreatedDays caps the lookup window so one sync cannot page through years.
const maxCreatedDays = 365

// wooOrdersPerPage and wooMaxPages bound one WooCommerce lookup.
const (
	wooOrdersPerPage = 100
	wooMaxPages      = 100
)

// SyncResult summarises a store sync for the panel.
type SyncResult struct {
	// Kept is the number of orders across the synced dates.
	Kept    int            `json:"kept"`
	Added   int            `json:"added"`
	Removed int            `json:"removed"`
	Dates   []string       `json:"dates"`
	PerDate map[string]int `json:"perDate"`
	// Exported are the days reconciled with the full export this run;
	// Refreshed the days updated with the creation-range lookup.
	Exported  []string `json:"exported,omitempty"`
	Refreshed []string `json:"refreshed,omitempty"`
	At        string   `json:"at"`
}

// SyncStore brings the requested days up to date. A day that was never
// reconciled pays the full export once (exact replace, marker saved); every
// later refresh uses the WooCommerce lookup by creation range, which is fast
// but cannot prove an order is gone, so it only adds, updates and removes the
// orders it actually saw.
func (c *Client) SyncStore(ctx context.Context, req SyncRequest) (SyncResult, error) {
	result := SyncResult{Dates: []string{}, PerDate: map[string]int{}, Exported: []string{}, Refreshed: []string{}}
	if c.kitchen.Index() == nil {
		return result, errors.New("el catálogo no está cargado")
	}
	if len(req.Dates) == 0 {
		return result, errors.New("sin fechas para sincronizar")
	}
	canExport := req.ExportRows != nil
	canLookup := strings.TrimSpace(req.APIBase) != "" && req.APIKey != "" && req.APISecret != ""

	var exports, lookups []string
	var failures []string
	c.kitchen.mu.Lock()
	for _, date := range req.Dates {
		_, exported := c.kitchen.state.Exported[date]
		switch {
		case (req.Full || !exported) && canExport:
			exports = append(exports, date)
		case canLookup:
			lookups = append(lookups, date)
		case canExport:
			// No lookup and no marker proof: the export is the only safe path.
			exports = append(exports, date)
		default:
			failures = append(failures, fmt.Sprintf("%s: falta la Cron key de WP All Export", date))
		}
	}
	c.kitchen.mu.Unlock()

	for _, date := range exports {
		orders, notes, annotations, err := exportOrders(ctx, req.ExportRows, date)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", date, err))
			continue
		}
		added, removed := c.replaceDay(date, orders, notes, annotations)
		c.markExported(date)
		result.Added += added
		result.Removed += removed
		result.Kept += len(orders)
		result.PerDate[date] = len(orders)
		result.Dates = append(result.Dates, date)
		result.Exported = append(result.Exported, date)
	}

	if len(lookups) > 0 && canLookup {
		window := req.CreatedDays
		if window <= 0 {
			window = DefaultCreatedDays
		}
		if window > maxCreatedDays {
			window = maxCreatedDays
		}
		orders, err := c.fetchStoreOrders(ctx, req, time.Now().AddDate(0, 0, -window))
		if err != nil {
			failures = append(failures, err.Error())
		} else {
			added, removed, kept, perDate, dates := c.applyLookup(lookups, orders)
			result.Added += added
			result.Removed += removed
			result.Kept += kept
			for date, count := range perDate {
				result.PerDate[date] = count
			}
			result.Dates = append(result.Dates, dates...)
			result.Refreshed = append(result.Refreshed, dates...)
		}
	}

	sort.Strings(result.Dates)
	result.At = time.Now().Format(time.RFC3339)
	if len(result.Dates) > 0 {
		c.changed()
	}
	if len(failures) > 0 {
		return result, errors.New(strings.Join(failures, "; "))
	}
	return result, nil
}

// applyLookup groups the fetched orders by delivery date and applies the
// requested days one order at a time, so every real change goes through the
// same path (and the same notice) a webhook uses. That makes the lookup the
// fallback for a webhook that never arrived: new, modified and cancelled
// orders are notified from here without touching the ones already stored.
func (c *Client) applyLookup(dates []string, orders []woocommerceOrder) (added, removed, kept int, perDate map[string]int, touched []string) {
	wanted := make(map[string]bool, len(dates))
	for _, date := range dates {
		wanted[date] = true
	}
	seen := map[string]map[string][]engine.ParsedOrderLine{}
	notes := map[string]map[string]string{}
	annotations := map[string]map[string]engine.OrderAnnotation{}
	removals := map[string]map[string]string{}
	for index := range orders {
		order := &orders[index]
		row := wooRow(order)
		export := engine.ParseWideOrderRows([]map[string]any{row}, engine.WideExportOptions{})
		date := export.DeliveryDate
		if date == "" || !wanted[date] {
			continue
		}
		number := strings.TrimSpace(order.Number)
		if number == "" {
			number = strings.TrimSpace(stringOfAny(row["ID orden"]))
		}
		if number == "" {
			continue
		}
		if orderIsCooked(order.Status) {
			if seen[date] == nil {
				seen[date] = map[string][]engine.ParsedOrderLine{}
				notes[date] = map[string]string{}
				annotations[date] = map[string]engine.OrderAnnotation{}
			}
			seen[date][number] = export.Lines
			if note := wooObservation(order); note != "" {
				notes[date][number] = note
			}
			if annotation, ok := export.Annotations[number]; ok {
				annotations[date][number] = annotation
			}
		} else {
			if removals[date] == nil {
				removals[date] = map[string]string{}
			}
			removals[date][number] = order.Status
		}
	}

	perDate = make(map[string]int, len(dates))
	for _, date := range dates {
		add, drop, total := c.applyLookupDay(date, seen[date], notes[date], annotations[date], removals[date])
		added += add
		removed += drop
		kept += total
		perDate[date] = total
		touched = append(touched, date)
	}
	sort.Strings(touched)
	return added, removed, kept, perDate, touched
}

// storedOrder is the stored state of one order, used to decide whether a
// lookup result is a change. `present` distinguishes an order with no parsed
// lines from an order that is not stored at all.
type storedOrder struct {
	present    bool
	lines      []engine.ParsedOrderLine
	note       string
	annotation engine.OrderAnnotation
}

// applyLookupDay applies one day's lookup rows. Orders that match the stored
// state are skipped; the rest go through applyOrder/applyRemoval, so a missed
// webhook still produces the notice a webhook would, exactly once.
func (c *Client) applyLookupDay(date string, orders map[string][]engine.ParsedOrderLine, notes map[string]string, annotations map[string]engine.OrderAnnotation, removals map[string]string) (added, removed, total int) {
	c.kitchen.mu.Lock()
	previous := make(map[string]storedOrder, len(orders)+len(removals))
	for number := range orders {
		previous[number] = c.storedOrderLocked(date, number)
	}
	for number := range removals {
		previous[number] = c.storedOrderLocked(date, number)
	}
	c.kitchen.mu.Unlock()

	for number, lines := range orders {
		before := previous[number]
		if before.present &&
			sameUnits(c.orderUnits(before.lines), c.orderUnits(lines)) &&
			orderText(before.lines) == orderText(lines) &&
			before.note == notes[number] &&
			before.annotation == annotations[number] {
			continue
		}
		title := labels.Bot.UpdatedOrder + " #" + number
		kind := eventUpdate
		if !before.present {
			title = labels.Bot.NewOrder + " #" + number
			kind = eventNew
			added++
		}
		c.applyOrder(date, number, lines, title, notes[number], annotations[number], kind)
	}
	for number, status := range removals {
		if !previous[number].present {
			continue
		}
		c.applyRemoval(date, number, statusTitle(status)+" #"+number)
		removed++
	}

	c.kitchen.mu.Lock()
	total = len(c.kitchen.state.Orders[date])
	c.kitchen.mu.Unlock()
	return added, removed, total
}

// storedOrderLocked copies one order's stored state. The caller holds
// kitchen.mu.
func (c *Client) storedOrderLocked(date, number string) storedOrder {
	lines, present := c.kitchen.state.Orders[date][number]
	return storedOrder{
		present:    present,
		lines:      lines,
		note:       c.kitchen.state.Observations[date][number],
		annotation: c.kitchen.state.Annotations[date][number],
	}
}

// markExported records that the day paid for the full export once, so the next
// refresh can use the fast lookup.
func (c *Client) markExported(date string) {
	c.kitchen.mu.Lock()
	defer c.kitchen.mu.Unlock()
	if c.kitchen.state.Exported == nil {
		c.kitchen.state.Exported = map[string]time.Time{}
	}
	c.kitchen.state.Exported[date] = time.Now()
	_ = saveBotState(c.kitchen.path, c.kitchen.state)
}

// SyncFromExport runs the store's WP All Export for each delivery date and
// makes that day's orders match the export exactly: orders in the sheet are
// stored or replaced, orders missing from it are dropped. The export filters
// by delivery date, so it never misses an order however early it was booked.
// It never sends WhatsApp notices; it repairs missed or failed webhooks.
// Dates are synced one after another (the export is a single saved config).
func (c *Client) SyncFromExport(ctx context.Context, fetch ExportRows, dates []string) (SyncResult, error) {
	result := SyncResult{Dates: []string{}, PerDate: map[string]int{}}
	if c.kitchen.Index() == nil {
		return result, errors.New("el catálogo no está cargado")
	}
	var failures []string
	for _, date := range dates {
		orders, notes, annotations, err := exportOrders(ctx, fetch, date)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", date, err))
			continue
		}
		added, removed := c.replaceDay(date, orders, notes, annotations)
		result.Added += added
		result.Removed += removed
		result.Kept += len(orders)
		result.PerDate[date] = len(orders)
		result.Dates = append(result.Dates, date)
	}
	sort.Strings(result.Dates)
	result.At = time.Now().Format(time.RFC3339)
	if len(result.Dates) > 0 {
		c.changed()
	}
	if len(failures) > 0 {
		return result, errors.New(strings.Join(failures, "; "))
	}
	return result, nil
}

// exportOrders runs the export for one date and parses it into order lines.
// A sheet whose rows belong to another date means the filter did not apply,
// and is rejected rather than wiping the day.
func exportOrders(ctx context.Context, fetch ExportRows, date string) (map[string][]engine.ParsedOrderLine, map[string]string, map[string]engine.OrderAnnotation, error) {
	rows, err := fetch(ctx, date)
	if err != nil {
		return nil, nil, nil, err
	}
	orders := map[string][]engine.ParsedOrderLine{}
	notes := map[string]string{}
	annotations := map[string]engine.OrderAnnotation{}
	if len(rows) == 0 || (len(rows) == 1 && isBlankRow(rows[0])) {
		return orders, notes, annotations, nil
	}
	if !engine.IsWideOrderHeader(rows[0]) {
		return nil, nil, nil, errors.New("el export no tiene las columnas esperadas («ID orden», «Productos»)")
	}
	for _, record := range engine.TableToRecords(rows) {
		number := strings.TrimSpace(fmt.Sprint(record["ID orden"]))
		if number == "" {
			continue
		}
		// The optional status column (Ajustes → the saved export gets an
		// "Estado" field) keeps cancelled orders out of the day the same way
		// the lookup does. An export without the column is kept as it was.
		if status := exportRowStatus(record); status != "" && !orderIsCooked(status) {
			continue
		}
		parsed := engine.ParseWideOrderRows([]map[string]any{record}, engine.WideExportOptions{})
		if parsed.DeliveryDate != "" && parsed.DeliveryDate != date {
			return nil, nil, nil, fmt.Errorf("el export devolvió pedidos del %s; el filtro de fecha no se aplicó", parsed.DeliveryDate)
		}
		orders[number] = append(orders[number], parsed.Lines...)
		if annotation, ok := parsed.Annotations[number]; ok {
			annotations[number] = annotation
		}
		if note := strings.TrimSpace(fmt.Sprint(record["Observaciones"])); note != "" && note != "<nil>" {
			notes[number] = note
		}
	}
	return orders, notes, annotations, nil
}

func isBlankRow(row []string) bool {
	for _, cell := range row {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

// replaceDay swaps a day's orders for the exported ones and rebuilds its list.
func (c *Client) replaceDay(date string, orders map[string][]engine.ParsedOrderLine, notes map[string]string, annotations map[string]engine.OrderAnnotation) (added, removed int) {
	c.kitchen.mu.Lock()
	defer c.kitchen.mu.Unlock()
	state := c.kitchen.state
	previous := state.Orders[date]
	for number := range orders {
		if _, ok := previous[number]; !ok {
			added++
		}
	}
	for number := range previous {
		if _, ok := orders[number]; !ok {
			removed++
		}
	}
	if len(orders) == 0 {
		delete(state.Orders, date)
		delete(state.Observations, date)
		delete(state.Annotations, date)
	} else {
		state.Orders[date] = orders
		if state.Observations == nil {
			state.Observations = map[string]map[string]string{}
		}
		state.Observations[date] = notes
		if state.Annotations == nil {
			state.Annotations = map[string]map[string]engine.OrderAnnotation{}
		}
		state.Annotations[date] = annotations
	}
	if next := c.listFromStateLocked(date); next != nil {
		state.Lists[date] = *next
	} else {
		delete(state.Lists, date)
	}
	if added > 0 || removed > 0 {
		addEventLocked(state, "update", fmt.Sprintf("Sincronizado con la tienda (%s): %d pedidos, %d nuevos, %d quitados", date, len(orders), added, removed))
	}
	_ = saveBotState(c.kitchen.path, state)
	return added, removed
}

// fetchStoreOrders pages the WooCommerce REST orders created after `after`.
// The delivery date lives in meta_data, which the REST API cannot filter, so
// the lookup takes a creation window and the caller groups by delivery date.
// A page blocked by the host bot challenge is retried once through a browser
// session, when one is set.
func (c *Client) fetchStoreOrders(ctx context.Context, req SyncRequest, after time.Time) ([]woocommerceOrder, error) {
	base := strings.TrimRight(strings.TrimSpace(req.APIBase), "/")
	if base == "" {
		return nil, errors.New("falta la URL de la tienda (Ajustes)")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 60 * time.Second, Jar: jar}
	var out []woocommerceOrder
	recovered := false
	for page := 1; page <= wooMaxPages; page++ {
		endpoint := base + "/wp-json/wc/v3/orders?per_page=" + strconv.Itoa(wooOrdersPerPage) +
			"&page=" + strconv.Itoa(page) +
			"&orderby=date&order=desc" +
			"&after=" + url.QueryEscape(after.Format(time.RFC3339))
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		request.SetBasicAuth(req.APIKey, req.APISecret)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", wpexport.UserAgent)
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("no se pudo consultar la tienda: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<20))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			if challenge := antibot.Error(response, raw); challenge != nil {
				if req.BrowserSession == nil || recovered {
					return nil, challenge
				}
				recovered = true
				if mintErr := adoptSession(ctx, jar, base, req.BrowserSession); mintErr != nil {
					return nil, errors.Join(challenge, fmt.Errorf("no se pudo verificar la conexión por navegador: %w", mintErr))
				}
				page-- // the same page again, now with the browser cookies
				continue
			}
			return nil, fmt.Errorf("la tienda respondió %s al listar pedidos", response.Status)
		}
		if readErr != nil {
			return nil, readErr
		}
		var batch []woocommerceOrder
		if err := json.Unmarshal(raw, &batch); err != nil {
			return nil, fmt.Errorf("respuesta inesperada de la tienda: %w", err)
		}
		out = append(out, batch...)
		if len(batch) < wooOrdersPerPage {
			break
		}
	}
	return out, nil
}

// adoptSession mints a browser-granted session and stores its cookies for the
// store, so the retried request presents the verified identity.
func adoptSession(ctx context.Context, jar *cookiejar.Jar, base string, mint func(context.Context) ([]*http.Cookie, error)) error {
	cookies, err := mint(ctx)
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return nil
	}
	u, err := url.Parse(base + "/")
	if err != nil {
		return err
	}
	jar.SetCookies(u, cookies)
	return nil
}
