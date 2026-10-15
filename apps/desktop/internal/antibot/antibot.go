// Package antibot recognises hosting-level bot challenges that answer HTTP
// 202 with a JavaScript captcha instead of the requested page. The store is
// on SiteGround, whose Anti-Bot AI ships one (SG-Captcha) on every site: a
// browser solves it invisibly, but an application without a JavaScript engine
// never gets the challenge cookie, so every request keeps getting the captcha
// and the session never opens. Callers detect it to explain the real cause
// instead of blaming the credentials, or to retry the step through a real
// browser that can pass the challenge.
package antibot

import (
	"fmt"
	"net/http"
	"strings"
)

// isChallenge reports whether the response is a host bot challenge: the
// SG-Captcha header, or its 202 meta refresh into /.well-known/sgcaptcha/,
// the "Robot Challenge Screen".
func isChallenge(resp *http.Response, body []byte) bool {
	if resp == nil {
		return false
	}
	if resp.Header.Get("SG-Captcha") != "" {
		return true
	}
	if resp.StatusCode != http.StatusAccepted {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "sgcaptcha") || strings.Contains(lower, "robot challenge screen")
}

// ChallengeError is the bot challenge itself. Callers use errors.As to react
// to it (e.g. retry the step through a browser session) while every other
// failure keeps its own message.
type ChallengeError struct {
	Status int
}

func (e *ChallengeError) Error() string {
	return fmt.Sprintf("el servidor de la tienda (SiteGround) pidió un captcha anti-bot en vez de responder (HTTP %d); el usuario y la contraseña no son el problema. Pide a SiteGround que excluya la IP de este equipo del sistema Anti-Bot (SG-Captcha); en el navegador la tienda abre normal", e.Status)
}

// Error returns a descriptive error when the response is a bot challenge, and
// nil otherwise, so callers keep their own message for every other case.
func Error(resp *http.Response, body []byte) error {
	if !isChallenge(resp, body) {
		return nil
	}
	return &ChallengeError{Status: resp.StatusCode}
}
