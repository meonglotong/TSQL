// tsqld is the TSQL database server daemon.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/meonglotong/tsql/internal/server"
	"github.com/meonglotong/tsql/internal/storage"
)

func main() {
	datadir := flag.String("datadir", "/var/lib/tsql", "data directory")
	listen := flag.String("listen", "127.0.0.1:5433", "TCP listen address (empty to disable)")
	unixsocket := flag.String("unixsocket", "", "unix socket path (empty to disable)")
	flag.Parse()

	eng, err := storage.Open(*datadir)
	if err != nil {
		log.Fatalf("tsqld: open datadir %s: %v", *datadir, err)
	}

	srv := server.New(eng)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	type entry struct {
		name string
		ln   net.Listener
	}
	var lns []entry
	if *listen != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatalf("tsqld: listen %s: %v", *listen, err)
		}
		lns = append(lns, entry{"tcp " + *listen, ln})
	}
	if *unixsocket != "" {
		if err := os.MkdirAll(filepath.Dir(*unixsocket), 0o755); err != nil {
			log.Fatalf("tsqld: socket dir: %v", err)
		}
		os.Remove(*unixsocket)
		ln, err := net.Listen("unix", *unixsocket)
		if err != nil {
			log.Fatalf("tsqld: listen unix %s: %v", *unixsocket, err)
		}
		lns = append(lns, entry{"unix " + *unixsocket, ln})
	}
	if len(lns) == 0 {
		log.Fatal("tsqld: no listener configured (use -listen or -unixsocket)")
	}
	for _, en := range lns {
		log.Printf("tsqld: listening on %s (datadir %s)", en.name, *datadir)
	}

	errc := make(chan error, len(lns))
	for _, en := range lns {
		ln := en.ln
		go func() {
			if err := srv.ListenAndServe(ctx, ln); err != nil {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Printf("tsqld: shutting down (checkpointing)")
	case err := <-errc:
		log.Printf("tsqld: listener error: %v", err)
	}
	for _, en := range lns {
		en.ln.Close()
	}
	if *unixsocket != "" {
		os.Remove(*unixsocket)
	}
	if err := eng.Close(); err != nil {
		log.Printf("tsqld: close: %v", err)
		os.Exit(1)
	}
	log.Printf("tsqld: bye")
}
