package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if err := execute(); err != nil {
		slog.Error("tether", "error", err)
		os.Exit(1)
	}
}

func execute() error {
	path := flag.String("config", defaultConfigFile(), "JSON configuration file")
	list := flag.Bool("list", false, "print saved Wi-Fi profiles without changing networking")
	check := flag.Bool("check", false, "validate config and probe current connection without changing networking")
	flag.Parse()
	if flag.NArg() != 0 || (*list && *check) {
		return fmt.Errorf("use -list, -check, or neither; no positional arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	nm, err := openManager()
	if err != nil {
		return err
	}
	defer nm.bus.Close()
	if *list {
		profiles, err := nm.profiles(ctx)
		if err != nil {
			return err
		}
		return printJSON(profiles)
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	var c controller
	if !*check {
		s, err := loadState(cfg.StateFile)
		if err != nil {
			return fmt.Errorf("read reset budget: %w", err)
		}
		c = controller{config: cfg, nm: nm, state: s, probe: checkHealth, persist: saveState, wait: pause, now: time.Now}
		// Restore an interrupted reset before validating possibly changed profiles.
		if err := c.restoreRadio(ctx); err != nil {
			return err
		}
	}
	profiles, err := nm.profiles(ctx)
	if err != nil {
		return err
	}
	for _, n := range cfg.Networks {
		found := false
		for _, p := range profiles {
			if n.UUID == p.UUID && n.SSID == p.SSID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("save a system-wide NetworkManager profile for %q with UUID %s first", n.SSID, n.UUID)
		}
	}
	if _, err := nm.device(ctx, cfg.Interface); err != nil {
		return err
	}
	if *check {
		s, err := nm.snapshot(ctx, cfg.Interface)
		if err != nil {
			return err
		}
		h, err := checkHealth(ctx, cfg, s)
		if err != nil {
			return err
		}
		return printJSON(struct {
			Connection snapshot `json:"connection"`
			Health     health   `json:"health"`
			Allowed    bool     `json:"allowed"`
		}{s, h, cfg.allowed(s.UUID, s.SSID)})
	}
	return c.run(ctx)
}

func printJSON(value interface{}) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func defaultConfigFile() string {
	root, err := os.UserConfigDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(root, "tether", "config.json")
}
