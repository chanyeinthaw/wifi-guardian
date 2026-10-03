# wifi-guardian

A small Linux service that monitors Wi-Fi and tries saved networks in priority order. It uses NetworkManager's D-Bus API directly. It does not invoke networking commands or store Wi-Fi passwords.

## Recovery

1. Probe the gateway and multiple public IPv4 addresses with ICMP, binding each socket to the configured Wi-Fi interface.
2. After repeated unhealthy samples, request a scan and try the configured saved profiles in order.
3. If none work and no gateway answered, cycle the Wi-Fi radio. Permit at most three cycles during an outage.
4. Keep the radio on and retry the saved profiles every minute, even after the reset budget is exhausted.
5. Restore the reset budget after three minutes of continuously healthy samples. Stay on a working fallback until it fails.

Router recovery does not require a button press on the machine. Periodic connection attempts and probes detect when the router starts working again, even if its SSID stayed visible during the outage.

A reachable gateway suppresses radio resets for that recovery round, but the service still tries fallback profiles. This handles an upstream internet outage without repeatedly cycling the adapter. A radio reset is followed by another connection sweep on the next retry interval.

The reset counter and an interrupted radio reset are recorded atomically on disk. A service restart does not replenish the counter. After an interrupted reset, the service enables the radio before resuming monitoring. A corrupt or unreadable state file prevents monitoring from starting rather than silently granting more resets.

## Requirements

- Linux, NetworkManager, and systemd for the supplied service unit.
- Go 1.26 or newer to build.
- Saved NetworkManager profiles with credentials available without a desktop session. Use system-wide profiles for unattended operation.
- Root for the supplied service. Raw ICMP needs `CAP_NET_RAW`; root can also authorize NetworkManager operations over the system bus.
- Targets and gateway that answer ICMP. A gateway that blocks ping cannot reliably distinguish an upstream outage from a local failure.

This version checks IPv4 connectivity. At least one internet target must meet the configured packet-loss threshold. It does not measure throughput or diagnose DNS failures. Probe setup errors stop that recovery iteration instead of triggering radio resets.

## Configure

```sh
go build -o bin/wifi-guardian .
./bin/wifi-guardian -list
cp config.example.json config.local.json
```

Edit `config.local.json` with your interface and the UUID/SSID pairs from `-list`. Array order sets priority. Every entry must match an existing saved profile; the service never creates profiles or reads their secrets. `-list` and `-check` do not change network settings.

```sh
sudo ./bin/wifi-guardian -config config.local.json -check
```

`-check` prints the current connection, whether it is whitelisted, and sent/received counts for each probe. It exits successfully when configuration and probes execute; inspect `internet_healthy` to see the health result. It never updates the state file.

| Setting | Default | Meaning |
| --- | --- | --- |
| `interval` | `10s` | Delay between normal health samples |
| `failure_samples` | `3` | Consecutive unhealthy samples before recovery |
| `packets` | `3` | Echo requests per target per sample |
| `max_loss` | `0.5` | Maximum loss fraction for a healthy internet target |
| `probe_timeout` | `2s` | Timeout per echo request |
| `connect_timeout` | `30s` | Deadline for each profile activation |
| `retry_interval` | `60s` | Delay between recovery rounds |
| `wifi_off` | `3s` | Radio-off duration per reset |
| `max_resets` | `3` | Reset budget, configurable from zero to three |
| `stable_for` | `3m` | Healthy sampling period before clearing the counter |
| `state_file` | `/var/lib/wifi-guardian/state.json` | Persistent reset budget and latest status |

Probe targets run concurrently; requests to each target run sequentially. Probe and activation time adds to the configured delays. A dropped packet does not immediately trigger a switch.

## Install

Installing and starting the service authorizes it to switch the configured Wi-Fi device between profiles and cycle the Wi-Fi radio. NetworkManager's radio toggle affects **all Wi-Fi devices** on the host.

```sh
sudo install -Dm755 bin/wifi-guardian /usr/local/bin/wifi-guardian
sudo install -Dm600 config.local.json /etc/wifi-guardian/config.json
sudo install -Dm644 wifi-guardian.service /etc/systemd/system/wifi-guardian.service
sudo systemctl daemon-reload
sudo systemctl enable --now wifi-guardian.service
```

The service starts without waiting for internet access. systemd restarts it after a crash. Its state directory remains writable under the supplied sandbox; configure a different state path only if you also adjust the unit's writable paths.

NetworkManager remains responsible for networking. Its own autoconnect behavior can affect which profile is active between recovery rounds. For predictable operation, configure autoconnect priorities consistently and disable autoconnect on unwanted profiles through your normal NetworkManager settings. The guardian only requests activation of listed UUID/SSID pairs.

## Status

```sh
journalctl -u wifi-guardian -f
sudo cat /var/lib/wifi-guardian/state.json
sudo systemctl stop wifi-guardian.service
```

The service logs status transitions, failed connection attempts, and radio resets. `waiting_for_router` and a counter at the configured maximum indicate that radio resets are exhausted; connection attempts continue. `waiting_for_internet` means a gateway answered but all internet probes failed.

There is no remote alert transport. During a complete outage, the journal and state file remain available locally; inspect them after reconnecting.

## Development

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o bin/wifi-guardian .
```

Recovery tests use a fake NetworkManager and do not change the host's networking.
