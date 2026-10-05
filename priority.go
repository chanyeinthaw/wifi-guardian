package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Try visible profiles above the current priority; restore the original on failure.
func (c *controller) prefer(ctx context.Context, current snapshot, currentHealth health) (snapshot, health, error) {
	index := slices.IndexFunc(c.config.Networks, func(n network) bool {
		return n.UUID == current.UUID && n.SSID == current.SSID
	})
	if index <= 0 || c.now().Before(c.nextPriorityCheck) {
		return current, currentHealth, nil
	}
	c.nextPriorityCheck = c.now().Add(time.Duration(c.config.PriorityCheckInterval))
	visible, err := c.nm.visible(ctx, c.config.Interface)
	if err != nil {
		if ctx.Err() != nil {
			return current, currentHealth, ctx.Err()
		}
		slog.Warn("preferred-network scan failed; keeping current connection", "error", err)
		return current, currentHealth, nil
	}
	attempted := false
	for _, n := range c.config.Networks[:index] {
		if !slices.Contains(visible, n.SSID) {
			continue
		}
		attempted = true
		c.healthySince = time.Time{}
		slog.Info("trying higher-priority network", "ssid", n.SSID)
		s, h, err := c.connectAndCheck(ctx, n)
		if err == nil && h.Internet {
			slog.Info("higher-priority network healthy", "ssid", n.SSID)
			return s, h, nil
		}
		slog.Warn("higher-priority network failed", "ssid", n.SSID, "error", err)
		c.nextPriorityCheck = c.now().Add(time.Duration(c.config.PriorityRetryInterval))
		if ctx.Err() != nil {
			break
		}
	}
	if !attempted {
		return current, currentHealth, nil
	}
	c.nextPriorityCheck = c.now().Add(time.Duration(c.config.PriorityRetryInterval))
	// Cancellation must not leave a failed activation in place during shutdown.
	rollback := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		rollback, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
	}
	slog.Info("restoring working fallback", "ssid", current.SSID)
	s, h, err := c.connectAndCheck(rollback, c.config.Networks[index])
	if err != nil {
		return s, h, fmt.Errorf("restore fallback: %w", err)
	}
	return s, h, nil
}

func (c *controller) connectAndCheck(ctx context.Context, n network) (snapshot, health, error) {
	attempt, cancel := context.WithTimeout(ctx, time.Duration(c.config.ConnectTimeout))
	defer cancel()
	if err := c.nm.activate(attempt, c.config.Interface, n); err != nil {
		return snapshot{}, health{}, err
	}
	s, h, err := c.observe(ctx)
	if err == nil && (s.UUID != n.UUID || s.SSID != n.SSID || !s.Connected) {
		err = fmt.Errorf("requested profile %q is not active", n.SSID)
	}
	return s, h, err
}
