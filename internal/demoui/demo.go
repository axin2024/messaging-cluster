package demoui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

func Attach(api http.Handler) http.Handler {
	files, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	static := http.StripPrefix("/demo", http.FileServerFS(files))
	routes := http.NewServeMux()
	routes.HandleFunc("GET /demo/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self' ws: wss:; frame-ancestors 'none'; form-action 'none'; base-uri 'none'")
		static.ServeHTTP(w, r)
	})
	routes.Handle("/", api)
	return routes
}
