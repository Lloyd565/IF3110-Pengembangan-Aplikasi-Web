package main

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestFill(t *testing.T) {
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	env := fill(string(example), map[string]string{"SESSION_KEY": random(32), "APP_ENV": "x"})
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SESSION_KEY="); ok {
			if b, err := base64.StdEncoding.DecodeString(v); err != nil || len(b) != 32 {
				t.Fatalf("SESSION_KEY bukan base64 32 byte: %q", v)
			}
		}
	}
	if !strings.Contains(env, "APP_ENV=development") {
		t.Fatal("nilai yang sudah terisi ikut ditimpa")
	}
}
