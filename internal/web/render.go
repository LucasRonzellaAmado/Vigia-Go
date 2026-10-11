package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/luquinha/vigia/internal/alert"
)

//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

type Renderer struct {
	pages  map[string]*template.Template
	logger *slog.Logger
}

func NewRenderer(fsys fs.FS, layout string, pages []string, funcs template.FuncMap, logger *slog.Logger) (*Renderer, error) {
	r := &Renderer{pages: make(map[string]*template.Template), logger: logger}
	partials, err := fs.Glob(fsys, "partials/*.html")
	if err != nil {
		return nil, err
	}
	for _, page := range pages {
		files := append([]string{layout}, partials...)
		files = append(files, page)
		t, err := template.New(path.Base(page)).Funcs(funcs).ParseFS(fsys, files...)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", page, err)
		}
		r.pages[strings.TrimSuffix(path.Base(page), ".html")] = t
	}
	return r, nil
}

func (r *Renderer) Page(w http.ResponseWriter, status int, page string, data any) {
	r.exec(w, status, page, "layout", data)
}

func (r *Renderer) Fragment(w http.ResponseWriter, status int, page, block string, data any) {
	r.exec(w, status, page, block, data)
}

func (r *Renderer) exec(w http.ResponseWriter, status int, page, block string, data any) {
	t, ok := r.pages[page]
	if !ok {
		r.logger.Error("template inexistente", "page", page)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		r.logger.Error("renderizar template", "page", page, "block", block, "error", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func Funcs(loc *time.Location) template.FuncMap {
	asTime := func(v any) (time.Time, bool) {
		switch t := v.(type) {
		case time.Time:
			return t, !t.IsZero()
		case *time.Time:
			if t == nil {
				return time.Time{}, false
			}
			return *t, !t.IsZero()
		}
		return time.Time{}, false
	}

	return template.FuncMap{
		"datetime": func(v any) string {
			t, ok := asTime(v)
			if !ok {
				return "—"
			}
			return t.In(loc).Format("02/01/2006 15:04")
		},
		"date": func(v any) string {
			t, ok := asTime(v)
			if !ok {
				return "—"
			}
			return t.In(loc).Format("02/01/2006")
		},
		"clock": func(v any) string {
			t, ok := asTime(v)
			if !ok {
				return "—"
			}
			return t.In(loc).Format("15:04:05")
		},
		"iso": func(v any) string {
			t, ok := asTime(v)
			if !ok {
				return ""
			}
			return t.UTC().Format(time.RFC3339)
		},
		"ago": func(v any) string {
			t, ok := asTime(v)
			if !ok {
				return "nunca"
			}
			d := time.Since(t)
			if d < 5*time.Second {
				return "agora"
			}
			return "há " + alert.HumanDuration(d)
		},
		"since": func(start any, end any) string {
			s, ok := asTime(start)
			if !ok {
				return "—"
			}
			e, ok := asTime(end)
			if !ok {
				e = time.Now()
			}
			return alert.HumanDuration(e.Sub(s))
		},
		"duration": alert.HumanDuration,
		"seconds": func(n int32) string {
			return alert.HumanDuration(time.Duration(n) * time.Second)
		},
		"pct": func(f float64) string {
			if math.IsNaN(f) {
				return "—"
			}
			v := math.Floor(f*10000) / 100
			s := fmt.Sprintf("%.2f%%", v)
			if v == 100 {
				s = "100%"
			}
			return strings.Replace(s, ".", ",", 1)
		},
		"ms": func(v any) string {
			switch n := v.(type) {
			case int32:
				return fmt.Sprintf("%d ms", n)
			case float64:
				return fmt.Sprintf("%.0f ms", n)
			case time.Duration:
				return fmt.Sprintf("%d ms", n.Milliseconds())
			}
			return fmt.Sprint(v)
		},
		"kindLabel": KindLabel,
		"statusLabel": func(status string, enabled bool) string {
			if !enabled {
				return "Pausado"
			}
			switch status {
			case "up":
				return "No ar"
			case "down":
				return "Fora do ar"
			}
			return "Aguardando"
		},
		"statusClass": func(status string, enabled bool) string {
			if !enabled {
				return "paused"
			}
			return status
		},
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"derefBool": func(b *bool) bool { return b != nil && *b },
		"eq64":      func(a, b int64) bool { return a == b },
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict precisa de pares chave/valor")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("chave do dict precisa ser string")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"year": func() int { return time.Now().In(loc).Year() },
	}
}

func KindLabel(kind string) string {
	switch kind {
	case "http":
		return "HTTP(S)"
	case "tcp":
		return "Porta TCP"
	case "ping":
		return "Ping"
	case "ssl":
		return "Certificado SSL"
	case "heartbeat":
		return "Heartbeat"
	}
	return kind
}

func StaticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fileServer.ServeHTTP(w, r)
	})
}

func TemplatesFS() fs.FS {
	sub, _ := fs.Sub(templatesFS, "templates")
	return sub
}
