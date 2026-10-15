package wpexport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoreDate(t *testing.T) {
	got, err := StoreDate("2026-10-04")
	if err != nil || got != "4 octubre, 2026" {
		t.Fatalf("StoreDate = %q, %v", got, err)
	}
	if _, err := StoreDate("04/10/2026"); err == nil {
		t.Fatal("expected an error for a non ISO date")
	}
}

func TestToken(t *testing.T) {
	// md5("key1"), first 16 hex chars (what WP All Export uses).
	if got := Token("key", "1"); len(got) != 16 || got != "c2add694bf942dc7" {
		t.Fatalf("Token = %q", got)
	}
}

func TestSetStoreDate(t *testing.T) {
	rules, _ := json.Marshal([]map[string]any{{"element": "cf_Seleccionar una Fecha", "value": "1 enero, 2020"}})
	form := `<form><input name="wp_all_export_value[1]" value="1 enero, 2020">` +
		`<input type="hidden" name="filter_rules_hierarhy" value="` + strings.ReplaceAll(string(rules), `"`, "&quot;") + `">` +
		`<input type="checkbox" name="off" value="x"><input type="checkbox" name="on" value="y" checked>` +
		`<textarea name="whereclause">AND meta.meta_value = '1 enero, 2020'</textarea>` +
		`<select name="s"><option value="a">A</option><option value="b" selected>B</option></select></form>`
	fields, found := setStoreDate(parseFields(form), "4 octubre, 2026")
	if !found {
		t.Fatal("filter rule not found")
	}
	got := map[string]string{}
	for _, f := range fields {
		got[f.name] = f.value
	}
	if got["wp_all_export_value[1]"] != "4 octubre, 2026" ||
		!strings.Contains(got["filter_rules_hierarhy"], "4 octubre, 2026") ||
		got["whereclause"] != "AND meta.meta_value = '4 octubre, 2026'" ||
		got["s"] != "b" || got["on"] != "y" {
		t.Fatalf("fields = %#v", got)
	}
	if _, ok := got["off"]; ok {
		t.Fatal("unchecked checkbox must not be sent")
	}
}

func TestReadXLSX(t *testing.T) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	write := func(name, body string) {
		w, _ := z.Create(name)
		_, _ = w.Write([]byte(body))
	}
	write("xl/workbook.xml", `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="S" sheetId="1" r:id="rId1"/></sheets></workbook>`)
	write("xl/_rels/workbook.xml.rels", `<Relationships><Relationship Id="rId1" Target="worksheets/data.xml"/></Relationships>`)
	write("xl/sharedStrings.xml", `<sst><si><t>ID orden</t></si><si><r><t>Pro</t></r><r><t>ductos</t></r></si><si><t>Waffle &amp; café</t></si></sst>`)
	write("xl/worksheets/data.xml", `<worksheet><sheetData>`+
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row>`+
		`<row r="3"><c r="A3"><v>75758</v></c><c r="C3" t="s"><v>2</v></c><c r="D3" t="inlineStr"><is><t>x</t></is></c></row>`+
		`</sheetData></worksheet>`)
	_ = z.Close()
	rows, err := ReadXLSX(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"ID orden", "", "Productos"}, {}, {"75758", "", "Waffle & café", "x"}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %#v", rows)
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Fatalf("row %d = %#v, want %#v", i, rows[i], want[i])
		}
	}
}

func TestLoginReportsBotChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("SG-Captcha", "challenge")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `<html><head><meta http-equiv="refresh" content="0;/.well-known/sgcaptcha/?r=%2Fwp-login.php&y=ipc:1.2.3.4:1"></head></html>`)
	}))
	defer srv.Close()
	client, err := New(Config{Base: srv.URL, User: "cocina", Password: "secreta", CronKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Login(context.Background())
	if err == nil {
		t.Fatal("expected a login error")
	}
	if !strings.Contains(err.Error(), "SiteGround") {
		t.Fatalf("challenge not detected: %v", err)
	}
	if strings.Contains(err.Error(), "revisa el usuario") {
		t.Fatalf("challenge explained as bad credentials: %v", err)
	}
}

func TestLoginWrongPasswordKeepsCredentialsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html><body><form><input name="pwd"></form><div id="login_error">La contraseña es incorrecta.</div></body></html>`)
	}))
	defer srv.Close()
	client, err := New(Config{Base: srv.URL, User: "cocina", Password: "mala", CronKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "revisa el usuario y la contraseña") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginRecoversThroughBrowserSession(t *testing.T) {
	var logins, sessions int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_, _ = io.WriteString(w, `<html><form><input name="pwd"></form></html>`)
			return
		}
		logins++
		if !strings.Contains(r.Header.Get("Cookie"), "_I_=") {
			w.Header().Set("SG-Captcha", "challenge")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `<html><head><meta http-equiv="refresh" content="0;/.well-known/sgcaptcha/?r=%2Fwp-login.php"></head></html>`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "wordpress_logged_in_abc", Value: "yes", Path: "/", HttpOnly: true})
		_, _ = io.WriteString(w, `<html>panel</html>`)
	}))
	defer srv.Close()

	client, err := New(Config{
		Base: srv.URL, User: "cocina", Password: "secreta", CronKey: "key",
		BrowserSession: func(context.Context) ([]*http.Cookie, error) {
			sessions++
			return []*http.Cookie{{Name: "_I_", Value: "whitelisted", Path: "/"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login with browser recovery = %v", err)
	}
	if sessions != 1 || logins != 2 {
		t.Fatalf("sessions = %d, logins = %d; want 1 and 2", sessions, logins)
	}
}

func TestLoginReportsFailedBrowserSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("SG-Captcha", "challenge")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `<html>Robot Challenge Screen</html>`)
	}))
	defer srv.Close()
	client, err := New(Config{
		Base: srv.URL, User: "cocina", Password: "secreta", CronKey: "key",
		BrowserSession: func(context.Context) ([]*http.Cookie, error) {
			return nil, errors.New("no se encontró Microsoft Edge")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SiteGround") || !strings.Contains(err.Error(), "Microsoft Edge") {
		t.Fatalf("err = %v", err)
	}
}
