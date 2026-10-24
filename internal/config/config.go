package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
)

type Config struct {
	DatabaseURL   string
	Address       string
	Production    bool
	PublicOrigin  string
	SessionPolicy auth.Policy
	SessionKey    []byte
}

func Load() (Config, error) { return load(os.Getenv) }

func load(env func(string) string) (Config, error) {
	c := Config{DatabaseURL: env("PLATFORM_DATABASE_URL"), Address: env("HTTP_ADDR"), SessionPolicy: auth.DefaultPolicy()}
	if c.DatabaseURL == "" {
		return c, errors.New("PLATFORM_DATABASE_URL belum diset")
	}
	if c.Address == "" {
		c.Address = ":8080"
	}
	switch env("APP_ENV") {
	case "", "development", "test":
	case "production":
		c.Production = true
	default:
		return c, errors.New("APP_ENV harus development, test, atau production")
	}
	c.PublicOrigin = env("APP_ORIGIN")
	if c.PublicOrigin != "" {
		u, err := url.Parse(c.PublicOrigin)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "http" && u.Scheme != "https") || (c.Production && u.Scheme != "https") {
			return c, errors.New("APP_ORIGIN harus origin http(s) tanpa path; production wajib https")
		}
	}
	if c.Production && c.PublicOrigin == "" {
		return c, errors.New("APP_ORIGIN HTTPS wajib diisi pada production")
	}
	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{
		{"SESSION_IDLE_TIMEOUT", &c.SessionPolicy.IdleTimeout},
		{"SESSION_ABSOLUTE_TTL", &c.SessionPolicy.AbsoluteTTL},
		{"SESSION_ROTATION_INTERVAL", &c.SessionPolicy.RotationInterval},
		{"SESSION_GRACE_PERIOD", &c.SessionPolicy.GracePeriod},
	} {
		if value := env(setting.name); value != "" {
			d, err := time.ParseDuration(value)
			if err != nil {
				return c, fmt.Errorf("%s harus berupa durasi Go yang valid", setting.name)
			}
			*setting.target = d
		}
	}
	if err := c.SessionPolicy.Validate(); err != nil {
		return c, err
	}
	if value := env("SESSION_KEY"); value != "" {
		key, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(key) != 32 {
			return c, errors.New("SESSION_KEY harus base64 dari tepat 32 byte")
		}
		c.SessionKey = key
	} else {
		if c.Production {
			return c, errors.New("SESSION_KEY wajib diisi pada production")
		}
		c.SessionKey = make([]byte, 32)
		if _, err := rand.Read(c.SessionKey); err != nil {
			return c, err
		}
	}
	return c, nil
}
