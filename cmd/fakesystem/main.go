// fakesystem runs the fake openccu-lite (and its stand-in OAuth provider) for the page's
// development and the Playwright tests (scripts/dev-serve.sh); /dev/set drives what it answers.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/fakesys"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "the fake system's address")
	token := flag.String("token", "dev", "the token the fake accepts")
	flag.Parse()
	s := fakesys.New(*token)
	log.Printf("fake system on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
