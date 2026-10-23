package httpx

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

// devPrincipal menyuntikkan Principal palsu dari env DEV_USER_ID agar B dan C
// tidak menunggu A. Tanpa env tersebut, middleware ini tidak melakukan apa-apa.
// TODO(A): hapus file ini setelah middleware session jadi.
func devPrincipal(next http.Handler) http.Handler {
	id, err := strconv.ParseInt(os.Getenv("DEV_USER_ID"), 10, 64)
	if err != nil {
		return next
	}
	log.Printf("PERINGATAN: DEV_USER_ID=%d aktif, semua request dianggap login", id)
	p := reqctx.Principal{UserID: id, Username: "dev", IsPlatformAdmin: os.Getenv("DEV_ADMIN") == "1"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(reqctx.WithPrincipal(r.Context(), p)))
	})
}
