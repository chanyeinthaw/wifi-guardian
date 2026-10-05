package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func fallbackController(t *testing.T) (*controller, *fakeManager, *time.Time) {
	c, m, clock := testController(t)
	m.current = snapshot{UUID: "fallback", SSID: "home-G", Connected: true}
	m.activation = func(n network) error {
		m.current = snapshot{UUID: n.UUID, SSID: n.SSID, Connected: true}
		return nil
	}
	return c, m, clock
}

func TestReturnToPrimaryWhenItAppearsAfterFallback(t *testing.T) {
	c, m, clock := fallbackController(t)
	stepOK(t, c)
	if len(m.attempts) != 0 {
		t.Fatal("attempted an invisible network")
	}
	m.visibleSSIDs = []string{"home-G", "home-A"}
	*clock = clock.Add(30 * time.Second)
	stepOK(t, c)
	if len(m.attempts) != 0 {
		t.Fatal("scanned before the configured interval")
	}
	*clock = clock.Add(30 * time.Second)
	stepOK(t, c)
	if !reflect.DeepEqual(m.attempts, []string{"home-A"}) || c.state.Connection.UUID != "primary" {
		t.Fatalf("did not return to primary: %v / %+v", m.attempts, c.state)
	}
	stepOK(t, c)
	if m.scans != 2 || len(m.attempts) != 1 || len(m.radios) != 0 {
		t.Fatal("continued promotion attempts on the primary")
	}
}

func TestBrokenPrimaryRestoresFallbackAndWaitsBeforeRetry(t *testing.T) {
	for _, activationFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "unhealthy", true: "activation_failure"}[activationFails], func(t *testing.T) {
			c, m, clock := fallbackController(t)
			c.state.Resets = 2
			m.visibleSSIDs = []string{"home-A"}
			m.activation = func(n network) error {
				if activationFails && n.UUID == "primary" {
					m.current = snapshot{}
					return errors.New("association failed")
				}
				m.current = snapshot{UUID: n.UUID, SSID: n.SSID, Connected: true}
				return nil
			}
			c.probe = func(_ context.Context, _ config, s snapshot) (health, error) {
				return health{Local: s.Connected, Internet: s.UUID == "fallback"}, nil
			}
			stepOK(t, c)
			if !reflect.DeepEqual(m.attempts, []string{"home-A", "home-G"}) || c.state.Status != "healthy" || c.state.Resets != 2 || len(m.radios) != 0 {
				t.Fatalf("unsafe promotion: %v / %+v", m.attempts, c.state)
			}
			*clock = clock.Add(time.Minute)
			stepOK(t, c)
			if len(m.attempts) != 2 {
				t.Fatal("retried a broken primary before the cooldown")
			}
			*clock = clock.Add(4 * time.Minute)
			stepOK(t, c)
			if !reflect.DeepEqual(m.attempts, []string{"home-A", "home-G", "home-A", "home-G"}) {
				t.Fatalf("retry attempts %v", m.attempts)
			}
		})
	}
}

func TestPromotionRespectsPriorityAcrossMultipleVisibleNetworks(t *testing.T) {
	c, m, clock := fallbackController(t)
	c.config.Networks = []network{{"primary", "home-A"}, {"second", "home-G"}, {"fallback", "home-E"}}
	m.current.SSID = "home-E"
	m.visibleSSIDs = []string{"unlisted", "home-G", "home-A"}
	c.probe = func(_ context.Context, _ config, s snapshot) (health, error) {
		return health{Local: true, Internet: s.UUID != "primary"}, nil
	}
	stepOK(t, c)
	if !reflect.DeepEqual(m.attempts, []string{"home-A", "home-G"}) || c.state.Connection.UUID != "second" {
		t.Fatalf("incorrect priority: %v / %+v", m.attempts, c.state)
	}
	*clock = clock.Add(time.Minute)
	stepOK(t, c)
	if len(m.attempts) != 2 {
		t.Fatal("retried the failed primary before cooldown after finding a better fallback")
	}
}

func TestFailedScanKeepsWorkingFallback(t *testing.T) {
	c, m, _ := fallbackController(t)
	m.visibleErr = errors.New("scan busy")
	stepOK(t, c)
	if len(m.attempts) != 0 || c.state.Status != "healthy" || len(m.radios) != 0 {
		t.Fatalf("scan error interrupted fallback: %+v", c.state)
	}
}

func TestWrongActiveProfileDoesNotCountAsSuccessfulPromotion(t *testing.T) {
	c, m, _ := fallbackController(t)
	m.visibleSSIDs = []string{"home-A"}
	m.activation = func(n network) error {
		if n.UUID == "primary" {
			m.current = snapshot{UUID: "unexpected", SSID: "home-A", Connected: true}
		} else {
			m.current = snapshot{UUID: n.UUID, SSID: n.SSID, Connected: true}
		}
		return nil
	}
	stepOK(t, c)
	if !reflect.DeepEqual(m.attempts, []string{"home-A", "home-G"}) || c.state.Connection.UUID != "fallback" {
		t.Fatalf("accepted incorrect profile: %+v", c.state)
	}
}

func TestShutdownDuringPromotionRestoresFallback(t *testing.T) {
	c, m, _ := fallbackController(t)
	m.visibleSSIDs = []string{"home-A"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.activation = func(n network) error {
		if n.UUID == "primary" {
			cancel()
			m.current = snapshot{}
			return ctx.Err()
		}
		m.current = snapshot{UUID: n.UUID, SSID: n.SSID, Connected: true}
		return nil
	}
	c.probe = func(probeCtx context.Context, _ config, s snapshot) (health, error) {
		if err := probeCtx.Err(); err != nil {
			return health{}, err
		}
		return health{Internet: s.Connected}, nil
	}
	if _, err := c.step(ctx); err != nil {
		t.Fatal(err)
	}
	if m.current.UUID != "fallback" || !reflect.DeepEqual(m.attempts, []string{"home-A", "home-G"}) || len(m.radios) != 0 {
		t.Fatalf("shutdown left the fallback disconnected: %v / %+v", m.attempts, m.current)
	}
}
