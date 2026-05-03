// Package renderer renders HTML templates from a filesystem to an http.ResponseWriter.
//
// Templates are parsed once at construction time. Rendering goes through a
// pooled buffer so partial output never reaches the client on a template
// error — the response is either complete HTML or a 500.
package renderer

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// Renderer is a goroutine-safe HTML template renderer.
type Renderer struct {
	tmpl *template.Template
	pool sync.Pool
}

// New parses every file in fsys whose name ends in ".html" into a single
// template set. Templates can reference each other by their parsed name.
func New(fsys fs.FS) (*Renderer, error) {
	tmpl := template.New("").Option("missingkey=zero")

	walkErr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".html") {
			return nil
		}
		if _, err := tmpl.ParseFS(fsys, p); err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk template fs: %w", walkErr)
	}

	return &Renderer{
		tmpl: tmpl,
		pool: sync.Pool{
			New: func() any { return bytes.NewBuffer(make([]byte, 0, 1024)) },
		},
	}, nil
}

// RenderHTML executes the named template against data and writes the result
// to w with status 200. Rendering goes via an internal buffer so the response
// either contains the complete page or, on template error, a generic 500.
func (r *Renderer) RenderHTML(w http.ResponseWriter, name string, data any) {
	buf, ok := r.pool.Get().(*bytes.Buffer)
	if !ok {
		panic("render: pool returned non-buffer")
	}
	buf.Reset()
	defer r.pool.Put(buf)

	if err := r.tmpl.ExecuteTemplate(buf, name, data); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}
