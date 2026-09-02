// Command poligon runs the phone-farm control plane: device polling, the JSON
// API and the dashboard, plus a small CLI for user management.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/api"
	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/devices"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/live"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/store"
	"github.com/pancir/poligon/internal/webui"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfgPath := envOr("POLIGON_CONFIG", "config/devices.yaml")

	switch os.Args[1] {
	case "serve":
		if err := serve(log, cfgPath); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	case "user":
		if err := userCmd(log, cfgPath, os.Args[2:]); err != nil {
			log.Error("user", "err", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func serve(log *slog.Logger, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StorageDir, 0o755); err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	mgr, err := devices.New(cfg, st, log)
	if err != nil {
		return err
	}
	res := reserve.New(st, st.DB(), cfg.IdleTimeout, cfg.MaxLease)
	inst := install.New(adb.New(cfg.ADBPath), ios.Default(), install.Options{
		BundletoolJar:   os.Getenv("POLIGON_BUNDLETOOL"),
		SigningIdentity: os.Getenv("POLIGON_SIGNING_IDENTITY"),
		ProfileDir:      envOr("POLIGON_PROFILE_DIR", "config/profiles"),
		WorkDir:         os.TempDir(),
	})
	a := auth.New(st.DB())
	lp := live.New(cfg.LiveSidecar, st, res, log)
	iosCtl := iosscreen.New(iosscreen.ParseEndpoints(cfg.IOSScreen))

	srv := api.New(cfg, st, res, inst, lp, iosCtl, http.FS(webui.FS()), log)
	handler := srv.Handler(a, os.Getenv("POLIGON_DEV_USER"))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go mgr.Run(ctx)
	go reapLoop(ctx, res, log)

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: handler}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sh)
	}()

	log.Info("poligon listening", "addr", cfg.Listen, "devices", len(cfg.Devices))
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func reapLoop(ctx context.Context, res *reserve.Manager, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := res.ReapExpired(); err != nil {
				log.Warn("reap", "err", err)
			} else if n > 0 {
				log.Info("reaped stale reservations", "count", n)
			}
		}
	}
}

func userCmd(log *slog.Logger, cfgPath string, args []string) error {
	if len(args) < 2 || args[0] != "add" {
		return errors.New("usage: poligon user add <name> [--admin]")
	}
	name := args[1]
	admin := len(args) > 2 && args[2] == "--admin"

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	tok, err := auth.New(st.DB()).CreateUser(name, admin)
	if err != nil {
		return err
	}
	fmt.Printf("user %q created%s\ntoken: %s\n", name, adminSuffix(admin), tok)
	return nil
}

func adminSuffix(a bool) string {
	if a {
		return " (admin)"
	}
	return ""
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprint(os.Stderr, `poligon — phone farm control plane

usage:
  poligon serve                     run the API + dashboard + device poller
  poligon user add <name> [--admin] create a user, print its token

env:
  POLIGON_CONFIG            config path (default config/devices.yaml)
  POLIGON_DEV_USER          bypass auth, treat every request as this user (dev only)
  POLIGON_BUNDLETOOL        path to bundletool.jar (aab installs)
  POLIGON_SIGNING_IDENTITY  codesign identity for iOS re-signing
  POLIGON_PROFILE_DIR       farm .mobileprovision dir (default config/profiles)
`)
}
