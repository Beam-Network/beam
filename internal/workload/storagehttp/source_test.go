package storagehttp

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestHuggingFaceRedirectPreservesFrozenRange(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		header, status := http.Header{}, 206
		if calls == 1 {
			status = 302
			header.Set("Location", "https://us.aws.cdn.hf.co/object?opaque=route")
		} else {
			if r.Header.Get("Range") != "bytes=0-3" || r.Header.Get("If-Match") != `"frozen"` {
				t.Fatal("frozen range lost")
			}
			for _, key := range []string{"Authorization", "Cookie", "Referer", "X-Beam-Path-Token"} {
				if r.Header.Get(key) != "" {
					t.Fatalf("secret header forwarded: %s", key)
				}
			}
			header.Set("Content-Range", "bytes 0-3/4")
			header.Set("ETag", `"frozen"`)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("data")), Request: r}, nil
	})}
	request, _ := http.NewRequest("GET", "https://s3.hf.co/team/key?signature=opaque", nil)
	request.Header.Set("Range", "bytes=0-3")
	request.Header.Set("If-Match", `"frozen"`)
	for _, key := range []string{"Authorization", "Cookie", "Referer", "X-Beam-Path-Token"} {
		request.Header.Set(key, "private")
	}
	response, err := Get(client, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls != 2 || response.StatusCode != 206 || response.Header.Get("ETag") != `"frozen"` {
		t.Fatal("provider range not returned")
	}
}

func TestRejectsUnsafeOrExcessiveRedirects(t *testing.T) {
	for _, target := range []string{"http://us.aws.cdn.hf.co/object", "https://us.aws.cdn.hf.co.attacker.example/key", "https://user:secret@us.aws.cdn.hf.co/key", "https://us.aws.cdn.hf.co:9443/key", "https://us.aws.cdn.hf.co/key#fragment", "https://us.aws.cdn.hf.co/loop"} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			request, _ := http.NewRequest("GET", "https://s3.hf.co/team/key", nil)
			if _, err := Get(client, request); err == nil {
				t.Fatal("unsafe redirect accepted")
			}
			if calls > 6 {
				t.Fatal("unbounded redirects")
			}
		})
	}
}
