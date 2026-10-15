package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsuites/palorosa-kitchen/apps/desktop/internal/engine"
)

// orderJSON builds one WooCommerce order payload for the lookup tests.
func orderJSON(number, status, spanishDate string) map[string]any {
	return map[string]any{
		"number": number,
		"status": status,
		"meta_data": []map[string]any{
			{"key": "Seleccionar una Fecha", "value": spanishDate},
		},
		"line_items": []any{},
	}
}

func lookupServer(t *testing.T, orders []map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "ck" || pass != "cs" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(orders)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestSyncStoreExportsOnceThenUsesLookup is the hybrid rule: the first sync of
// a day pays the full export (exact replace), every later sync uses the fast
// creation-range lookup, and a forced sync exports again.
func TestSyncStoreExportsOnceThenUsesLookup(t *testing.T) {
	c := testClient(t)
	exportCalls := 0
	fetch := func(_ context.Context, date string) ([][]string, error) {
		exportCalls++
		if date != "2026-10-04" {
			return [][]string{header}, nil
		}
		return [][]string{
			header,
			{"1", "4 octubre, 2026", "", "", ""},
			{"2", "4 octubre, 2026", "", "", ""},
		}, nil
	}
	server := lookupServer(t, []map[string]any{
		orderJSON("2", "processing", "4 octubre, 2026"),
		orderJSON("3", "processing", "4 octubre, 2026"),
		orderJSON("1", "cancelled", "4 octubre, 2026"),
		orderJSON("4", "processing", "5 octubre, 2026"),
	})
	request := func(full bool) SyncRequest {
		return SyncRequest{
			Dates:       []string{"2026-10-04"},
			ExportRows:  fetch,
			APIBase:     server.URL,
			APIKey:      "ck",
			APISecret:   "cs",
			CreatedDays: 45,
			Full:        full,
		}
	}

	first, err := c.SyncStore(context.Background(), request(false))
	if err != nil {
		t.Fatal(err)
	}
	if exportCalls != 1 || len(first.Exported) != 1 || len(first.Refreshed) != 0 {
		t.Fatalf("first sync = %+v, exportCalls = %d", first, exportCalls)
	}
	if day := c.kitchen.state.Orders["2026-10-04"]; len(day) != 2 {
		t.Fatalf("day after export = %v", day)
	}
	if _, ok := c.kitchen.state.Exported["2026-10-04"]; !ok {
		t.Fatal("day was not marked as exported")
	}

	second, err := c.SyncStore(context.Background(), request(false))
	if err != nil {
		t.Fatal(err)
	}
	if exportCalls != 1 || len(second.Refreshed) != 1 || len(second.Exported) != 0 {
		t.Fatalf("second sync = %+v, exportCalls = %d", second, exportCalls)
	}
	day := c.kitchen.state.Orders["2026-10-04"]
	_, hasTwo := day["2"]
	_, hasThree := day["3"]
	_, hasOne := day["1"]
	if len(day) != 2 || !hasTwo || !hasThree || hasOne {
		t.Fatalf("day after lookup = %v", day)
	}
	if second.Added != 1 || second.Removed != 1 {
		t.Fatalf("lookup counts = added %d, removed %d", second.Added, second.Removed)
	}

	forced, err := c.SyncStore(context.Background(), request(true))
	if err != nil {
		t.Fatal(err)
	}
	if exportCalls != 2 || len(forced.Exported) != 1 {
		t.Fatalf("forced sync = %+v, exportCalls = %d", forced, exportCalls)
	}
	day = c.kitchen.state.Orders["2026-10-04"]
	if len(day) != 2 {
		t.Fatalf("day after forced export = %v", day)
	}
	if _, ok := day["3"]; ok {
		t.Fatal("the lookup-added order survived the forced export")
	}
}

func TestFetchStoreOrdersPaginates(t *testing.T) {
	c := testClient(t)
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		count := 100
		if page == 2 {
			count = 1
		}
		if page > 2 {
			count = 0
		}
		batch := make([]map[string]any, 0, count)
		for i := 0; i < count; i++ {
			batch = append(batch, orderJSON(fmt.Sprintf("%d", page*1000+i), "processing", "4 octubre, 2026"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(batch)
	}))
	t.Cleanup(server.Close)

	orders, err := c.fetchStoreOrders(context.Background(), SyncRequest{APIBase: server.URL, APIKey: "ck", APISecret: "cs"}, time.Now().AddDate(0, 0, -45))
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 101 {
		t.Fatalf("orders = %d, want 101", len(orders))
	}
	if page != 2 {
		t.Fatalf("pages = %d, want 2", page)
	}
}

// TestFetchStoreOrdersRecoversFromBotChallenge is the Anti-Bot rule: when the
// host answers the lookup with the challenge, a browser session mints the
// cookies once and the same page is retried with them.
func TestFetchStoreOrdersRecoversFromBotChallenge(t *testing.T) {
	c := testClient(t)
	requests, sessions := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !strings.Contains(r.Header.Get("Cookie"), "_I_=") {
			w.Header().Set("SG-Captcha", "challenge")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`<html>Robot Challenge Screen</html>`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{orderJSON("7", "processing", "4 octubre, 2026")})
	}))
	t.Cleanup(server.Close)

	orders, err := c.fetchStoreOrders(context.Background(), SyncRequest{
		APIBase: server.URL, APIKey: "ck", APISecret: "cs",
		BrowserSession: func(context.Context) ([]*http.Cookie, error) {
			sessions++
			return []*http.Cookie{{Name: "_I_", Value: "whitelisted", Path: "/"}}, nil
		},
	}, time.Now().AddDate(0, 0, -45))
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || sessions != 1 || requests != 2 {
		t.Fatalf("orders = %d, sessions = %d, requests = %d", len(orders), sessions, requests)
	}
}

