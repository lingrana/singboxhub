package webui_test

import (
	"bytes"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func userCookies(t *testing.T, e *uiEnv) []*http.Cookie {
	t.Helper()
	res := e.postForm(t, "/login", url.Values{"username": {"admin"}, "password": {"test-password"}}, nil)
	if res.StatusCode != 303 {
		t.Fatalf("login=%d", res.StatusCode)
	}
	return res.Cookies()
}

func TestUserFormsAndCreatedTime(t *testing.T) {
	e := newUIEnv(t)
	cookies := userCookies(t, e)
	page := readBody(t, e.get(t, "/admin/users", cookies, false))
	if !strings.Contains(page, `hx-get="/admin/users/new"`) || !strings.Contains(page, `id="user-list"`) {
		t.Fatal("user page has wrong create route or no list container")
	}
	form := readBody(t, e.get(t, "/admin/users/new", cookies, true))
	if !strings.Contains(form, `hx-post="/admin/users/new"`) || strings.Contains(form, `name="api_url"`) {
		t.Fatal("create form is not a user form")
	}
	res := e.postForm(t, "/admin/users/new", url.Values{"username": {"second"}, "password": {"test-password"}}, cookies)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("HX-Trigger"), "refresh-users") || readBody(t, res) != "" {
		t.Fatal("create did not close form and refresh users")
	}
	list := readBody(t, e.get(t, "/admin/users/list", cookies, true))
	if !strings.Contains(list, "second") {
		t.Fatal("new user not shown")
	}
	res = e.postForm(t, "/admin/users/new", url.Values{"username": {"second"}, "password": {"test-password"}}, cookies)
	if body := readBody(t, res); !strings.Contains(body, `role="alert"`) || !strings.Contains(body, `value="second"`) {
		t.Fatal("duplicate user did not retain form with an error")
	}
	form = readBody(t, e.get(t, "/admin/users/2/password", cookies, true))
	if !strings.Contains(form, `hx-post="/admin/users/2/password"`) {
		t.Fatal("password form missing")
	}
}

func TestSelfPasswordResetInvalidatesCachedRefresh(t *testing.T) {
	e := newUIEnv(t)
	cookies := userCookies(t, e)
	var oldRefresh []*http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == refreshCookieName {
			oldRefresh = append(oldRefresh, cookie)
		}
	}
	rotated := e.get(t, "/admin/users", oldRefresh, false)
	if rotated.StatusCode != 200 || len(rotated.Cookies()) != 2 {
		t.Fatal("refresh did not rotate")
	}
	res := e.postForm(t, "/admin/users/1/password", url.Values{"username": {"admin"}, "password": {"updated-password"}}, rotated.Cookies())
	if res.StatusCode != 200 || res.Header.Get("HX-Redirect") != "/login" {
		t.Fatal("self reset did not redirect to login")
	}
	res = e.get(t, "/admin/users", oldRefresh, true)
	if res.Header.Get("HX-Redirect") != "/login" {
		t.Fatal("cached successor survived password reset")
	}
	res = e.get(t, "/admin/users", cookies, true)
	if res.Header.Get("HX-Redirect") != "/login" {
		t.Fatal("old cookies survived password reset")
	}
}

func TestProfileEditUpdatesUsernameAndRotatesSession(t *testing.T) {
	e := newUIEnv(t)
	cookies := userCookies(t, e)
	edit := readBody(t, e.get(t, "/user/profile/edit", cookies, true))
	if !strings.Contains(edit, `method="post" action="/user/profile/edit"`) {
		t.Fatal("profile edit form missing")
	}
	res := e.postForm(t, "/user/profile/edit", url.Values{
		"username": {"澪染管理员"}, "new_password": {"updated-password"},
	}, cookies)
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("HX-Trigger"), "profile-updated") {
		t.Fatalf("profile edit status=%d trigger=%q", res.StatusCode, res.Header.Get("HX-Trigger"))
	}
	if len(res.Cookies()) != 2 {
		t.Fatalf("profile edit did not issue a replacement session")
	}
	profile := readBody(t, e.get(t, "/user/profile", res.Cookies(), true))
	if !strings.Contains(profile, "澪染管理员") {
		t.Fatalf("profile did not render updated username: %s", profile[:min(len(profile), 300)])
	}
}

func TestSubscriptionPageRendersCardAndImportedNode(t *testing.T) {
	e := newUIEnv(t)
	cookies := userCookies(t, e)
	res := e.postForm(t, "/admin/nodes/new", url.Values{
		"name":          {"subscription-node"},
		"api_url":       {"http://127.0.0.1:1"},
		"api_secret":    {"secret"},
		"outbound_json": {`{"type":"vless","tag":"parsed-node","server":"example.com","server_port":443,"uuid":"11111111-2222-3333-4444-555555555555"}`},
		"enabled":       {"on"},
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("node create status=%d", res.StatusCode)
	}
	body := readBody(t, e.get(t, "/admin/subscription", cookies, false))
	for _, want := range []string{`id="sub-card"`, `data-group="rule"`, "subscription-node", "订阅链接"} {
		if !strings.Contains(body, want) {
			t.Fatalf("subscription page missing %q: %s", want, body[:min(len(body), 600)])
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("subscription page still contains an inline script blocked by CSP")
	}
	res = e.postForm(t, "/admin/subscription/token", nil, cookies)
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("HX-Trigger"), "ui-toast") {
		t.Fatalf("subscription token reset status=%d trigger=%q", res.StatusCode, res.Header.Get("HX-Trigger"))
	}
}

func TestMultipartBrandingPreservesBytesAndRefreshesPage(t *testing.T) {
	e := newUIEnv(t)
	cookies := userCookies(t, e)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"favicon", "logo"} {
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		part, err := mw.CreateFormFile(field, "test.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest("POST", e.ts.URL+"/admin/settings/"+field, &form)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", e.ts.URL)
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		res, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("HX-Refresh") != "true" {
			t.Fatal("upload did not request page refresh")
		}
		get := e.get(t, "/settings/"+field, nil, false)
		if got := readBody(t, get); got != data.String() {
			t.Fatal("multipart upload changed image bytes")
		}
	}
	page := readBody(t, e.get(t, "/login", nil, false))
	if !strings.Contains(page, `src="/settings/logo"`) || strings.Contains(page, "/api/settings/") {
		t.Fatal("login branding route incorrect")
	}
}
