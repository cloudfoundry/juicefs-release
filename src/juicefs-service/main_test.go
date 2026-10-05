package main

import (
	"strings"
	"testing"
)

func TestMetadataURL(t *testing.T) {
	var cfg config
	cfg.Metadata.Host = "database.internal"
	cfg.Metadata.User = "juicefs"
	cfg.Metadata.Database = "juicefs"
	cfg.Metadata.Password = "never-in-url"
	for _, tc := range []struct{ kind, options, want string }{
		{"postgres", "sslmode=disable", "postgres://juicefs:@database.internal:5432/juicefs?sslmode=disable"},
		{"mysql", "parseTime=true", "mysql://juicefs:@(database.internal:3306)/juicefs?parseTime=true"},
	} {
		cfg.Metadata.Type, cfg.Metadata.Options = tc.kind, tc.options
		got, err := metadataURL(cfg)
		if err != nil || got != tc.want || strings.Contains(got, cfg.Metadata.Password) {
			t.Fatalf("URL %q error %v", got, err)
		}
	}
	cfg.Metadata.Type = "unknown"
	if _, err := metadataURL(cfg); err == nil {
		t.Fatal("unsupported metadata accepted")
	}
}
