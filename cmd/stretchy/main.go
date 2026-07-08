package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ostap-mykhaylyak/stretchy/internal/config"
	"github.com/ostap-mykhaylyak/stretchy/internal/index"
	"github.com/ostap-mykhaylyak/stretchy/internal/logx"
	"github.com/ostap-mykhaylyak/stretchy/internal/server"
	"github.com/ostap-mykhaylyak/stretchy/internal/setup"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		configPath  = flag.String("config", config.DefaultPath, "path to config file")
		runInit     = flag.Bool("init", false, "install stretchy: copy binary to /sbin, create systemd unit and default config")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("stretchy %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	if *runInit {
		if err := setup.Install(version); err != nil {
			fmt.Fprintf(os.Stderr, "init failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	log, err := logx.New(cfg.Logging.Dir, cfg.Logging.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging: %v\n", err)
		os.Exit(1)
	}
	defer log.Close()

	log.Info("stretchy %s starting (commit %s)", version, commit)

	store, err := index.OpenStore(cfg.Storage.DataDir, log)
	if err != nil {
		log.Error("open store: %v", err)
		os.Exit(1)
	}

	srv := server.New(cfg, store, log, version)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info("received %s, shutting down", sig)
	case err := <-errCh:
		log.Error("server: %v", err)
		store.Close()
		os.Exit(1)
	}

	srv.Shutdown()
	store.Close()
	log.Info("stretchy stopped")
}
