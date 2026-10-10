# tether

A Linux service that monitors Wi-Fi, recovers failed connections, and returns to higher-priority saved networks when they become available.

## Why I built it

My home server runs headlessly, and my router sometimes stops working, so I have to unplug it and plug it back in. After restarting, the router brings up 2.4 GHz before 5 GHz. The server connects to 2.4 GHz and stays there even after my preferred 5 GHz network returns. Other times, the server's Wi-Fi stops working until I turn it off and back on. I didn't want to log into the server to fix either problem every time.

I built Tether to handle those connection changes and recovery attempts automatically.

- Configure saved networks in your preferred order. Tether keeps checking for a higher-priority network even while the current connection works.
- Keep a preferred connection only after it passes connectivity checks. If it fails, Tether tries to restore the previous working network.
- Retry saved profiles after repeated connection failures and, when the gateway is unreachable, cycle the Wi-Fi radio within a configurable reset limit.
- Preserve reset counts across service restarts and restore the radio after an interrupted reset.

The 2.4 GHz and 5 GHz networks are my use case. Tether does not hardcode frequency bands or network names. You configure the saved profiles, their priorities, probe thresholds, retry timings, and reset allowance.

Tether uses NetworkManager's D-Bus API directly. It does not invoke networking commands or store Wi-Fi passwords. It controls the server's Wi-Fi connection; it cannot unplug or restart the router.

## Recovery

1. Probe the gateway and multiple public IPv4 addresses with unprivileged ICMP sockets, binding each socket to the configured Wi-Fi interface.
2. After repeated unhealthy samples, request a scan and try the configured saved profiles in order.
3. If none work and no gateway answered, cycle the Wi-Fi radio. Permit at most three cycles during an outage.
4. Keep the radio on and retry the saved profiles every minute, even after the reset budget is exhausted.
5. Restore the reset budget after three minutes of continuously healthy samples.
6. On a healthy fallback, scan once a minute for higher-priority networks and try visible profiles in configuration order. Keep a preferred connection only if its profile matches and its internet probes pass. If all preferred attempts fail, restore the previous fallback immediately. After a failed preferred attempt, wait five minutes before the next priority check. Promotion never cycles the Wi-Fi radio.

Router recovery does not require a button press on the machine. Periodic connection attempts and probes detect when the router starts working again, even if its SSID stayed visible during the outage.

A reachable gateway suppresses radio resets for that recovery round, but the service still tries fallback profiles. This handles an upstream internet outage without repeatedly cycling the adapter. A radio reset is followed by another connection sweep on the next retry interval.

The reset counter and an interrupted radio reset are recorded atomically on disk. A service restart does not replenish the counter. After an interrupted reset, the service enables the radio before resuming monitoring. A corrupt or unreadable state file prevents monitoring from starting rather than silently granting more resets.

## Requirements

- Linux, NetworkManager, and systemd for the supplied service unit.
- Go 1.26 or newer to build.
- Saved NetworkManager profiles with credentials available without a desktop session. Use system-wide profiles for unattended operation.
- Your group must be allowed by `net.ipv4.ping_group_range`.
- A user-specific Polkit rule authorizing NetworkManager connection control and Wi-Fi radio toggles.
- User lingering for unattended startup before login.
- Targets and gateway that answer ICMP. A gateway that blocks ping cannot reliably distinguish an upstream outage from a local failure.

This version checks IPv4 connectivity. At least one internet target must meet the configured packet-loss threshold. It does not measure throughput or diagnose DNS failures. Probe setup errors stop that recovery iteration instead of triggering radio resets.

## Configure

```sh
go build -o bin/tether .
./bin/tether -list
cp config.example.json config.local.json
```

Edit `config.local.json` with your interface and the UUID/SSID pairs from `-list`. Array order sets priority. Every entry must match an existing saved profile; the service never creates profiles or reads their secrets. `-list` and `-check` do not change network settings.

```sh
./bin/tether -config config.local.json -check
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
| `priority_check_interval` | `60s` | Delay between scans for higher-priority networks on a healthy fallback |
| `priority_retry_interval` | `5m` | Delay before retrying after preferred-network attempts fail |
| `wifi_off` | `3s` | Radio-off duration per reset |
| `max_resets` | `3` | Reset budget, configurable from zero to three |
| `stable_for` | `3m` | Healthy sampling period before clearing the counter |
| `state_file` | `$XDG_STATE_HOME/tether/state.json`, defaulting to `~/.local/state/tether/state.json` | Persistent reset budget and latest status |

Probe targets run concurrently; requests to each target run sequentially. Probe and activation time adds to the configured delays. A dropped packet does not immediately trigger a switch.

## Install

Installing and starting the service authorizes it to switch the configured Wi-Fi device between profiles and cycle the Wi-Fi radio. NetworkManager's radio toggle affects **all Wi-Fi devices** on the host.

Fleet manages installation, authorization, and startup when `[tether] enabled = true` is set in its machine-local `.fleetrc`. Configuration is part of Fleet's encrypted dotfiles at `dotfiles/dot-config/tether/config.json`.

For manual installation after setting up Polkit authorization and lingering:

```sh
install -Dm755 bin/tether ~/.local/bin/tether
install -Dm600 config.local.json ~/.config/tether/config.json
install -Dm644 tether.service ~/.config/systemd/user/tether.service
systemctl --user daemon-reload
systemctl --user enable --now tether.service
```

The user service starts without waiting for internet access. systemd restarts it after a crash. It uses the user's config and state directories; an omitted `state_file` selects the XDG state path.

NetworkManager remains responsible for networking. Its own autoconnect behavior can affect which profile is active between recovery rounds. For predictable operation, configure autoconnect priorities consistently and disable autoconnect on unwanted profiles through your normal NetworkManager settings. Tether only requests activation of listed UUID/SSID pairs.

## Status

```sh
journalctl --user -u tether -f
cat ~/.local/state/tether/state.json
systemctl --user stop tether.service
```

The service logs status transitions, failed connection attempts, and radio resets. `waiting_for_router` and a counter at the configured maximum indicate that radio resets are exhausted; connection attempts continue. `waiting_for_internet` means a gateway answered but all internet probes failed.

There is no remote alert transport. During a complete outage, the journal and state file remain available locally; inspect them after reconnecting.

## Development

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o bin/tether .
```

Recovery tests use a fake NetworkManager and do not change the host's networking.
