package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type fakeManager struct {
	current     snapshot
	attempts    []string
	radios      []bool
	activation  func(network) error
	snapshotErr error
	radioErr    error
}

func (m *fakeManager) snapshot(context.Context, string) (snapshot, error) {
	return m.current, m.snapshotErr
}
func (m *fakeManager) scan(context.Context, string) error { return nil }
func (m *fakeManager) activate(_ context.Context, _ string, n network) error {
	m.attempts = append(m.attempts, n.SSID)
	if m.activation != nil {
		return m.activation(n)
	}
	return errors.New("router unavailable")
}
func (m *fakeManager) radio(_ context.Context, enabled bool) error {
	m.radios = append(m.radios, enabled)
	return m.radioErr
}

func testController(t *testing.T) (*controller, *fakeManager, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &fakeManager{}
	c := &controller{
		config: config{
			Interface: "test0", Networks: []network{{"primary", "home-A"}, {"fallback", "home-G"}},
			Interval: duration(10 * time.Second), RetryInterval: duration(time.Minute),
			ConnectTimeout: duration(time.Second), WiFiOff: duration(time.Second),
			StableFor: duration(3 * time.Minute), FailureSamples: 1, MaxResets: 3,
			StateFile: filepath.Join(t.TempDir(), "state.json"),
		},
		nm: m, persist: saveState, now: func() time.Time { return clock },
		wait: func(context.Context, time.Duration) error { return nil },
		probe: func(_ context.Context, _ config, s snapshot) (health, error) {
			return health{Local: s.Connected, Internet: s.Connected}, nil
		},
	}
	return c, m, &clock
}

