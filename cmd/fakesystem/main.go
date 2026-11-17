// fakesystem runs the fake openccu-lite (and its stand-in OAuth provider) for the page's
// development and the Playwright tests (scripts/dev-serve.sh); /dev/set drives what it answers.
// With -runner it starts the addon's own runner for a timer's "run", as the job's unit on a
// system does (`lite-sync run <job>`; SYNC_* in the environment place it).
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/fakesys"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "the fake system's address")
	token := flag.String("token", "dev", "the token the fake accepts")
	runner := flag.String("runner", "", "the lite-sync binary a timer's run starts (empty: a run does nothing)")
	readable := flag.String("readable", "", "an addon whose data a timer may read")
	flag.Parse()
	s := fakesys.New(*token)
	if *readable != "" {
		s.Readable = []string{*readable}
	}
	if *runner != "" {
		var mu sync.Mutex
		running := map[string]*exec.Cmd{}
		s.RunJob = func(name string) {
			c := exec.Command(*runner, "run", name)
			c.Stdout, c.Stderr = os.Stderr, os.Stderr
			if err := c.Start(); err != nil {
				log.Printf("runner: %v", err)
				return
			}
			mu.Lock()
			running[name] = c
			mu.Unlock()
			go func() {
				_ = c.Wait()
				mu.Lock()
				if running[name] == c {
					delete(running, name)
				}
				mu.Unlock()
			}()
		}
		s.StopJob = func(name string) {
			mu.Lock()
			defer mu.Unlock()
			if c := running[name]; c != nil && c.Process != nil {
				_ = c.Process.Signal(syscall.SIGTERM)
			}
		}
	}
	log.Printf("fake system on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
