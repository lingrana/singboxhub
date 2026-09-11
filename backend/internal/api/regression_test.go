package api

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"testing"
)

func responseMediaType(t *testing.T, res *http.Response) string {
	t.Helper()
	mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	return mediaType
}

func rawAPI(t *testing.T, e *testEnv, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, raw
}

func TestBrandingBinaryRoundTripAndConditionalRequests(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	for _, field := range []string{"logo", "favicon"} {
		for _, format := range []string{"png", "jpeg", "gif"} {
			t.Run(field+"-"+format, func(t *testing.T) {
				var data bytes.Buffer
				img := image.NewRGBA(image.Rect(0, 0, 2, 2))
				var err error
				switch format {
				case "png":
					err = png.Encode(&data, img)
				case "jpeg":
					err = jpeg.Encode(&data, img, nil)
				case "gif":
					err = gif.Encode(&data, img, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				path := "/settings/" + field
				headers := e.authedHeaders(t)
				headers["Content-Type"] = "image/" + format
				res, _ := rawAPI(t, e, "POST", path, data.Bytes(), headers)
				if res.StatusCode != 200 {
					t.Fatalf("upload=%d", res.StatusCode)
				}
				res, raw := rawAPI(t, e, "GET", path, nil, nil)
				if res.StatusCode != 200 || !bytes.Equal(raw, data.Bytes()) || res.Header.Get("Content-Type") != "image/"+format {
					t.Fatalf("image round trip: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
				}
				etag := res.Header.Get("ETag")
				res, raw = rawAPI(t, e, "HEAD", path, nil, nil)
				if res.StatusCode != 200 || len(raw) != 0 || res.ContentLength != int64(data.Len()) {
					t.Fatal("HEAD metadata mismatch")
				}
				res, raw = rawAPI(t, e, "GET", path, nil, map[string]string{"If-None-Match": etag})
				if res.StatusCode != 304 || len(raw) != 0 {
					t.Fatal("conditional GET failed")
				}
				res, raw = rawAPI(t, e, "GET", path, nil, map[string]string{"Range": "bytes=0-7"})
				if res.StatusCode != 206 || !bytes.Equal(raw, data.Bytes()[:8]) {
					t.Fatal("range GET failed")
				}
				res, _ = rawAPI(t, e, "GET", path, nil, map[string]string{"Range": "bytes=999999-"})
				if res.StatusCode != 416 || responseMediaType(t, res) != "application/problem+json" {
					t.Fatal("invalid range response failed")
				}
				_, settings, _ := e.do(t, "GET", "/settings", nil, headers)
				if settings[field+"_url"] != path {
					t.Fatal("advertised image URL differs from route")
				}
			})
		}
	}
}

func TestBrandingValidationRejectsInvalidAndOversizedImages(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	var valid, tooWide bytes.Buffer
	if err := png.Encode(&valid, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&tooWide, image.NewRGBA(image.Rect(0, 0, 4097, 1))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, media string
		body        []byte
		status      int
	}{
		{"empty", "image/png", nil, 400},
		{"type", "text/plain", valid.Bytes(), 415},
		{"mismatch", "image/jpeg", valid.Bytes(), 415},
		{"invalid", "image/png", []byte("invalid"), 422},
		{"truncated", "image/png", valid.Bytes()[:40], 422},
		{"bytes", "image/png", make([]byte, maxBrandingBytes+1), 413},
		{"dimensions", "image/png", tooWide.Bytes(), 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := e.authedHeaders(t)
			headers["Content-Type"] = tc.media
			res, _ := rawAPI(t, e, "POST", "/settings/logo", tc.body, headers)
			if res.StatusCode != tc.status || responseMediaType(t, res) != "application/problem+json" {
				t.Fatalf("upload=%d want=%d", res.StatusCode, tc.status)
			}
		})
	}
}

func TestConcurrentSettingsRequestsRejectStaleETag(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	headers := e.authedHeaders(t)
	_, _, response := e.do(t, "GET", "/settings", nil, headers)
	results := make(chan int, 8)
	for range 8 {
		go func() {
			req, _ := http.NewRequest("PATCH", e.ts.URL+"/settings", bytes.NewBufferString(`{"retention_days":45}`))
			req.Header.Set("Authorization", headers["Authorization"])
			req.Header.Set("Content-Type", "application/merge-patch+json")
			req.Header.Set("If-Match", response.Get("ETag"))
			res, err := e.client.Do(req)
			if err != nil {
				results <- 0
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			results <- res.StatusCode
		}()
	}
	winners := 0
	for range 8 {
		status := <-results
		if status == 200 {
			winners++
		} else if status != 412 {
			t.Errorf("concurrent update=%d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("successful updates=%d", winners)
	}
}

func TestInvalidUserPatchIsAtomicAndUsersPaginate(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := e.authedHeaders(t)
	var first string
	for i := range 3 {
		status, _, hdr := e.do(t, "POST", "/users", map[string]string{"username": fmt.Sprintf("user-%d", i), "password": "test-password"}, h)
		if status != 201 {
			t.Fatalf("create=%d", status)
		}
		if i == 0 {
			first = hdr.Get("Location")
		}
	}
	status, _, _ := e.do(t, "PATCH", first, map[string]string{"username": "renamed", "password": "x"}, h)
	if status != 422 {
		t.Fatalf("invalid patch=%d", status)
	}
	status, user, _ := e.do(t, "GET", first, nil, h)
	if status != 200 || user["username"] != "user-0" || user["created_at"] == nil {
		t.Fatalf("user changed on rejection: %v", user)
	}
	status, page, _ := e.do(t, "GET", "/users?page_size=2", nil, h)
	if status != 200 || len(page["items"].([]any)) != 2 {
		t.Fatal("first page size")
	}
	cursor, ok := page["next_cursor"].(string)
	if !ok {
		t.Fatal("no next cursor")
	}
	status, page, _ = e.do(t, "GET", "/users?page_size=2&cursor="+cursor, nil, h)
	if status != 200 || len(page["items"].([]any)) != 2 || page["next_cursor"] != nil {
		t.Fatal("last page invalid")
	}
}

func TestUnknownRouteAndMethodRemainErrors(t *testing.T) {
	e := newTestEnv(t)
	res, _ := rawAPI(t, e, "GET", "/missing", nil, nil)
	if res.StatusCode != 404 || responseMediaType(t, res) != "application/problem+json" {
		t.Fatalf("unknown path=%d", res.StatusCode)
	}
	res, _ = rawAPI(t, e, "POST", "/health", nil, nil)
	if res.StatusCode != 405 || res.Header.Get("Allow") == "" || responseMediaType(t, res) != "application/problem+json" {
		t.Fatal("wrong method did not return 405 with Allow")
	}
}
