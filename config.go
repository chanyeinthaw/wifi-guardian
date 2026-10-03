package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"
)

type duration time.Duration

func (d *duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	*d = duration(parsed)
	return nil
}

type network struct {
	UUID string `json:"uuid"`
	SSID string `json:"ssid"`
}

type config struct {
	Interface      string    `json:"interface"`
	Networks       []network `json:"networks"`
	Targets        []string  `json:"targets"`
	Interval       duration  `json:"interval"`
	RetryInterval  duration  `json:"retry_interval"`
	ConnectTimeout duration  `json:"connect_timeout"`
	ProbeTimeout   duration  `json:"probe_timeout"`
	WiFiOff        duration  `json:"wifi_off"`
	StableFor      duration  `json:"stable_for"`
	FailureSamples int       `json:"failure_samples"`
	Packets        int       `json:"packets"`
	MaxLoss        float64   `json:"max_loss"`
	MaxResets      int       `json:"max_resets"`
	StateFile      string    `json:"state_file"`
}

func loadConfig(path string) (config, error) {
	c := config{
		Targets:  []string{"1.1.1.1", "8.8.8.8"},
		Interval: duration(10 * time.Second), RetryInterval: duration(time.Minute),
		ConnectTimeout: duration(30 * time.Second), ProbeTimeout: duration(2 * time.Second),
		WiFiOff: duration(3 * time.Second), StableFor: duration(3 * time.Minute),
		FailureSamples: 3, Packets: 3, MaxLoss: 0.5, MaxResets: 3,
		StateFile: "/var/lib/wifi-guardian/state.json",
	}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return c, errors.New("config must contain exactly one JSON object")
	}
	return c, c.validate()
}

func (c config) validate() error {
	if c.Interface == "" || c.StateFile == "" || len(c.Networks) == 0 || len(c.Targets) == 0 {
		return errors.New("interface, state_file, networks, and targets are required")
	}
	seen := make(map[string]bool)
	for _, n := range c.Networks {
		if n.UUID == "" || n.SSID == "" || seen[n.UUID] {
			return errors.New("each network needs a unique saved-profile UUID and an SSID")
		}
		seen[n.UUID] = true
	}
	for _, target := range c.Targets {
		ip, err := netip.ParseAddr(target)
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return fmt.Errorf("target %q must be a public IPv4 address", target)
		}
	}
	if c.Interval <= 0 || c.RetryInterval <= 0 || c.ConnectTimeout <= 0 || c.ProbeTimeout <= 0 || c.WiFiOff <= 0 || c.StableFor <= 0 {
		return errors.New("durations must be positive")
	}
	if c.FailureSamples < 1 || c.Packets < 1 || c.Packets > 20 || c.MaxLoss < 0 || c.MaxLoss >= 1 || c.MaxResets < 0 || c.MaxResets > 3 {
		return errors.New("invalid thresholds: packets 1..20, max_loss 0..<1, max_resets 0..3, failure_samples >=1")
	}
	return nil
}

func (c config) allowed(uuid, ssid string) bool {
	for _, n := range c.Networks {
		if n.UUID == uuid && n.SSID == ssid {
			return true
		}
	}
	return false
}
