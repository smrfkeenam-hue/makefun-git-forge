// Command forge: the git forge core, as a statically linked Linux amd64
// binary. The Node entrypoint (main.ts) spawns it with --port 0; it binds an
// ephemeral port, prints "READY <port>" on stdout, and serves until it is
// killed. All state lives in memory; the store is created once, here, and
// shared by every surface.
//
// Usage: forge [--port N]   (0, the default, picks an ephemeral port)

package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

func main() {
	port := flag.Int("port", 0, "TCP port to listen on (0 = ephemeral)")
	flag.Parse()

	listener, err := net.Listen("tcp", ":"+strconv.Itoa(*port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	bound := listener.Addr().(*net.TCPAddr).Port

	app := NewApp()
	fmt.Printf("READY %d\n", bound)

	// A clean shutdown on SIGTERM/SIGINT: close the listener and exit 0. The
	// launcher kills us when the env says done.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		listener.Close()
		os.Exit(0)
	}()

	Serve(listener, app.Handler())
}
