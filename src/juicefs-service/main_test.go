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

func TestStorageBackends(t *testing.T) {
	for _, kind := range []string{"webdav", "s3", "wasb"} {
		if err := validateStorage(kind, "https://container.blob.core.windows.net"); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	for _, tc := range []struct{ kind, bucket string }{
		{"azure", "https://container.blob.core.windows.net"},
		{"wasb", "file:///container"},
		{"wasb", ""},
	} {
		if err := validateStorage(tc.kind, tc.bucket); err == nil {
			t.Fatalf("invalid storage accepted: %+v", tc)
		}
	}
}

func TestManagedIdentityEnvironment(t *testing.T) {
	var cfg config
	cfg.Storage.Type, cfg.Storage.Bucket, cfg.Storage.AccessKey = "wasb", "https://container.blob.core.windows.net", "account"
	cfg.Storage.ManagedIdentity = true
	for _, clientID := range []string{"", "user-assigned-client-id"} {
		cfg.Storage.ManagedIdentityClientID = clientID
		env, err := storageEnvironment(cfg, []string{"PATH=/bin", "AZURE_CLIENT_ID=wrong-identity", "AZURE_TOKEN_CREDENTIALS=EnvironmentCredential"})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(env, "\n")
		if !strings.Contains(joined, "AZURE_TOKEN_CREDENTIALS=ManagedIdentityCredential") || strings.Contains(joined, "wrong-identity") || strings.Contains(joined, "EnvironmentCredential") || !strings.Contains(joined, "PATH=/bin") {
			t.Fatalf("unexpected environment: %v", env)
		}
		if clientID == "" && strings.Contains(joined, "AZURE_CLIENT_ID=") || clientID != "" && !strings.Contains(joined, "AZURE_CLIENT_ID="+clientID) {
			t.Fatalf("incorrect identity selection: %v", env)
		}
	}
	for _, mutate := range []func(*config){
		func(c *config) { c.Storage.Type = "s3" },
		func(c *config) { c.Storage.Bucket = "http://container.blob.core.windows.net" },
		func(c *config) { c.Storage.SecretKey = "key" },
		func(c *config) { c.Storage.SessionToken = "sas" },
		func(c *config) { c.Storage.AccessKey = "" },
		func(c *config) { c.Storage.ManagedIdentity = false },
	} {
		invalid := cfg
		mutate(&invalid)
		if _, err := storageEnvironment(invalid, nil); err == nil {
			t.Fatalf("conflicting configuration accepted: %+v", invalid.Storage)
		}
	}
	if _, err := storageEnvironment(cfg, []string{"AZURE_STORAGE_CONNECTION_STRING=secret"}); err == nil {
		t.Fatal("connection string override accepted")
	}
	var legacy config
	env, err := storageEnvironment(legacy, []string{"PATH=/bin"})
	if err != nil || len(env) != 1 || env[0] != "PATH=/bin" {
		t.Fatalf("legacy environment changed: %v, %v", env, err)
	}
}
