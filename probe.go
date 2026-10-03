package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
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

// Bind every probe to the configured Wi-Fi device, including on VPN hosts.
func ping(ctx context.Context, iface, target string, packets int, timeout time.Duration) probeResult {
	result := probeResult{Target: target}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	file := os.NewFile(uintptr(fd), "icmp")
	defer file.Close()
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface); err != nil {
		result.Error = err.Error()
		return result
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{}); err != nil {
		result.Error = err.Error()
		return result
	}
	address, err := unix.Getsockname(fd)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	local, ok := address.(*unix.SockaddrInet4)
	if !ok {
		result.Error = "unexpected ICMP socket address"
		return result
	}
	id := local.Port // Linux uses the ping socket's port as the echo identifier.
	conn, err := net.FilePacketConn(file)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer conn.Close()
	// Interrupt a blocked read immediately on service shutdown.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	payload := []byte("tether:" + rand.Text())
	dest := &net.UDPAddr{IP: net.ParseIP(target)}
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
		if _, err := conn.WriteTo(data, dest); err != nil {
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
				result.Error = err.Error()
				return result
			}
			continue
		}
		for {
			n, source, err := conn.ReadFrom(buffer)
			if err != nil {
				var timeoutError net.Error
				if !errors.As(err, &timeoutError) || !timeoutError.Timeout() {
					result.Error = err.Error()
					return result
				}
				break
			}
			sourceIP, ok := source.(*net.UDPAddr)
			if !ok || !sourceIP.IP.Equal(dest.IP) {
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
