// envinit membuat .env dari .env.example dan mengisi kunci rahasia + admin pertama secara acak.
// Tidak menimpa .env yang sudah ada.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// fill mengisi variabel yang kosong di template; nilai yang sudah ada dibiarkan.
func fill(example string, values map[string]string) string {
	lines := strings.Split(example, "\n")
	for i, line := range lines {
		for key, v := range values {
			if strings.TrimSpace(line) == key+"=" {
				lines[i] = key + "=" + v
			}
		}
	}
	return strings.Join(lines, "\n")
}

func main() {
	if _, err := os.Stat(".env"); err == nil {
		fmt.Println(".env sudah ada, tidak diubah.")
		return
	}
	example, err := os.ReadFile(".env.example")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	password := strings.TrimRight(random(12), "=")
	env := fill(string(example), map[string]string{
		"SESSION_KEY":              random(32),
		"ENCRYPTION_KEY":           random(32),
		"BOOTSTRAP_ADMIN_USERNAME": "admin",
		"BOOTSTRAP_ADMIN_PASSWORD": password,
	})
	if err := os.WriteFile(".env", []byte(env), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(".env dibuat. Password admin ada di BOOTSTRAP_ADMIN_PASSWORD pada .env.")
}
