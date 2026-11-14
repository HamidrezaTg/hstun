// Package decoy is what an unauthenticated visitor sees. Every request that is
// not a valid tunnel request, on any path and with any method, gets the same
// answer an ordinary small web server would give: a static site from
// `decoy_dir` (or a built-in one-page site) and a plain HTML 404. A scanner
// that guesses the tunnel path therefore learns nothing from the status code,
// the body or the headers.
package decoy

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

const notFoundPage = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>404 Not Found</title></head>
<body>
<h1>Not Found</h1>
<p>The requested URL was not found on this server.</p>
</body>
</html>
`

const indexPage = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Welcome</title></head>
<body>
<h1>Welcome</h1>
<p>This site is under construction. Please check back later.</p>
</body>
</html>
`

func setHTML(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
}

// NotFound writes the standard 404 page. It is the single answer for unknown
// paths, wrong methods and rejected tunnel requests.
func NotFound(w http.ResponseWriter, _ *http.Request) {
	setHTML(w)
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notFoundPage))
}

// Error writes an error status with a page in the same style as the 404 page, for
// the answers the http carrier gives its own client (busy, gone, bad request). A
// plain-text "gone" or "tag" would be a signature of the tool; this looks like a
// small web server's error page. The status code is still the signal the client reads.
func Error(w http.ResponseWriter, code int) {
	if code == http.StatusNotFound {
		setHTML(w)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(notFoundPage))
		return
	}
	text := http.StatusText(code)
	setHTML(w)
	w.WriteHeader(code)
	_, _ = w.Write([]byte("<!DOCTYPE html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\"><title>" + strings.TrimSpace(itoa(code)+" "+text) +
		"</title></head>\n<body>\n<h1>" + text + "</h1>\n<p>The server could not complete the request.</p>\n</body>\n</html>\n"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

// Handler serves dir (a static site) or, when dir is empty, a built-in index
// page at "/". Directory listings are never produced, dot files are hidden,
// and only GET and HEAD are served; everything else is NotFound.
func Handler(dir string) http.Handler {
	var files http.Handler
	if dir != "" {
		files = http.FileServer(noListFS{http.Dir(dir)})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			NotFound(w, r)
			return
		}
		for _, seg := range strings.Split(path.Clean("/"+r.URL.Path), "/") {
			if strings.HasPrefix(seg, ".") {
				NotFound(w, r)
				return
			}
		}
		if files != nil {
			files.ServeHTTP(&notFoundWriter{ResponseWriter: w, r: r}, r)
			return
		}
		if r.URL.Path != "/" {
			NotFound(w, r)
			return
		}
		setHTML(w)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(indexPage))
		}
	})
}

// noListFS hides directories that have no index.html, so a directory is a 404
// instead of a listing.
type noListFS struct{ fs http.FileSystem }

func (n noListFS) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.IsDir() {
		idx, err := n.fs.Open(path.Join(name, "index.html"))
		if err != nil {
			_ = f.Close()
			return nil, os.ErrNotExist
		}
		_ = idx.Close()
	}
	return f, nil
}

// notFoundWriter swaps net/http's plain-text error bodies (404, 403 and 500
// from FileServer) for our HTML 404 so no Go-style text leaks out.
type notFoundWriter struct {
	http.ResponseWriter
	r      *http.Request
	failed bool
}

func (w *notFoundWriter) WriteHeader(code int) {
	if code == http.StatusNotFound || code == http.StatusForbidden || code == http.StatusInternalServerError {
		w.failed = true
		setHTML(w.ResponseWriter)
		w.ResponseWriter.Header().Del("Content-Length")
		w.ResponseWriter.WriteHeader(http.StatusNotFound)
		_, _ = w.ResponseWriter.Write([]byte(notFoundPage))
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *notFoundWriter) Write(b []byte) (int, error) {
	if w.failed {
		return len(b), nil // discard FileServer's own error text
	}
	return w.ResponseWriter.Write(b)
}

// CheckDir reports an error unless dir is a readable directory.
func CheckDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return &fs.PathError{Op: "decoy_dir", Path: dir, Err: fs.ErrInvalid}
	}
	return nil
}
