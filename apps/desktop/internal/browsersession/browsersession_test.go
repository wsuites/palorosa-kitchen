package browsersession

import (
	"testing"
	"time"
)

func TestPageState(t *testing.T) {
	const origin = "https://tienda.example"
	cases := []struct {
		name        string
		state       pageState
		challenged  bool
		interactive bool
		settled     bool
	}{
		{
			name:    "store login form",
			state:   pageState{Href: origin + "/wp-login.php", Title: "Inicio de sesión ‹ Tienda", Body: "Usuario Contraseña Acceder"},
			settled: true,
		},
		{
			name:       "proof of work",
			state:      pageState{Href: origin + "/.well-known/sgcaptcha/?r=%2Fwp-login.php&y=ipr:1.2.3.4:1", Title: "Robot Challenge Screen"},
			challenged: true,
		},
		{
			name:        "interactive captcha",
			state:       pageState{Href: origin + "/.well-known/captcha/?y=ipc:1.2.3.4:1&r=%2Fwp-login.php", Title: "Robot Challenge Screen", Body: "Our system thinks you might be a robot! Please complete the captcha below to prove you're a human"},
			challenged:  true,
			interactive: true,
		},
		{
			name:  "blank page",
			state: pageState{Href: "about:blank"},
		},
		{
			name:  "another site",
			state: pageState{Href: "https://otro.example/", Title: "Otro"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.challenged(); got != tc.challenged {
				t.Fatalf("challenged = %v, want %v", got, tc.challenged)
			}
			if got := tc.state.interactive(); got != tc.interactive {
				t.Fatalf("interactive = %v, want %v", got, tc.interactive)
			}
			if got := tc.state.settled(origin); got != tc.settled {
				t.Fatalf("settled = %v, want %v", got, tc.settled)
			}
		})
	}
}

func TestToCookies(t *testing.T) {
	list := []cdpCookie{
		{Name: "_I_", Value: "token", Domain: ".tienda.example", Path: "/", HTTPOnly: true, Expires: -1},
		{Name: "otra", Value: "x", Domain: ".otro.example", Path: "/"},
		{Name: "exacta", Value: "y", Domain: "tienda.example", Path: "/wp-admin"},
		{Name: "", Value: "sin nombre", Domain: ".tienda.example", Path: "/"},
	}
	cookies := toCookies("tienda.example", list)
	if len(cookies) != 2 {
		t.Fatalf("cookies = %#v", cookies)
	}
	first := cookies[0]
	if first.Name != "_I_" || first.Value != "token" || first.Domain != "tienda.example" ||
		!first.HttpOnly || !first.Expires.IsZero() {
		t.Fatalf("first cookie = %#v", first)
	}
	if cookies[1].Name != "exacta" || cookies[1].Path != "/wp-admin" {
		t.Fatalf("second cookie = %#v", cookies[1])
	}
}

func TestToCookiesExpiry(t *testing.T) {
	cookies := toCookies("tienda.example", []cdpCookie{
		{Name: "session", Value: "a", Domain: "tienda.example", Expires: -1},
		{Name: "persistent", Value: "b", Domain: "tienda.example", Expires: 1800000000},
	})
	if len(cookies) != 2 {
		t.Fatalf("cookies = %#v", cookies)
	}
	if !cookies[0].Expires.IsZero() {
		t.Fatalf("session cookie expires = %v", cookies[0].Expires)
	}
	if want := time.Unix(1800000000, 0); !cookies[1].Expires.Equal(want) {
		t.Fatalf("persistent cookie expires = %v, want %v", cookies[1].Expires, want)
	}
}

func TestCookieDomainMatch(t *testing.T) {
	cases := []struct {
		host, domain string
		want         bool
	}{
		{"tienda.example", ".tienda.example", true},
		{"www.tienda.example", "tienda.example", true},
		{"otro.example", ".tienda.example", false},
		{"tienda.example.evil", ".tienda.example", false},
		{"tienda.example", "", false},
	}
	for _, tc := range cases {
		if got := cookieDomainMatch(tc.host, tc.domain); got != tc.want {
			t.Fatalf("cookieDomainMatch(%q, %q) = %v, want %v", tc.host, tc.domain, got, tc.want)
		}
	}
}
