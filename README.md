# ⚡ Nitro On-Demand Tunneler

### Connect first. Start infrastructure second.

**Version: v1.0.0 · Author: Antonio Vega Alvarez**  
Website: https://www.nitrobutton.com · Contact: not@nitrobutton.com · GitHub: https://github.com/tonivega  
**Lunch or dinner with me: https://comidacontoni.com**  
**Telegram for some chat fun: [@dream_katapult](https://t.me/dream_katapult)**

Every executable invocation displays this version, author, contact information, lunch or dinner invitation, and Telegram chat contact prominently on stderr, including help, wizard mode, and failed launches. Error banners also include both the invitation and Telegram chat contact. Fatal errors and operational connection/startup failures display prominent ASCII-art `ERROR` panels with the underlying diagnostic and a suggested action, even without `-verbose`. Normal shutdown is not reported as an error.

A small, dependency-free Go application that turns a TCP connection into an infrastructure wake-up signal. Keep a stable local endpoint; start a container, run your automation, or establish an SSH tunnel only when traffic arrives.

**Go standard library · Six cross-compiled executables · OpenSSH integration · Bounded startup · JSON configuration**

> **Important distinction:** Nitro is a user-space TCP gate/proxy, not a packet sniffer or kernel SYN interceptor. It must own the configured listening IP and port. It accepts the client's TCP handshake immediately, holds application traffic while startup completes, then relays bytes. It cannot capture arbitrary connections to remote IPs or take over an already-bound port. This deliberate design works without administrator privileges on Windows, macOS, and Linux.

## The idea in one diagram

```text
application → 127.0.0.1:15432 → Nitro gate
                                  │
                         backend unavailable?
                                  │
                         run command → exit 0
                                  │
                         wait for TCP readiness
                                  │
                 bidirectional relay → 127.0.0.1:25432
```

For SSH, Nitro keeps the original local listening address and creates a separate private loopback forwarding port. This avoids the classic “proxy and SSH both need the same port” collision.

## Why this project is interesting

This repository demonstrates engineering decisions that matter beyond a demo:

- **Concurrency with purpose:** simultaneous arrivals share one startup attempt per tunnel.
- **Explicit failure semantics:** nonzero command exits close waiting clients; zero exits still require TCP readiness.
- **Resource-aware operation:** bounded concurrent connections, bounded diagnostic output, startup deadlines, and a short failure cooldown.
- **Portable networking:** ordinary TCP listeners instead of privileged, platform-specific packet interception.
- **SSH integration without reimplementing SSH:** OpenSSH resolves effective configuration with `ssh -G`; Nitro isolates forwarding from the front-door listener.
- **Operational clarity:** graceful connection shutdown, strict JSON parsing, loopback-only defaults, tests, CI, cross-build scripts, and checksum manifests.

No fabricated benchmarks or production-scale claims: the code, tests, trade-offs, and limitations are the portfolio.

## Quick start

Requirements: Docker with BuildKit support (running Linux containers) for building; no host Go installation is needed. An OpenSSH client in `PATH` is required for SSH functionality. Custom-command-only deployments need no SSH client when `import_ssh` is `false`.

```sh
sh scripts/build.sh
./output/nitro-ondemand-tunneler-linux-amd64 -wizardconfig
./output/nitro-ondemand-tunneler-linux-amd64
```

On macOS choose `darwin-arm64` for Apple Silicon or `darwin-amd64` for Intel. If an artifact loses its executable bit during download, run `chmod +x` on it.

Windows PowerShell:

```powershell
./scripts/build.ps1
./output/nitro-ondemand-tunneler-windows-amd64.exe -wizardconfig
./output/nitro-ondemand-tunneler-windows-amd64.exe
```

The wizard snapshots discovered SSH forwards and existing JSON entries, then lets you add custom tunnels one at a time. It validates entries and re-prompts for invalid addresses, ports, duplicate names/listeners, and malformed commands. Enter either a JSON argument array or a plain shell command. After each entry (or Enter at the name prompt to keep just the snapshot), it displays the complete proposed JSON and asks before saving or replacing the selected configuration. It then asks whether to add another tunnel or finish. Declining save discards only the pending changes; EOF finishes without implicitly saving. Saved entries remain on disk between additions.

The default destination is `~/.nitro-ondemand-tunneler.rc`; `-config` selects another path. Missing parent directories are created, and saving uses a synced temporary file with restrictive permissions on Unix before replacing the destination. Save errors are displayed and can be retried without deleting the old file. If SSH discovery fails, the wizard offers to continue without importing forwards while retaining existing JSON entries. Invalid existing JSON must be corrected first. On Windows, if replacing the file is refused, finish and use `-config` with a new path rather than deleting the original automatically.

## SSH: use the configuration you already have

Given `~/.ssh/config`:

```sshconfig
Host development-bastion
    HostName bastion.example.com
    User developer
    IdentityFile ~/.ssh/id_ed25519
    LocalForward 127.0.0.1:15432 database.internal:5432
```

Run Nitro, then connect your database client to `127.0.0.1:15432`. Nitro discovers the forward, starts SSH on demand, and relays traffic after the private forwarding endpoint is ready.

### Discovery and configuration boundaries

- Discovery finds **concrete `Host` aliases**, including aliases in `Include` files. Wildcard-only hosts cannot enumerate destinations; add concrete aliases or explicit JSON SSH entries.
- Effective options come from `ssh -G -F <config> <alias>`. OpenSSH, not Nitro, evaluates `Host`, `Match`, identity options, and proxy configuration.
- Discovery supports quoted paths, comments, globbed includes, and `~/` expansion. Relative include paths follow the user OpenSSH convention of resolving against `~/.ssh`. Token/environment expansion in Include paths is not implemented by discovery.
- Only TCP `LocalForward` is imported. Unix-domain sockets are rejected; `RemoteForward` and `DynamicForward` are not imported. Wildcard/unspecified binds are narrowed to loopback.
- An inherited forward may appear under multiple aliases. For duplicate listening IP/port pairs, Nitro keeps the first occurrence and logs each skipped tunnel, naming both the retained and skipped entries. `localhost`, equivalent IP spellings, and numeric port spellings are normalized before comparison. Different IPs or ports remain separate listeners.
- SSH aliases are discovered in configuration order, expanding `Include` files where encountered (glob matches in filename order). Imported entries precede new JSON entries; name-based JSON replacements retain the imported position. After merging and disabling, the first listener wins. The wizard applies the same rule before saving its snapshot.
- Nitro runs a sanitized effective SSH config with inherited forwards and multiplexing removed. Complex configurations should be verified locally; this is not a guarantee of compatibility with every OpenSSH version or extension.
- Imported names are stable strings such as `ssh:development-bastion:127.0.0.1:15432`.

Authentication is noninteractive (`BatchMode yes`): set up keys, your agent, and known-host verification beforehand. Nitro never disables host-key checking. Encrypted keys need an agent; password and first-use host confirmation prompts cannot be answered through Nitro.

## JSON configuration

Default location: `~/.nitro-ondemand-tunneler.rc`. Despite the `.rc` suffix, this is strict JSON, not a shell script.

```json
{
  "version": 1,
  "import_ssh": false,
  "tunnels": [
    {
      "name": "database-container",
      "listen": "127.0.0.1:15432",
      "backend": "127.0.0.1:25432",
      "command": ["docker", "start", "development-postgres"]
    },
    {
      "name": "private-api",
      "listen": "127.0.0.1:18443",
      "ssh": {
        "alias": "development-bastion",
        "target": "api.internal:443"
      }
    }
  ]
}
```

For the container example, create a container publishing its service on `127.0.0.1:25432` first. The front-door port and backend port **must differ**.

Omitting `import_ssh` defaults it to `true`. JSON entries replace imported entries **by name**, replacing the entire entry rather than merging individual fields. Add `{ "name": "ssh:development-bastion:127.0.0.1:15432", "disabled": true }` to disable an imported entry.

Custom commands support either `"command": ["docker", "start", "database"]` (argument arrays executed directly without a shell) or `"shell_command": "docker start database && echo ready"`. Choose exactly one; neither can be combined with `ssh`. Shell commands run through `/bin/sh -c` on Linux/macOS or `cmd.exe /D /S /C` on Windows, so quoting and available commands are platform-specific. For PowerShell, use an explicit argv such as `["powershell", "-NoProfile", "-Command", "..."]`. Treat configuration as executable code. Commands inherit Nitro's environment and working directory. Use absolute executable paths when running as a service.

In the wizard, input beginning with `[` is parsed strictly as JSON argv; other input is a shell command. Prefix input with `shell:` to force shell mode when the command itself begins with `[`. The wizard saves shell input in `shell_command`, preserving its quoting rather than attempting to split it into arguments. The preview shows command contents and may expose secrets to anyone viewing the terminal.

Custom commands should perform a bounded startup action and **exit 0**; use a service manager or a detached container for long-running workloads. Nitro then polls the backend until TCP readiness or the shared startup deadline. If the backend already accepts TCP, the command is skipped. Readiness probes open and close a real connection; they do not validate application health or TLS.

SSH uses `ssh -f -N` with `ExitOnForwardFailure yes`: the launcher must exit 0 before traffic is released. This confirms authentication and local forward setup, **not remote service health**. The private local readiness probe likewise cannot prove the remote service is working; failures can still occur on the first forwarded request.

## Flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `-wizardconfig` | false | Interactive JSON setup |
| `-config` | `~/.nitro-ondemand-tunneler.rc` | Override JSON path |
| `-ssh-config` | `~/.ssh/config` | Override OpenSSH source |
| `-startup-timeout` | `45s` | Maximum startup/connection hold time |
| `-max-connections` | `256` | Global active connection limit; excess arrivals are closed |
| `-allow-public` | false | Explicitly permit non-loopback bind addresses |
| `-verbose` | false | Detailed timestamped configuration, discovery, startup, readiness, connection, relay, and shutdown logs |

Use `-h` for built-in help. A healthy service stays running until Ctrl+C or SIGTERM, including between connections and when no tunnels are configured. Run `-wizardconfig` in another terminal or save JSON/SSH configuration; listeners start when valid tunnels become available. The wizard and help are intentionally one-shot commands. Invalid initial configuration, initial SSH discovery failures, initial bind failures, and unexpected listener failures remain errors and exit nonzero.

### Configuration hot reload

Nitro checks the selected JSON file, the selected SSH configuration, and recursively discovered SSH `Include` files every two seconds. Content changes, atomic file replacements, creation/deletion, and new/deleted Include glob matches trigger reload without a restart. Only these sources are watched; changes to system-wide SSH configuration, keys, environment variables, or other external dependencies do not trigger reload.

Reloads repeat discovery, merging, duplicate selection, and validation. New listeners are prepared before committing the update. Invalid edits, SSH discovery errors, or bind conflicts are logged prominently and leave the previous listeners running; fix the problem and save a configuration file again to retry. Removing all tunnels leaves Nitro running and watching for further edits.

Unchanged custom tunnels retain their startup state. Changed listeners use the new configuration for new connections; established connections retain their original tunnel and are not interrupted, even if their listener is removed. SSH tunnels receive a fresh private endpoint on reload so new connections use current SSH settings. Detached SSH processes and custom services are not terminated by reload; old SSH forwards may therefore remain running. Changes requiring incompatible overlapping bind addresses may be rejected rather than interrupting the old listeners; remove the old listener in a separate save first if necessary.

Both regular and `-verbose` modes log detected configuration changes, successful reloads, failures, and listener additions/removals/reconfiguration. Verbose mode additionally logs discovery, validation, preparation, and commit details. Configuration contents and proxied payloads are not logged.

For troubleshooting, launch from a terminal so startup errors remain visible:

```sh
./output/nitro-ondemand-tunneler-linux-amd64 -verbose
```

Logs go to stderr. Verbose mode includes configuration paths, discovered aliases and forwards, merge/validation steps, listener setup, private SSH endpoint allocation, command executable/PID/exit status and duration, each readiness probe, shared startup waits and cooldowns, connection admission/rejection, relay byte totals/errors in both directions, cleanup, and shutdown. It does not log proxied payloads, full command argument arrays, or effective SSH/configuration contents. Existing command-failure diagnostics still include bounded child output, which may contain secrets; treat diagnostic logs as sensitive. Idle configuration polling and readiness probes can produce frequent verbose messages.

Exit code 0 indicates normal shutdown, not “one connection completed.” Successfully launched SSH processes and custom services are not terminated when Nitro stops.

## Build matrix

| OS | x64 (`amd64`) | ARM64 (`arm64`) |
| --- | --- | --- |
| Linux | ✓ | ✓ |
| macOS (`darwin`) | ✓ | ✓ |
| Windows | ✓ `.exe` | ✓ `.exe` |

Both build scripts invoke Docker to write all six binaries and `SHA256SUMS` into **`./output`**; neither uses the host Go environment. The Docker builder runs vet and race-enabled tests, then cross-compiles with `CGO_ENABLED=0` using the toolchain in the Dockerfile. Generated output is ignored by Git. Cross-compilation does not imply runtime validation on every architecture: CI additionally runs tests on GitHub-hosted Linux, macOS, and Windows runners.

### Containerized build

Export binaries without installing Go on the host:

```sh
docker build --target artifacts --output type=local,dest=./output .
```

Or use the default build/export image:

```sh
docker build -t nitro-builder .
mkdir -p output
docker run --rm -v "$(pwd)/output:/output" nitro-builder
```

PowerShell bind mount:

```powershell
New-Item -ItemType Directory -Force output | Out-Null
docker run --rm -v "${PWD}/output:/output" nitro-builder
```

The container is for **building**. Run the generated executable on the host to own host-loopback listeners. Docker Desktop networking is not a transparent replacement for host interception, and no SSH credentials are copied into the build image.

## CI and local verification

GitHub Actions runs `go vet` and race-enabled tests on three operating systems, then builds and uploads all six artifacts through Docker. Download `nitro-six-platforms` from a successful Actions run for CI builds. Pushing a tag (for example, `v1.0.0`) also publishes a GitHub Release with all six executables and `SHA256SUMS` as individual, uncompressed assets—no ZIP or tar archives are required. Download the executable for your platform directly from the release; on Linux and macOS, run `chmod +x` on the downloaded file before running it. Release signing is not automated.

Run the containerized checks and builds locally:

```sh
sh scripts/build.sh
```

For optional native development checks only, with Go 1.23+ installed:

```sh
go vet ./...
go test -race -count=1 ./...
```

Tests cover strict configuration, merging/disabling, first-listener selection and duplicate warnings, SSH discovery order, public-bind protection, SSH address/token parsing, command exit semantics, failed startup, a real loopback relay, reload change detection, listener reuse/removal, failed-reload rollback, preservation of established streams, and reload logging in regular and verbose modes. Full OpenSSH authentication and server compatibility need an integration environment; CI does not pretend to provision your bastion.

## Security and operational limits

- **Only use listeners and destinations you control or are authorized to access.** This tool does not bypass authentication, decrypt TLS, inspect payloads, or sniff unrelated traffic.
- **No transparent remote-IP interception.** Configure the client to use Nitro's local endpoint. A non-loopback listen IP must belong to the machine. Redirecting an existing remote destination requires separate OS-specific routing/firewall facilities and loop avoidance; Nitro does not install them.
- **Client deadlines still apply.** Nitro holds the application stream, not the client's timeout budget. A client with a short deadline may give up before infrastructure is ready. Incoming bytes remain subject to ordinary TCP buffering/backpressure; Nitro does not spool arbitrary payloads.
- **Loopback by default.** `-allow-public` can expose a sensitive service to the network with no additional authentication. Apply host firewall policy and protocol-level access controls.
- **Config is trusted code.** JSON commands execute as your user; OpenSSH configuration can itself contain executable options such as `ProxyCommand` or `Match exec`, including during `ssh -G`. Never consume untrusted configuration.
- **Detached SSH lifetime is explicit.** Successfully started SSH tunnels remain running after Nitro exits; there is no automatic idle teardown or PID manager. A restart may create another private tunnel. Inspect and terminate only your own SSH processes when finished. Custom-started services likewise remain running.
- **SSH launch timeouts are best-effort.** A process that has already forked can outlive a timed-out launcher. Custom commands may also spawn descendants; context cancellation kills the direct process, not a portable process tree.
- **Private SSH port allocation has a small race.** Nitro selects an available loopback port before SSH binds it. Another process could take it; `ExitOnForwardFailure` then rejects startup rather than silently continuing. Use a trusted local environment.
- **No automatic retry of an in-flight stream.** Failed startup closes waiting connections; later arrivals can retry after a short cooldown. Existing streams cannot migrate after backend failure.
- **Resource bounds are not idle eviction.** Established streams have no idle timeout. A stalled half-close can occupy a slot until the peer finishes or Nitro shuts down.
- On Unix, low-numbered ports may need special privileges. Prefer high loopback ports. Windows requires an installed OpenSSH client for SSH mode; macOS and Linux commonly provide one.

## Design direction

Potential next steps: managed SSH process lifecycles, an idle-reaping policy, application-aware health checks, end-to-end SSH integration fixtures, signed releases, and optional platform-specific redirection adapters. These are roadmap ideas, not shipped features.

**Small surface area. Clear trade-offs. Infrastructure that wakes up when you need it.**
