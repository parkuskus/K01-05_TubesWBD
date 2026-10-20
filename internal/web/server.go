package web

import (
	"net/http"

	"github.com/a-h/templ"
)

func New() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", templ.Handler(home()))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))
	return mux
}
