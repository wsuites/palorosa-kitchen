package panelserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// syncCooldown is the minimum gap between two syncs of the same dates. A sync
// asked for sooner returns the last result, so several open panels cannot
// hammer the store with exports.
const syncCooldown = 20 * time.Second

// DefaultSyncMinutes is the automatic sync interval when none is configured.
const DefaultSyncMinutes = 10

// DefaultFullSyncMinutes is the automatic full WP All Export interval when none
// is configured: the exact export repairs what the fast lookup cannot see
// (orders booked more than the creation window ago) and drops the ones that
// disappeared.
const DefaultFullSyncMinutes = 180

// orderSyncer serialises store syncs from the button and the background loop.
type orderSyncer struct {
	// run syncs the dates; full forces the exact WP All Export even for the
	// days that already have one.
	run func(context.Context, []string, bool) (any, error)

	mu        sync.Mutex
	running   bool
	last      time.Time
	lastFull  time.Time
	lastDates string
	result    any
	err       string
}

// maxSyncDates caps one explicit sync, the same size as the several-days
// range in the panel. Each day runs its own export, one after another.
const maxSyncDates = 14

// syncDates are the delivery days a sync covers: today and tomorrow in
// Bogotá, plus the day the operator is looking at.
func syncDates(extra string) []string {
	loc := time.FixedZone("America/Bogota", -5*60*60)
	now := time.Now().In(loc)
	dates := []string{now.Format("2006-01-02"), now.AddDate(0, 0, 1).Format("2006-01-02")}
	if extra != "" && extra != dates[0] && extra != dates[1] {
		if _, err := time.Parse("2006-01-02", extra); err == nil {
			dates = append(dates, extra)
		}
	}
	return dates
}

// requestedDates resolves what one sync covers. An explicit list (the
// several-days range: each day is exported on its own) is validated,
// deduplicated, sorted and capped; without it, today, tomorrow and the shown
// day.
func requestedDates(extra string, dates []string) []string {
	out := make([]string, 0, len(dates))
	seen := map[string]bool{}
	for _, value := range dates {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		if _, err := time.Parse("2006-01-02", value); err != nil {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) >= maxSyncDates {
			break
		}
	}
	if len(out) > 0 {
		sort.Strings(out)
		return out
	}
	return syncDates(extra)
}

type syncStatus struct {
	OK       bool   `json:"ok"`
	Enabled  bool   `json:"enabled"`
	Running  bool   `json:"running"`
	Interval int    `json:"intervalMinutes"`
	At       string `json:"at,omitempty"`
	Result   any    `json:"result,omitempty"`
	Error    string `json:"error,omitempty"`
}

// sync runs one store sync unless one ran within the cooldown or is running.
// It reports whether a new sync actually ran. A forced full export ignores the
// cooldown: the operator asked for it.
func (o *orderSyncer) sync(ctx context.Context, dates []string, full bool) bool {
	key := strings.Join(dates, ",")
	o.mu.Lock()
	if o.running || (!full && !o.last.IsZero() && key == o.lastDates && time.Since(o.last) < syncCooldown) {
		o.mu.Unlock()
		return false
	}
	o.running = true
	o.mu.Unlock()

	result, err := o.run(ctx, dates, full)

	o.mu.Lock()
	defer o.mu.Unlock()
	o.running = false
	o.last = time.Now()
	if full {
		o.lastFull = o.last
	}
	o.lastDates = key
	if result != nil {
		o.result = result
	}
	if err != nil {
		o.err = err.Error()
		return true
	}
	o.err = ""
	o.result = result
	return true
}

// due reports whether the background loop should sync now, and whether that
// sync must be the periodic full export. The lookup interval drives the fast
// refresh; the full interval forces the exact WP All Export on its own clock.
func (o *orderSyncer) due(interval, fullInterval int, now time.Time) (run, full bool) {
	if interval <= 0 {
		return false, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if fullInterval > 0 && !o.lastFull.IsZero() && now.Sub(o.lastFull) >= time.Duration(fullInterval)*time.Minute {
		return true, true
	}
	if o.last.IsZero() || now.Sub(o.last) >= time.Duration(interval)*time.Minute {
		return true, false
	}
	return false, false
}

func (o *orderSyncer) status(interval int) syncStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := syncStatus{OK: true, Enabled: true, Running: o.running, Interval: interval, Result: o.result, Error: o.err}
	if !o.last.IsZero() {
		out.At = o.last.Format(time.RFC3339)
	}
	return out
}

// syncInterval is the configured automatic interval in minutes (0 = off).
func (s *Server) syncInterval() int {
	if s.cfg.SyncInterval == nil {
		return DefaultSyncMinutes
	}
	return s.cfg.SyncInterval()
}

// syncFullInterval is the configured automatic full-export interval in minutes
// (0 = off).
func (s *Server) syncFullInterval() int {
	if s.cfg.SyncFullInterval == nil {
		return DefaultFullSyncMinutes
	}
	return s.cfg.SyncFullInterval()
}

// syncLoop syncs right after start and then every configured interval. The
// interval is re-read every minute so a change in Ajustes applies live. The
// full export runs on its own slower clock, so the fast lookup only adds and
// updates and the exact export repairs the day.
func (s *Server) syncLoop(ctx context.Context) {
	if s.syncer == nil {
		return
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if run, full := s.syncer.due(s.syncInterval(), s.syncFullInterval(), time.Now()); run {
			if s.runSync(ctx, syncDates(""), full) {
				s.Publish("orders")
				s.Publish("sync")
			}
		}
		timer.Reset(time.Minute)
	}
}

// runSync runs one background sync. An unrecovered panic in this goroutine
// would kill the whole app (a fatal panic prints to a stderr the GUI build
// never shows), so it is caught, logged and the loop keeps going: a failed
// store refresh must not take the kitchen down.
func (s *Server) runSync(ctx context.Context, dates []string, full bool) (ran bool) {
	defer func() {
		if value := recover(); value != nil {
			s.log.Printf("sync: panic: %v\n%s", value, debug.Stack())
		}
	}()
	return s.syncer.sync(ctx, dates, full)
}

// handleSyncGet reports the last store sync.
func (s *Server) handleSyncGet(w http.ResponseWriter, _ *http.Request) {
	if s.syncer == nil {
		writeJSON(w, http.StatusOK, syncStatus{OK: true, Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, s.syncer.status(s.syncInterval()))
}

// handleSyncRun pulls the orders from the store now (any signed-in viewer can
// refresh; the cooldown protects the store). An optional {"date"} adds the day
// the panel is showing to today and tomorrow; an explicit {"dates": [...]}
// syncs exactly those days, which is what the several-days list sends; {"full":
// true} forces the exact WP All Export.
func (s *Server) handleSyncRun(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Date  string   `json:"date"`
		Dates []string `json:"dates"`
		Full  bool     `json:"full"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&payload)
	if s.syncer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "La sincronización con la tienda no está disponible (bot desactivado)"})
		return
	}
	// Each day is a full export; a range needs more room than the three days
	// of the default path.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if s.syncer.sync(ctx, requestedDates(strings.TrimSpace(payload.Date), payload.Dates), payload.Full) {
		s.Publish("orders")
		s.Publish("sync")
	}
	status := s.syncer.status(s.syncInterval())
	code := http.StatusOK
	if status.Error != "" {
		status.OK = false
		code = http.StatusBadGateway
	}
	writeJSON(w, code, status)
}
