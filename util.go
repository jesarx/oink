package main

import (
	"embed"
	"io/fs"
	"net/http"
)

func fsSub(f embed.FS, dir string) (http.FileSystem, error) {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}

func serveStatic(fsys http.FileSystem, name, ctype, cache string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := fsys.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", cache)
		http.ServeContent(w, r, name, st.ModTime(), f)
	})
}
