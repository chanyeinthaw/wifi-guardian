package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type networkControl interface {
	snapshot(context.Context, string) (snapshot, error)
	scan(context.Context, string) error
	activate(context.Context, string, network) error
	radio(context.Context, bool) error
}

type state struct {
	Resets     int       `json:"resets"`
	RadioOff   bool      `json:"radio_off"`
	Status     string    `json:"status"`
	Updated    time.Time `json:"updated"`
	Connection snapshot  `json:"connection"`
	Health     health    `json:"health"`
}

func loadState(path string) (state, error) {
	var s state
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	var budget struct {
		Resets *int `json:"resets"`
	}
	if err := json.Unmarshal(data, &budget); err != nil || budget.Resets == nil {
		return s, errors.New("state file is missing its reset counter")
	}
	if s.Resets < 0 || s.Resets > 3 {
		return s, errors.New("invalid persisted reset counter")
	}
	return s, nil
}

// Persist the budget before switching the radio off; replace state atomically.
func saveState(path string, s state) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type controller struct {
	config       config
	nm           networkControl
	state        state
	probe        func(context.Context, config, snapshot) (health, error)
	persist      func(string, state) error
	wait         func(context.Context, time.Duration) error
	now          func() time.Time
	failures     int
	healthySince time.Time
}

func (c *controller) observe(ctx context.Context) (snapshot, health, error) {
	s, err := c.nm.snapshot(ctx, c.config.Interface)
	if err != nil {
		return s, health{}, err
	}
	h, err := c.probe(ctx, c.config, s)
	return s, h, err
}

func (c *controller) record(status string, s snapshot, h health) error {
	changed := c.state.Status != status || c.state.Connection.UUID != s.UUID
	c.state.Status, c.state.Connection, c.state.Health, c.state.Updated = status, s, h, c.now()
	if changed {
		slog.Info("connection status", "status", status, "ssid", s.SSID, "resets", c.state.Resets)
	}
	return c.persist(c.config.StateFile, c.state)
}

func (c *controller) healthy(s snapshot, h health) error {
	c.failures = 0
	if c.healthySince.IsZero() {
		c.healthySince = c.now()
	}
	if c.state.Resets > 0 && c.now().Sub(c.healthySince) >= time.Duration(c.config.StableFor) {
		c.state.Resets = 0
		slog.Info("connection stable; reset budget restored")
	}
	return c.record("healthy", s, h)
}

func (c *controller) step(ctx context.Context) (time.Duration, error) {
	s, h, err := c.observe(ctx)
	if err != nil {
		c.healthySince = time.Time{}
		c.failures = 0
		// Unknown health neither spends the reset budget nor counts toward stability.
		return time.Duration(c.config.RetryInterval), err
	}
	if h.Internet && c.config.allowed(s.UUID, s.SSID) {
		return time.Duration(c.config.Interval), c.healthy(s, h)
	}
	c.healthySince = time.Time{}
	c.failures++
	if c.failures < c.config.FailureSamples {
		return time.Duration(c.config.Interval), c.record("suspect", s, h)
	}
	if err := c.record("recovering", s, h); err != nil {
		return 0, err
	}
	localSeen := h.Local
	if err := c.nm.scan(ctx, c.config.Interface); err != nil {
		slog.Debug("scan unavailable; trying saved profiles", "error", err)
	}
	for _, n := range c.config.Networks {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		attempt, cancel := context.WithTimeout(ctx, time.Duration(c.config.ConnectTimeout))
		err := c.nm.activate(attempt, c.config.Interface, n)
		cancel()
		if err != nil {
			slog.Warn("connection attempt failed", "ssid", n.SSID, "error", err)
			continue
		}
		s, h, err = c.observe(ctx)
		if err != nil {
			return time.Duration(c.config.RetryInterval), err
		}
		localSeen = localSeen || h.Local
		if h.Internet && c.config.allowed(s.UUID, s.SSID) {
			return time.Duration(c.config.Interval), c.healthy(s, h)
		}
	}
	if !localSeen && c.state.Resets < c.config.MaxResets {
		if err := c.resetRadio(ctx); err != nil {
			return time.Duration(c.config.RetryInterval), err
		}
	}
	status := "waiting_for_router"
	if localSeen {
		status = "waiting_for_internet"
	}
	if err := c.record(status, s, h); err != nil {
		return 0, err
	}
	return time.Duration(c.config.RetryInterval), nil
}

func (c *controller) resetRadio(ctx context.Context) error {
	c.state.Resets++
	c.state.RadioOff = true
	if err := c.record("resetting_wifi", c.state.Connection, c.state.Health); err != nil {
		return err
	}
	// Always attempt to restore the radio, even if shutdown interrupts the off interval.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.restoreRadio(cleanup); err != nil {
			slog.Error("could not restore Wi-Fi radio", "error", err)
		}
	}()
	if err := c.nm.radio(ctx, false); err != nil {
		return err
	}
	return c.wait(ctx, time.Duration(c.config.WiFiOff))
}

func (c *controller) restoreRadio(ctx context.Context) error {
	if !c.state.RadioOff {
		return nil
	}
	if err := c.nm.radio(ctx, true); err != nil {
		return fmt.Errorf("enable Wi-Fi: %w", err)
	}
	c.state.RadioOff = false
	return c.persist(c.config.StateFile, c.state)
}

func (c *controller) run(ctx context.Context) error {
	for ctx.Err() == nil {
		if c.state.RadioOff {
			if err := c.restoreRadio(ctx); err != nil {
				slog.Error("restoring interrupted radio reset", "error", err)
				if err := c.wait(ctx, time.Duration(c.config.RetryInterval)); err != nil {
					break
				}
				continue
			}
		}
		delay, err := c.step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Error("monitor iteration failed", "error", err)
			if delay == 0 {
				return err
			} // Failed persistence must stop recovery.
		}
		if err := c.wait(ctx, delay); err != nil {
			break
		}
	}
	return nil
}
