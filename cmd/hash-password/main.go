// hash-password reads a password from stdin and prints only its Argon2id hash.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
)

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, auth.MaxPasswordBytes+3))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Tidak dapat membaca password dari stdin.")
		os.Exit(1)
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(input), "\n"), "\r")
	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Password harus berisi 1 sampai 1024 byte.")
		os.Exit(1)
	}
	fmt.Println(hash)
}
