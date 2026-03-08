package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loganjanssen/ccm/internal/api"
	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/control"
	"github.com/loganjanssen/ccm/internal/deploy"
	"github.com/loganjanssen/ccm/internal/inventory"
	"github.com/loganjanssen/ccm/internal/logs"
	"github.com/loganjanssen/ccm/internal/restart"
	"github.com/loganjanssen/ccm/internal/script"
	"github.com/loganjanssen/ccm/internal/sshx"
)

func main() {
	cfgPath := flag.String("config", "/etc/ccm/config.yml", "Path to CCM config file")
	listen := flag.String("listen", ":8080", "HTTP listen address")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	sshMgr, err := sshx.NewManager(cfg)
	if err != nil {
		log.Fatalf("init ssh manager: %v", err)
	}
	defer sshMgr.Close()

	inv := inventory.NewService(cfg, sshMgr, 3*time.Second)
	deployer := deploy.NewService(cfg, sshMgr)
	controller := control.NewService(cfg, sshMgr)
	logSvc := logs.NewService(cfg, sshMgr)
	restartSvc, err := restart.NewService(cfg, sshMgr)
	if err != nil {
		log.Fatalf("init restart scheduler: %v", err)
	}
	restartSvc.Start(context.Background())
	defer restartSvc.Stop()
	scriptSvc, err := script.NewService(cfg, sshMgr)
	if err != nil {
		log.Fatalf("init script scheduler: %v", err)
	}
	scriptSvc.Start(context.Background())
	defer scriptSvc.Stop()

	srv := &http.Server{
		Addr:         *listen,
		Handler:      api.NewRouter(cfg, inv, deployer, controller, logSvc, restartSvc, scriptSvc),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("ccm listening on %s", *listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
