package servetest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"strings"
	"time"
)

var modtime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func isBreak(line string) bool {
	return line == "" || line == "\n"
}

func Parse(name, s string) (_ http.Handler, err error) {
	pull, stop := iter.Pull(strings.Lines(s))
	defer stop()

	var lineno int
	next := func() string {
		for {
			lineno++
			line, _ := pull()
			if strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimLeft(line, " \t")
			return line
		}
	}

	defer func() {
		if err != nil {
			err = fmt.Errorf("%s:%d: %w", name, lineno, err)
		}
	}()

	m := http.NewServeMux()
	for {
		pattern := next()
		for pattern == "\n" {
			pattern = next()
		}
		if pattern == "" {
			break
		}
		if !strings.HasPrefix(pattern, "GET ") {
			return nil, fmt.Errorf("invalid pattern: %q", pattern)
		}
		pattern = strings.TrimSpace(pattern)

		var b bytes.Buffer
		status := next()
		if strings.HasPrefix(status, "< HTTP/1.1 ") {
			b.WriteString(status[2:])
		} else {
			return nil, fmt.Errorf("invalid status line: %q", status)
		}

		header := next()
		if isBreak(header) {
			// break after status, so write end of headers newline
			b.WriteString("\n")
		} else {
			hasHeaderSep := false
			for {
				switch header {
				case "<", "<\n":
					hasHeaderSep = true
					b.WriteString("\n")
				}
				if strings.HasPrefix(header, "< ") {
					b.WriteString(header[2:])
				}
				header = next()
				if isBreak(header) {
					if !hasHeaderSep {
						b.WriteString("\n")
					}
					break
				}
			}
		}

		readResponse := func() (*http.Response, error) {
			br := bufio.NewReader(bytes.NewReader(b.Bytes()))
			res, err := http.ReadResponse(br, nil)
			if err != nil {
				err = fmt.Errorf("failed to read response: %w: %q", err, b.String())
				return nil, err
			}
			return res, nil
		}

		// fast fail if response cannot be read
		res, err := readResponse()
		if err != nil {
			return nil, err
		}
		resBodyData, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}

		// replace any sha256:? with the actual digest of the response content
		parts := strings.Split(pattern, "/")
		for i := range parts {
			if parts[i] == "sha256:?" {
				parts[i] = fmt.Sprintf("sha256:%x", sha256.Sum256(resBodyData))
			}
		}
		pattern = strings.Join(parts, "/")

		m.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			br := bufio.NewReader(bytes.NewReader(b.Bytes()))
			res, err := http.ReadResponse(br, r)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to read response: %v", err), 500)
				return
			}
			maps.Copy(w.Header(), res.Header)

			shouldSatisfyRange := true &&
				r.Header.Get("Range") != "" && // client requested a range
				res.Header.Get("Accept-Ranges") == "bytes" && // server accepts ranges
				res.StatusCode/100 == 2 // server would otherwise return some 2xx status

			if shouldSatisfyRange {
				// clone body into an io.ReadSeeker
				data, err := io.ReadAll(res.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("failed to read response body: %v", err), 500)
					return
				}
				res.Body.Close()

				// ServeContent will set the Content-Length
				// header and handle Range requests correctly
				// for us.
				http.ServeContent(w, r, "", modtime, bytes.NewReader(data))
			} else {
				if r.Header.Get("Range") != "" {
					http.Error(w, "Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
					return
				}
				w.WriteHeader(res.StatusCode)
				_, err := io.Copy(w, res.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("failed to copy response body: %v", err), 500)
					return
				}
			}
		}))
	}
	return m, nil
}
