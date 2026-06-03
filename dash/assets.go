package dash

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
)

// Embedded assets — templates and static files are shipped inside the
// binary so `jot dash` has no external filesystem dependencies.

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static/*
var staticFS embed.FS

// templates is the parsed template set. Panel fragments (ticket_queue,
// ticket_row, health_strip) are keyed by their {{define ...}} name.
var templates *template.Template

func init() {
	var err error
	templates, err = template.New("").Funcs(tmplFuncs).ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		panic(fmt.Sprintf("dash: parse embedded templates: %v", err))
	}
}

// staticSub returns the static/ directory as an io/fs.FS for http.FS.
func staticSub() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("dash: sub static fs: %v", err))
	}
	return sub
}
