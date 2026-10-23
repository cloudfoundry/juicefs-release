package testapp

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// New creates a fresh process identity while retaining files in the mounted directory.
// The directory must already exist: silently creating it could hide a missing mount.
func New(directory, token string) (http.Handler, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || token == "" {
		return nil, fmt.Errorf("a mounted directory and test token are required")
	}
	identity := rand.Text()
	file := filepath.Join(directory, "acceptance-guid")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, identity)
	})
	mux.HandleFunc("POST /file", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128))
		if err != nil || len(body) == 0 {
			http.Error(w, "expected a nonempty GUID of at most 128 bytes", http.StatusBadRequest)
			return
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err == nil {
			_, err = f.Write(body)
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /file", func(w http.ResponseWriter, r *http.Request) {
		body, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}
