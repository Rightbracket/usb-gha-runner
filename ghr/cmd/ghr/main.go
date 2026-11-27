// Command ghr is the single binary for the USB GitHub Actions runner image.
//
//	ghr agent              root daemon: job supervisor + fixed-action socket
//	ghr web                unprivileged HTTPS config/status page
//	ghr import             read ghr/ on the GHR-BOOT drive (boot time, root)
//	ghr prune [--if-over N] clear builder caches when /data is over N% full
//	ghr version
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/agent"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/auth"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/importer"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/prune"
	"github.com/Rightbracket/usb-gha-runner/ghr/internal/web"
)

func main() {
	log.SetFlags(0) // journald adds timestamps
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "agent":
		err = agent.Run(ctx)
	case "web":
		err = runWeb(ctx, os.Args[2:])
	case "import":
		err = importer.Run(importer.DefaultPaths())
	case "prune":
		fs := flag.NewFlagSet("prune", flag.ExitOnError)
		over := fs.Float64("if-over", 0, "only prune when /data is at least this % full (0 = always)")
		_ = fs.Parse(os.Args[2:])
		var r *prune.Result
		r, err = prune.Run(ctx, *over)
		if errors.Is(err, prune.ErrBusy) {
			log.Printf("%v; skipping this run", err)
			err = nil
		} else if err == nil {
			if r.Skipped {
				log.Printf("/data is %.0f%% full, under %.0f%%: nothing to do", r.BeforePct, *over)
			} else {
				log.Printf("/data went from %.0f%% to %.0f%% full", r.BeforePct, r.AfterPct)
			}
		}
	case "version":
		fmt.Println(agent.Version())
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ghr agent|web|import|prune|version")
	os.Exit(2)
}

func runWeb(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	addr := fs.String("addr", ":443", "HTTPS listen address")
	httpAddr := fs.String("http", ":80", "plain HTTP listen address that redirects to HTTPS (empty to disable)")
	dir := fs.String("dir", importer.WebDir, "state directory (password, TLS certificate)")
	socket := fs.String("socket", agent.SocketPath, "ghr-agent socket")
	_ = fs.Parse(args)

	host, _ := os.Hostname()
	certFile, keyFile, fp, err := web.EnsureCert(filepath.Join(*dir, "tls"), host)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	log.Printf("certificate SHA-256 fingerprint %s", fp)
	srv, err := web.New(auth.NewStore(filepath.Join(*dir, "admin.json")), *socket, agent.Version(), host, fp)
	if err != nil {
		return err
	}
	srv.SetupLockFile = importer.PasswordErrorFile
	https := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	errc := make(chan error, 2)
	go func() { errc <- https.ListenAndServeTLS(certFile, keyFile) }()
	var plain *http.Server
	if *httpAddr != "" {
		plain = &http.Server{Addr: *httpAddr, Handler: web.RedirectHTTP(), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- plain.ListenAndServe() }()
	}
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = https.Shutdown(sctx)
		if plain != nil {
			_ = plain.Shutdown(sctx)
		}
		return nil
	case err := <-errc:
		return err
	}
}
