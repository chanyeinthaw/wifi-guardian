package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

type probeResult struct {
	Target   string `json:"target"`
	Sent     int    `json:"sent"`
	Received int    `json:"received"`
	Error    string `json:"error,omitempty"`
}

func (p probeResult) passes(maxLoss float64) bool {
	return p.Error == "" && p.Sent > 0 && float64(p.Sent-p.Received)/float64(p.Sent) <= maxLoss
}

type health struct {
	Local    bool          `json:"gateway_reachable"`
	Internet bool          `json:"internet_healthy"`
	Probes   []probeResult `json:"probes"`
}

var echoID atomic.Uint32

// Bind every probe to the configured Wi-Fi device, including on VPN hosts.
func ping(ctx context.Context, iface, target string, packets int, timeout time.Duration) probeResult {
	result := probeResult{Target: target}
	conn, err := net.ListenIP("ip4:icmp", &net.IPAddr{IP: net.IPv4zero})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer conn.Close()
	raw, err := conn.SyscallConn()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	var bindErr error
	if err := raw.Control(func(fd uintptr) {
		bindErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
	}); err != nil {
		result.Error = err.Error()
		return result
	}
	if bindErr != nil {
		result.Error = bindErr.Error()
		return result
	}
	// Interrupt a blocked read immediately on service shutdown.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	id := int(echoID.Add(1) & 0xffff)
	payload := []byte("wifi-guardian:" + rand.Text())
	dest := &net.IPAddr{IP: net.ParseIP(target)}
	if dest.IP == nil {
		result.Error = "invalid IPv4 target"
		return result
	}
	buffer := make([]byte, 1500)
	for seq := 1; seq <= packets; seq++ {
		if ctx.Err() != nil {
			result.Error = ctx.Err().Error()
			return result
		}
		message := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload}}
		data, err := message.Marshal(nil)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		deadline := time.Now().Add(timeout)
		if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		if err := conn.SetDeadline(deadline); err != nil {
			result.Error = err.Error()
			return result
		}
		result.Sent++
		if _, err := conn.WriteToIP(data, dest); err != nil {
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
				result.Error = err.Error()
				return result
			}
			continue
		}
		for {
			n, source, err := conn.ReadFromIP(buffer)
			if err != nil {
				var timeoutError net.Error
				if !errors.As(err, &timeoutError) || !timeoutError.Timeout() {
					result.Error = err.Error()
					return result
				}
				break
			}
			if !source.IP.Equal(dest.IP) {
				continue
			}
			reply, err := icmp.ParseMessage(1, buffer[:n])
			if err != nil || reply.Type != ipv4.ICMPTypeEchoReply {
				continue
			}
			echo, ok := reply.Body.(*icmp.Echo)
			if ok && echo.ID == id && echo.Seq == seq && string(echo.Data) == string(payload) {
				result.Received++
				break
			}
		}
	}
	return result
}

func checkHealth(ctx context.Context, c config, s snapshot) (health, error) {
	h := health{Probes: make([]probeResult, 0)}
	if !s.Connected {
		return h, nil
	}
	targets := append([]string(nil), c.Targets...)
	if s.Gateway != "" {
		targets = append(targets, s.Gateway)
	}
	results := make([]probeResult, len(targets))
	var group sync.WaitGroup
	for i, target := range targets {
		group.Add(1)
		go func() {
			defer group.Done()
			results[i] = ping(ctx, c.Interface, target, c.Packets, time.Duration(c.ProbeTimeout))
		}()
	}
	group.Wait()
	h.Probes = results
	for i, result := range results {
		// Probe setup failures must not be mistaken for a broken router.
		if result.Error != "" {
			return h, fmt.Errorf("probe %s: %s", result.Target, result.Error)
		}
		if i < len(c.Targets) && result.passes(c.MaxLoss) {
			h.Internet = true
		}
		if i == len(c.Targets) && result.Received > 0 {
			h.Local = true
		}
	}
	return h, nil
}
