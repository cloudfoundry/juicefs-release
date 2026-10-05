package main

import "testing"

func TestNormalizeAPIURL(t *testing.T) {
	cases := map[string]string{
		"api.example.test":         "https://api.example.test",
		"https://api.example.test": "https://api.example.test",
		"http://api.example.test":  "http://api.example.test",
	}
	for input, expected := range cases {
		if actual := normalizeAPIURL(input); actual != expected {
			t.Errorf("normalizeAPIURL(%q)=%q, want %q", input, actual, expected)
		}
	}
}

func TestEnvOrUsesNonEmptyEnvironmentValue(t *testing.T) {
	t.Setenv("BROKER_REGISTRATION_TEST", "environment")
	if got := envOr("BROKER_REGISTRATION_TEST", "fallback"); got != "environment" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("BROKER_REGISTRATION_TEST", "")
	if got := envOr("BROKER_REGISTRATION_TEST", "fallback"); got != "fallback" {
		t.Fatalf("got %q", got)
	}
}
