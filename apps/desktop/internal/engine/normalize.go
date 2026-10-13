package engine

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// accentPool hands every caller its own accent transformer: a transform.Chain
// keeps state (buffers and cursors) and is not safe for concurrent use, and
// the resolver runs at once from the panel HTTP handlers (state, orders, order
// details), the store sync loop and the WhatsApp event goroutines. A single
// shared chain was a data race: the clobbered cursors made x/text panic with
// "slice bounds out of range" and killed the app when the panic happened
// outside an HTTP handler (the background sync).
var accentPool = sync.Pool{New: func() any {
	return transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
}}

// StripAccents removes diacritics (mirrors core stripAccents).
func StripAccents(text string) string {
	transformer := accentPool.Get().(transform.Transformer)
	out, _, err := transform.String(transformer, text)
	// transform.String resets the transformer on entry, so it is clean for
	// the next borrower.
	accentPool.Put(transformer)
	if err != nil {
		return text
	}
	return out
}

// NormalizeName lowercases, strips accents and collapses non alphanumerics to
// single spaces (mirrors core normalizeName).
func NormalizeName(text string) string {
	var builder strings.Builder
	pendingSpace := false
	for _, r := range strings.ToLower(StripAccents(text)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingSpace && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			pendingSpace = false
			builder.WriteRune(r)
			continue
		}
		pendingSpace = true
	}
	return builder.String()
}
