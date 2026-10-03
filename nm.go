package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	nmName                      = "org.freedesktop.NetworkManager"
	nmRoot      dbus.ObjectPath = "/org/freedesktop/NetworkManager"
	deviceAPI                   = nmName + ".Device"
	wirelessAPI                 = deviceAPI + ".Wireless"
	profileAPI                  = nmName + ".Settings.Connection"
	activeAPI                   = nmName + ".Connection.Active"
)

type profile struct {
	UUID string          `json:"uuid"`
	SSID string          `json:"ssid"`
	Name string          `json:"name"`
	Path dbus.ObjectPath `json:"-"`
}

type snapshot struct {
	UUID      string `json:"uuid"`
	SSID      string `json:"ssid"`
	Connected bool   `json:"connected"`
	Gateway   string `json:"gateway,omitempty"`
}

type manager struct{ bus *dbus.Conn }

func openManager() (*manager, error) {
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	return &manager{bus: bus}, nil
}

// All calls have a deadline, including calls made outside an activation timeout.
func (m *manager) call(ctx context.Context, path dbus.ObjectPath, method string, args ...interface{}) *dbus.Call {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return m.bus.Object(nmName, path).CallWithContext(ctx, method, 0, args...)
}

func (m *manager) get(ctx context.Context, path dbus.ObjectPath, api, name string, value interface{}) error {
	var variant dbus.Variant
	if err := m.call(ctx, path, "org.freedesktop.DBus.Properties.Get", api, name).Store(&variant); err != nil {
		return err
	}
	return variant.Store(value)
}

func (m *manager) device(ctx context.Context, iface string) (dbus.ObjectPath, error) {
	var path dbus.ObjectPath
	if err := m.call(ctx, nmRoot, nmName+".GetDeviceByIpIface", iface).Store(&path); err != nil {
		return "", err
	}
	var kind uint32
	if err := m.get(ctx, path, deviceAPI, "DeviceType", &kind); err != nil {
		return "", err
	}
	if kind != 2 {
		return "", fmt.Errorf("%s is not a Wi-Fi device", iface)
	}
	return path, nil
}

func (m *manager) profile(ctx context.Context, path dbus.ObjectPath) (profile, error) {
	var settings map[string]map[string]dbus.Variant
	if err := m.call(ctx, path, profileAPI+".GetSettings").Store(&settings); err != nil {
		return profile{}, err
	}
	var p profile
	var kind string
	if err := settings["connection"]["type"].Store(&kind); err != nil {
		return p, err
	}
	if kind != "802-11-wireless" {
		return p, nil
	}
	if err := settings["connection"]["uuid"].Store(&p.UUID); err != nil {
		return p, err
	}
	if err := settings["connection"]["id"].Store(&p.Name); err != nil {
		return p, err
	}
	var ssid []byte
	if err := settings[kind]["ssid"].Store(&ssid); err != nil {
		return p, err
	}
	p.SSID, p.Path = string(ssid), path
	return p, nil
}

func (m *manager) profiles(ctx context.Context) ([]profile, error) {
	var paths []dbus.ObjectPath
	if err := m.call(ctx, nmRoot+"/Settings", nmName+".Settings.ListConnections").Store(&paths); err != nil {
		return nil, err
	}
	profiles := make([]profile, 0)
	for _, path := range paths {
		p, err := m.profile(ctx, path)
		if err != nil {
			return nil, err
		}
		if p.UUID != "" {
			profiles = append(profiles, p)
		}
	}
	return profiles, nil
}

func (m *manager) snapshot(ctx context.Context, iface string) (snapshot, error) {
	var s snapshot
	device, err := m.device(ctx, iface)
	if err != nil {
		return s, err
	}
	var state uint32
	if err := m.get(ctx, device, deviceAPI, "State", &state); err != nil {
		return s, err
	}
	if state != 100 {
		return s, nil
	}
	var active, connection, ipConfig dbus.ObjectPath
	if err := m.get(ctx, device, deviceAPI, "ActiveConnection", &active); err != nil {
		return s, err
	}
	if active == "/" {
		return s, nil
	}
	if err := m.get(ctx, active, activeAPI, "Connection", &connection); err != nil {
		return s, err
	}
	p, err := m.profile(ctx, connection)
	if err != nil {
		return s, err
	}
	// Read the actual access-point SSID, not just the profile's configured SSID.
	var ap dbus.ObjectPath
	if err := m.get(ctx, device, wirelessAPI, "ActiveAccessPoint", &ap); err != nil {
		return s, err
	}
	if ap == "/" {
		return s, nil
	}
	var ssid []byte
	if err := m.get(ctx, ap, nmName+".AccessPoint", "Ssid", &ssid); err != nil {
		return s, err
	}
	s.UUID, s.SSID, s.Connected = p.UUID, string(ssid), true
	if err := m.get(ctx, device, deviceAPI, "Ip4Config", &ipConfig); err != nil {
		return s, err
	}
	if ipConfig != "/" {
		if err := m.get(ctx, ipConfig, nmName+".IP4Config", "Gateway", &s.Gateway); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (m *manager) scan(ctx context.Context, iface string) error {
	device, err := m.device(ctx, iface)
	if err != nil {
		return err
	}
	return m.call(ctx, device, wirelessAPI+".RequestScan", map[string]dbus.Variant{}).Err
}

func (m *manager) activate(ctx context.Context, iface string, n network) error {
	device, err := m.device(ctx, iface)
	if err != nil {
		return err
	}
	profiles, err := m.profiles(ctx)
	if err != nil {
		return err
	}
	var saved dbus.ObjectPath
	for _, p := range profiles {
		if p.UUID == n.UUID && p.SSID == n.SSID {
			saved = p.Path
			break
		}
	}
	if saved == "" {
		return fmt.Errorf("no saved profile matches UUID %s and SSID %q", n.UUID, n.SSID)
	}
	var active dbus.ObjectPath
	if err := m.call(ctx, nmRoot, nmName+".ActivateConnection", saved, device, dbus.ObjectPath("/")).Store(&active); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			// Cancel unfinished activation so it cannot reconnect after the next attempt.
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			m.call(cleanup, nmRoot, nmName+".DeactivateConnection", active)
		}
	}()
	for {
		var state uint32
		if err := m.get(ctx, active, activeAPI, "State", &state); err != nil {
			return err
		}
		if state == 2 {
			success = true
			return nil
		}
		if state == 4 {
			return errors.New("NetworkManager deactivated the connection")
		}
		if err := pause(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (m *manager) radio(ctx context.Context, enabled bool) error {
	return m.call(ctx, nmRoot, "org.freedesktop.DBus.Properties.Set", nmName, "WirelessEnabled", dbus.MakeVariant(enabled)).Err
}

func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