func stepOK(t *testing.T, c *controller) {
	t.Helper()
	if _, err := c.step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFallbackPriorityAndStayOnWorkingFallback(t *testing.T) {
	c, m, _ := testController(t)
	m.activation = func(n network) error {
		if n.UUID == "primary" {
			return errors.New("primary broken")
		}
		m.current = snapshot{UUID: n.UUID, SSID: n.SSID, Connected: true}
		return nil
	}
	stepOK(t, c)
	stepOK(t, c)
	if !reflect.DeepEqual(m.attempts, []string{"home-A", "home-G"}) {
		t.Fatalf("attempts %v", m.attempts)
	}
	if len(m.radios) != 0 || c.state.Status != "healthy" {
		t.Fatalf("unexpected reset/status: %+v", c.state)
	}
}

func TestExhaustedBudgetSurvivesRestartAndRouterRecovery(t *testing.T) {
	c, m, clock := testController(t)
	for range 3 {
		stepOK(t, c)
	}
	saved, err := loadState(c.config.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Resets != 3 || saved.RadioOff {
		t.Fatalf("saved state %+v", saved)
	}
	restarted := *c
	restarted.state, restarted.failures, restarted.healthySince = saved, 0, time.Time{}
	for range 4 {
		stepOK(t, &restarted)
	}
	if len(m.radios) != 6 || len(m.attempts) != 14 {
		t.Fatalf("resets/attempts %v / %v", m.radios, m.attempts)
	}
	m.activation = func(n network) error {
		m.current = snapshot{UUID: n.UUID, SSID: n.SSID, Connected: true}
		return nil
	}
	stepOK(t, &restarted)
	if restarted.state.Status != "healthy" || restarted.state.Resets != 3 {
		t.Fatalf("recovery %+v", restarted.state)
	}
	*clock = clock.Add(3 * time.Minute)
	stepOK(t, &restarted)
	if restarted.state.Resets != 0 {
		t.Fatal("budget not restored after stable recovery")
	}
}

func TestReachableGatewaySuppressesRadioReset(t *testing.T) {
	c, m, _ := testController(t)
	c.probe = func(context.Context, config, snapshot) (health, error) { return health{Local: true}, nil }
	stepOK(t, c)
	if len(m.attempts) != 2 || len(m.radios) != 0 || c.state.Status != "waiting_for_internet" {
		t.Fatalf("unexpected recovery: %+v, %v", c.state, m.radios)
	}
}

func TestFailedBudgetPersistenceNeverTurnsRadioOff(t *testing.T) {
	c, m, _ := testController(t)
	c.persist = func(_ string, s state) error {
		if s.RadioOff {
			return errors.New("disk full")
		}
		return nil
	}
	if _, err := c.step(context.Background()); err == nil {
		t.Fatal("expected persistence error")
	}
	if len(m.radios) != 0 {
		t.Fatalf("radio changed without persisted budget: %v", m.radios)
	}
}

func TestShutdownDuringResetRestoresRadio(t *testing.T) {
	c, m, _ := testController(t)
	c.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if err := c.resetRadio(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if !reflect.DeepEqual(m.radios, []bool{false, true}) || c.state.RadioOff {
		t.Fatalf("radio not restored: %v", m.radios)
	}
	saved, err := loadState(c.config.StateFile)
	if err != nil || saved.RadioOff || saved.Resets != 1 {
		t.Fatalf("saved %+v, error %v", saved, err)
	}
}

func TestInterruptedResetRestoredWithoutSpendingMoreBudget(t *testing.T) {
	c, m, _ := testController(t)
	c.state = state{Resets: 3, RadioOff: true}
	if err := c.restoreRadio(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.radios, []bool{true}) || c.state.Resets != 3 {
		t.Fatalf("unexpected state %+v / %v", c.state, m.radios)
	}
	m.radioErr = errors.New("NetworkManager unavailable")
	c.state.RadioOff = true
	if err := c.restoreRadio(context.Background()); err == nil || !c.state.RadioOff {
		t.Fatal("lost pending radio restoration")
	}
}

func TestObservationErrorsDoNotResetOrRestoreBudget(t *testing.T) {
	c, m, clock := testController(t)
	c.state.Resets = 3
	c.healthySince = clock.Add(-10 * time.Minute)
	c.probe = func(context.Context, config, snapshot) (health, error) {
		return health{}, errors.New("raw socket permission denied")
	}
	if _, err := c.step(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if len(m.radios) != 0 || len(m.attempts) != 0 || !c.healthySince.IsZero() || c.state.Resets != 3 {
		t.Fatalf("unsafe action during unknown health: %+v", c.state)
	}
}

func TestIntermittentRecoveryDoesNotReplenishBudget(t *testing.T) {
	c, m, clock := testController(t)
	c.state.Resets = 3
	m.current = snapshot{UUID: "fallback", SSID: "home-G", Connected: true}
	stepOK(t, c)
	*clock = clock.Add(2 * time.Minute)
	m.current.Connected = false
	stepOK(t, c)
	m.current.Connected = true
	*clock = clock.Add(2 * time.Minute)
	stepOK(t, c)
	if c.state.Resets != 3 {
		t.Fatal("brief recovery replenished reset budget")
	}
}

func TestCorruptStateRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, data := range []string{"{", "null", "{}", `{"resets":-1}`, `{"resets":100}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadState(path); err == nil {
			t.Fatalf("accepted corrupt state %q", data)
		}
	}
}

func TestRequireRepeatedFailureSamples(t *testing.T) {
	c, m, _ := testController(t)
	c.config.FailureSamples = 3
	stepOK(t, c)
	stepOK(t, c)
	if len(m.attempts) != 0 {
		t.Fatal("recovered before threshold")
	}
	stepOK(t, c)
	if len(m.attempts) != 2 {
		t.Fatalf("attempts %v", m.attempts)
	}
}

func TestLossThreshold(t *testing.T) {
	for _, item := range []struct {
		result probeResult
		want   bool
	}{
		{probeResult{Sent: 3, Received: 2}, true},
		{probeResult{Sent: 3, Received: 1}, false},
		{probeResult{Sent: 3, Received: 0}, false},
		{probeResult{}, false},
		{probeResult{Sent: 3, Received: 3, Error: "permission denied"}, false},
	} {
		if got := item.result.passes(0.5); got != item.want {
			t.Fatalf("%+v passes = %v", item.result, got)
		}
	}
}
