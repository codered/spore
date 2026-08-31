package daemon

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/codered/spore/web"
)

// indexTemplate is parsed once at start. The page is mostly static — the
// live parts arrive over SSE — but it goes through html/template so anything
// interpolated into it later is escaped by construction rather than by
// remembering to.
var indexTemplate = template.Must(template.ParseFS(web.FS, "index.html"))

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// ServeMux's "/" pattern matches everything unmatched; only the root is
	// the UI, so anything else is a genuine 404 rather than a silent index.
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "no such path %s", r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "render index: %v", err)
	}
}

// handleStatic serves the embedded assets. It reads from the embed.FS by
// exact file name rather than through http.FileServer, so there is no
// directory listing and no path to traverse.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contentType := ""
	switch {
	case strings.HasSuffix(name, ".js"):
		contentType = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		contentType = "text/css; charset=utf-8"
	default:
		writeError(w, http.StatusNotFound, "no such asset %s", name)
		return
	}
	body, err := web.FS.ReadFile(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such asset %s", name)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(body)
}
