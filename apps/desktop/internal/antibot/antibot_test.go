package antibot

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestError(t *testing.T) {
	challenge := `<html><head><meta http-equiv="refresh" content="0;/.well-known/sgcaptcha/?r=%2Fwp-login.php&y=ipc:1.2.3.4:1"></head></html>`
	cases := []struct {
		name   string
		status int
		sg     string
		body   string
		want   bool
	}{
		{"sg-captcha header", http.StatusAccepted, "challenge", "", true},
		{"202 sgcaptcha redirect", http.StatusAccepted, "", challenge, true},
		{"robot challenge screen", http.StatusAccepted, "", "<title>Robot Challenge Screen</title>", true},
		{"plain 202", http.StatusAccepted, "", "<html>accepted</html>", false},
		{"login form", http.StatusOK, "", `<form><input name="pwd"></form>`, false},
		{"orders json", http.StatusOK, "", `[{"id":1}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			if tc.sg != "" {
				resp.Header.Set("SG-Captcha", tc.sg)
			}
			err := Error(resp, []byte(tc.body))
			if (err != nil) != tc.want {
				t.Fatalf("Error = %v, want challenge=%v", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "SiteGround") {
				t.Fatalf("message lacks the host: %v", err)
			}
			if err != nil {
				var challenge *ChallengeError
				if !errors.As(err, &challenge) {
					t.Fatalf("error is not a ChallengeError: %v", err)
				}
			}
		})
	}
	if err := Error(nil, nil); err != nil {
		t.Fatalf("nil response = %v", err)
	}
}
