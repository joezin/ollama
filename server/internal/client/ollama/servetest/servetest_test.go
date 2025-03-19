package servetest

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

const test = `
# This comment and empty line above should be ignored.

GET /foo/bar
< HTTP/1.1 200 OK

GET /foo/bar/baz
< HTTP/1.1 301 Moved Permanently
< Location: /foo/bar

GET /range
< HTTP/1.1 200 OK
< Content-Type: text/plain
< Accept-Ranges: bytes
<
< Hello, world!

GET /blobs/sha256:?
< HTTP/1.1 200 OK
< Content-Type: text/plain
<
< test

GET /indent
	< HTTP/1.1 200 OK

# Another comment with some empty lines above.
`

var testDigest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("test\n")))

func TestHandler(t *testing.T) {
	m, err := Parse("test", test)
	if err != nil {
		t.Fatal(err)
	}

	get := func(path, rng string, code int, body string) {
		t.Helper()
		t.Logf("GET %s", path)
		req := httptest.NewRequest("GET", path, nil)
		if rng != "" {
			req.Header.Set("Range", "bytes="+rng)
		}
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		if w.Code != code {
			t.Errorf("  StatusCode = %d; want %d", w.Code, code)
		}
		if g := w.Body.String(); g != body {
			t.Errorf("\n  Body:\n\t%q\n  Want:\n\t%q", g, body)
		}
	}

	get("/foo/bar", "", 200, "")
	get("/foo/bar/baz", "", 301, "")

	// request order should not matter and responses should be the same
	// upon duplicate requests
	get("/foo/bar/baz", "", 301, "")
	get("/foo/bar", "", 200, "")

	get("/range", "0-0", 206, "H")
	get("/foo/bar", "0-0", 416, "Range Not Satisfiable\n")

	get("/blobs/"+testDigest, "", 200, "test\n")

	get("/indent", "", 200, "")

	_, err = Parse("test", "GET /foo/bar\n")
	if err == nil || !strings.Contains(err.Error(), "invalid status line") {
		t.Errorf("got = %v; want error about missing response body", err)
	}
}
