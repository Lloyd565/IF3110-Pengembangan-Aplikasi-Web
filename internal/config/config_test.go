package config

import (
	"encoding/base64"
	"testing"
)

func TestConfig(t *testing.T) {
	for _, name := range []string{"defaults", "production", "missing db", "missing key", "missing origin", "insecure origin", "bad origin", "bad key", "bad duration", "bad order", "bad env"} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{"PLATFORM_DATABASE_URL": "postgres://localhost/platform"}
			switch name {
			case "production":
				values["APP_ENV"] = "production"
				values["SESSION_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 32))
				values["APP_ORIGIN"] = "https://db.example.test"
			case "missing db":
				delete(values, "PLATFORM_DATABASE_URL")
			case "missing key":
				values["APP_ENV"] = "production"
				values["APP_ORIGIN"] = "https://db.example.test"
			case "missing origin":
				values["APP_ENV"] = "production"
			case "insecure origin":
				values["APP_ENV"] = "production"
				values["APP_ORIGIN"] = "http://db.example.test"
			case "bad origin":
				values["APP_ORIGIN"] = "https://db.example.test/path"
			case "bad key":
				values["SESSION_KEY"] = "invalid"
			case "bad duration":
				values["SESSION_IDLE_TIMEOUT"] = "later"
			case "bad order":
				values["SESSION_GRACE_PERIOD"] = "16m"
			case "bad env":
				values["APP_ENV"] = "prodution"
			}
			cfg, err := load(func(k string) string { return values[k] })
			valid := name == "defaults" || name == "production"
			if valid && (err != nil || len(cfg.SessionKey) != 32) {
				t.Fatalf("valid config: %v", err)
			}
			if !valid && err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
