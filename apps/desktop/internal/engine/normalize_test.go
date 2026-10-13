package engine

import (
	"sync"
	"testing"
)

func TestStripAccents(t *testing.T) {
	cases := map[string]string{
		"Café con leche":    "Cafe con leche",
		"Arequipe de rosas": "Arequipe de rosas",
		"Piña colada":       "Pina colada",
		"Jalapeño ácido":    "Jalapeno acido",
		"":                  "",
	}
	for in, want := range cases {
		if got := StripAccents(in); got != want {
			t.Errorf("StripAccents(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStripAccentsConcurrent guards the shared transformer: StripAccents is
// called from HTTP handlers, the background sync loop and the WhatsApp event
// goroutines at the same time, and a Transformer is not safe for concurrent
// use. Racing it corrupted the chain state and panicked with "slice bounds out
// of range" inside x/text, killing the process.
func TestStripAccentsConcurrent(t *testing.T) {
	const goroutines = 8
	const rounds = 400
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				_ = StripAccents("Café con leche y arequipe")
				_ = NormalizeName("Piña colada con jalapeño")
			}
		}()
	}
	wg.Wait()
}