// spanishDate renders one YYYY-MM-DD date the way the store meta does.
func spanishDate(date string) string {
	months := [...]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return fmt.Sprintf("%d %s, %d", parsed.Day(), months[parsed.Month()-1], parsed.Year())
}

// storedLines builds the parsed lines of one order, the way the lookup stores
// them.
func storedLines(t testing.TB, number, date, products string) []engine.ParsedOrderLine {
	t.Helper()
	row := wooRow(&woocommerceOrder{
		Number: number,
		Status: "processing",
		MetaData: []wooMeta{
			{Key: "Seleccionar una Fecha", Value: date},
			{Key: "desayuno_excel", Value: products},
		},
	})
	return engine.ParseWideOrderRows([]map[string]any{row}, engine.WideExportOptions{}).Lines
}

// TestLookupNotifiesMissedWebhooks is the fallback rule: what the fast lookup
// finds (a new order, a new pending order and a cancelled one) produces the
// same notices a webhook would, and a second run with no changes stays silent.
func TestLookupNotifiesMissedWebhooks(t *testing.T) {
	c := testClient(t)
	var kinds []string
	c.cfg.Dispatcher = func(_ string, kind, _ string) int {
		kinds = append(kinds, kind)
		return 1
	}
	date := bogotaDate(0)
	spanish := spanishDate(date)
	server := lookupServer(t, []map[string]any{
		orderJSON("10", "pending", spanish),
		orderJSON("11", "processing", spanish),
		orderJSON("12", "cancelled", spanish),
	})
	request := SyncRequest{Dates: []string{date}, APIBase: server.URL, APIKey: "ck", APISecret: "cs", CreatedDays: 45}

	// Order 12 is stored already, so its cancelled row removes it and notifies.
	c.kitchen.state.Orders[date] = map[string][]engine.ParsedOrderLine{
		"12": storedLines(t, "12", spanish, "Box Hombre X 1"),
	}

	first, err := c.SyncStore(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Added != 2 || first.Removed != 1 || first.Kept != 2 {
		t.Fatalf("first = %+v", first)
	}
	count := map[string]int{}
	for _, kind := range kinds {
		count[kind]++
	}
	if count[eventNew] != 2 || count[eventUpdate] != 1 || len(kinds) != 3 {
		t.Fatalf("notices = %v", kinds)
	}
	day := c.kitchen.state.Orders[date]
	if _, ok := day["10"]; !ok {
		t.Fatal("the pending order was not cooked")
	}
	if _, ok := day["11"]; !ok {
		t.Fatal("the processing order is missing")
	}
	if _, ok := day["12"]; ok {
		t.Fatal("the cancelled order stayed in the day")
	}

	kinds = nil
	second, err := c.SyncStore(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Removed != 0 || len(kinds) != 0 {
		t.Fatalf("second run notified %v: %+v", kinds, second)
	}
}
