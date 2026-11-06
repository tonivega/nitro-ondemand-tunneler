package main

import (
 "context"
 "encoding/json"
 "errors"
 "flag"
 "fmt"
 "io"
 "log"
 "net"
 "os"
 "os/exec"
 "os/signal"
 "path/filepath"
 "strconv"
 "strings"
 "sync"
 "sync/atomic"
 "syscall"
 "time"
)

var verboseLogging atomic.Bool

const applicationVersion = "v1.0.0"

// Always visible, including help, wizard, and failed invocations.
func printStatus(w io.Writer) {
 fmt.Fprintf(w, "+------------------------------------------------------------+\n| Nitro On-Demand Tunneler %s\n| Author: Antonio Vega Alvarez\n| Website: https://www.nitrobutton.com\n| Contact: not@nitrobutton.com\n| GitHub: https://github.com/tonivega\n| Lunch or dinner with me: https://comidacontoni.com\n| Telegram for some chat fun: @dream_katapult\n| Chat: https://t.me/dream_katapult\n+------------------------------------------------------------+\n", applicationVersion)
}

// Keep errors prominent even when verbose logging is disabled. Preserve the
// underlying diagnostic and give the operator a concrete next step.
func reportError(summary string, err error, hint string) {
 log.Printf("\n+==================== ERROR ====================+\n|  !!!  %s\n| Details: %v\n| Action: %s\n| Lunch or dinner with me: https://comidacontoni.com\n| Telegram for some chat fun: @dream_katapult\n| Chat: https://t.me/dream_katapult\n+===============================================+", summary, err, hint)
}

// Log metadata, never proxied payloads or full command arguments/config contents.
func debugf(format string, args ...any) {
 if verboseLogging.Load() { log.Printf("DEBUG "+format, args...) }
}

type SSH struct {
 Alias string `json:"alias"`
 Target string `json:"target"`
}

type Spec struct {
 Name string `json:"name"`
 Listen string `json:"listen"`
 Backend string `json:"backend,omitempty"`
 Command []string `json:"command,omitempty"`
 ShellCommand string `json:"shell_command,omitempty"`
 SSH *SSH `json:"ssh,omitempty"`
 Disabled bool `json:"disabled,omitempty"`
}

type Config struct {
 Version int `json:"version"`
 ImportSSH *bool `json:"import_ssh,omitempty"`
 Tunnels []Spec `json:"tunnels"`
}

func readConfig(path string) (Config, error) {
 c := Config{Version: 1}
 debugf("reading JSON configuration %q", path)
 b, err := os.ReadFile(path)
 if errors.Is(err, os.ErrNotExist) { debugf("configuration %q does not exist; using defaults", path); return c, nil }
 if err != nil { return c, err }
 d := json.NewDecoder(strings.NewReader(string(b)))
 d.DisallowUnknownFields()
 if err = d.Decode(&c); err != nil { return c, err }
 var extra any
 if err = d.Decode(&extra); err != io.EOF { return c, errors.New("config must contain one JSON object") }
 if c.Version != 1 { return c, errors.New("unsupported config version") }
 debugf("loaded configuration %q: version=%d tunnels=%d import_ssh=%t", path, c.Version, len(c.Tunnels), c.ImportSSH == nil || *c.ImportSSH)
 return c, nil
}

func merge(base, overrides []Spec) ([]Spec, error) {
 debugf("merging %d imported tunnels with %d JSON entries", len(base), len(overrides))
 index := map[string]int{}
 for i, s := range base { index[s.Name] = i }
 seen := map[string]bool{}
 for _, s := range overrides {
  if s.Name == "" || seen[s.Name] { return nil, errors.New("config tunnel names must be nonempty and unique") }
  seen[s.Name] = true
  if i, ok := index[s.Name]; ok { base[i] = s } else { index[s.Name] = len(base); base = append(base, s) }
 }
 result := []Spec{}
 for _, s := range base {
  if !s.Disabled { result = append(result, s) } else { debugf("tunnel %q disabled by configuration", s.Name) }
 }
 debugf("merge produced %d enabled tunnels", len(result))
 return result, nil
}

// Keep the first listener in configuration order and report every skipped entry.
// Normalize equivalent IP spellings, localhost and numeric ports before comparing.
func firstListeners(specs []Spec) []Spec {
 result := make([]Spec, 0, len(specs))
 seen := map[string]string{}
 for _, s := range specs {
  host, port, err := net.SplitHostPort(s.Listen)
  if err == nil {
   if host == "localhost" { host = "127.0.0.1" }
   if ip := net.ParseIP(host); ip != nil { host = ip.String() }
   if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 { port = strconv.Itoa(n) }
   s.Listen = net.JoinHostPort(host, port)
  }
  if first, ok := seen[s.Listen]; ok {
   log.Printf("duplicate listener %s: keeping first tunnel %q; skipping %q", s.Listen, first, s.Name)
   continue
  }
  seen[s.Listen] = s.Name
  result = append(result, s)
 }
 return result
}

func validate(specs []Spec, public bool) error {
 for i := range specs {
  s := &specs[i]
  debugf("validating tunnel %q: listen=%s ssh=%t", s.Name, s.Listen, s.SSH != nil)
  if strings.TrimSpace(s.Name) == "" || strings.ContainsAny(s.Name, "\r\n\x00") { return errors.New("tunnel name must be nonempty and contain no control delimiters") }
  listen, err := normalizeEndpoint(s.Listen)
  if err != nil { return fmt.Errorf("%s: invalid listen address: %w", s.Name, err) }
  s.Listen = listen
  host, _, _ := net.SplitHostPort(s.Listen)
  ip := net.ParseIP(host)
  if !public && (ip == nil || !ip.IsLoopback()) { return fmt.Errorf("%s: non-loopback listener requires -allow-public", s.Name) }
  if s.SSH != nil {
   if len(s.Command) != 0 || s.ShellCommand != "" || s.Backend != "" { return fmt.Errorf("%s: SSH cannot be combined with command/backend", s.Name) }
   if s.SSH.Alias == "" || strings.HasPrefix(s.SSH.Alias, "-") || strings.ContainsAny(s.SSH.Alias, "\r\n\x00 \t") { return fmt.Errorf("%s: invalid SSH alias", s.Name) }
   if _, err := normalizeEndpoint(s.SSH.Target); err != nil { return fmt.Errorf("%s: invalid SSH target: %w", s.Name, err) }
  } else {
   backend, err := normalizeEndpoint(s.Backend)
   if err != nil { return fmt.Errorf("%s: invalid backend: %w", s.Name, err) }
   s.Backend = backend
   if s.Backend == s.Listen { return fmt.Errorf("%s: backend must not equal listen", s.Name) }
   if _, err := startupArgv(*s); err != nil { return fmt.Errorf("%s: %w", s.Name, err) }
  }
 }
 return nil
}

// Bound child-process diagnostics even when a command is noisy.
type tailBuffer struct { mu sync.Mutex; data []byte }
func (b *tailBuffer) Write(p []byte) (int, error) {
 b.mu.Lock(); defer b.mu.Unlock()
 n := len(p)
 if len(p) >= 8192 { b.data = append(b.data[:0], p[len(p)-8192:]...) } else {
  b.data = append(b.data, p...)
  if len(b.data) > 8192 { b.data = b.data[len(b.data)-8192:] }
 }
 return n, nil
}
func (b *tailBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

func runCommand(ctx context.Context, argv []string) error {
 if len(argv) == 0 || argv[0] == "" { return errors.New("command argv is required") }
 started := time.Now()
 debugf("executing command %q with %d arguments (arguments and output omitted from verbose logs)", argv[0], len(argv)-1)
 cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
 output := &tailBuffer{}
 cmd.Stdout, cmd.Stderr = output, output
 cmd.WaitDelay = time.Second
 if err := cmd.Start(); err != nil {
  debugf("command %q could not start: %v", argv[0], err)
  return fmt.Errorf("command failed: %w: %s", err, output.String())
 }
 debugf("command %q started: pid=%d", argv[0], cmd.Process.Pid)
 if err := cmd.Wait(); err != nil {
  debugf("command %q pid=%d failed after %s: %v (context=%v)", argv[0], cmd.Process.Pid, time.Since(started), err, ctx.Err())
  return fmt.Errorf("command failed: %w: %s", err, output.String())
 }
 debugf("command %q pid=%d exited successfully after %s", argv[0], cmd.Process.Pid, time.Since(started))
 return nil
}

type tunnel struct {
 spec Spec
 backend string
 sshConfig string
 timeout time.Duration
 root context.Context
 mu sync.Mutex
 flight chan struct{}
 lastErr error
 failedAt time.Time
}

func reachable(ctx context.Context, addr string) bool {
 debugf("TCP readiness probe: dialing %s", addr)
 c, err := (&net.Dialer{Timeout: 200*time.Millisecond}).DialContext(ctx, "tcp", addr)
 if err != nil { debugf("TCP readiness probe %s failed: %v", addr, err); return false }
 c.Close()
 debugf("TCP readiness probe %s succeeded; probe connection closed", addr)
 return true
}

func (t *tunnel) start(ctx context.Context) error {
 debugf("%s: starting readiness check for %s", t.spec.Name, t.backend)
 if reachable(ctx, t.backend) { debugf("%s: backend already available; skipping startup command", t.spec.Name); return nil }
 debugf("%s: backend unavailable; launching infrastructure (ssh=%t)", t.spec.Name, t.spec.SSH != nil)
 var err error
 if t.spec.SSH != nil {
  err = startSSH(ctx, t.sshConfig, *t.spec.SSH, t.backend)
 } else {
  var argv []string
  argv, err = startupArgv(t.spec)
  if err == nil { err = runCommand(ctx, argv) }
 }
 if err != nil { return err }
 debugf("%s: launcher exited successfully; waiting for TCP readiness", t.spec.Name)
 ticker := time.NewTicker(100*time.Millisecond)
 defer ticker.Stop()
 for {
  if reachable(ctx, t.backend) { return nil }
  select { case <-ctx.Done(): return fmt.Errorf("backend not ready: %w", ctx.Err()); case <-ticker.C: }
 }
}

// A single startup attempt is shared by all concurrent arrivals for this tunnel.
func (t *tunnel) ensure(ctx context.Context) error {
 t.mu.Lock()
 if t.flight == nil {
  if t.lastErr != nil && time.Since(t.failedAt) < time.Second {
   err := t.lastErr; t.mu.Unlock(); debugf("%s: startup rejected during failure cooldown: %v", t.spec.Name, err); return err
  }
  debugf("%s: creating shared startup attempt (timeout=%s)", t.spec.Name, t.timeout)
  ch := make(chan struct{})
  t.flight = ch
  go func() {
   startup, cancel := context.WithTimeout(t.root, t.timeout)
   defer cancel()
   err := t.start(startup)
   debugf("%s: shared startup attempt finished: error=%v", t.spec.Name, err)
   t.mu.Lock()
   t.lastErr = err
   if err != nil { t.failedAt = time.Now() }
   t.flight = nil
   close(ch)
   t.mu.Unlock()
  }()
 } else {
  debugf("%s: joining existing startup attempt", t.spec.Name)
 }
 ch := t.flight
 t.mu.Unlock()
 select {
 case <-ctx.Done(): return ctx.Err()
 case <-ch:
  t.mu.Lock(); err := t.lastErr; t.mu.Unlock(); return err
 }
}

func halfClose(c net.Conn) { if tcp, ok := c.(*net.TCPConn); ok { _ = tcp.CloseWrite() } }

func (t *tunnel) handle(client net.Conn) {
 started := time.Now()
 peer := client.RemoteAddr().String()
 debugf("%s: handling client %s; startup deadline=%s", t.spec.Name, peer, t.timeout)
 defer func() {
  client.Close()
  debugf("%s: client %s closed after %s", t.spec.Name, peer, time.Since(started))
 }()
 stop := context.AfterFunc(t.root, func() { _ = client.Close() })
 defer stop()
 ctx, cancel := context.WithTimeout(t.root, t.timeout)
 defer cancel()
 _ = client.SetDeadline(time.Now().Add(t.timeout))
 if err := t.ensure(ctx); err != nil {
  if t.root.Err() == nil { reportError(t.spec.Name+": infrastructure startup failed; client closed", err, "Check command/SSH authentication and backend readiness; use -verbose for details. A later connection can retry after the cooldown.") }
  return
 }
 debugf("%s: client %s dialing ready backend %s", t.spec.Name, peer, t.backend)
 backend, err := (&net.Dialer{}).DialContext(ctx, "tcp", t.backend)
 if err != nil {
  if t.root.Err() == nil { reportError(t.spec.Name+": backend connection failed; client closed", err, "Verify the backend address and service availability, then reconnect.") }
  return
 }
 defer func() { backend.Close(); debugf("%s: backend connection for client %s closed", t.spec.Name, peer) }()
 stopBackend := context.AfterFunc(t.root, func() { _ = backend.Close() })
 defer stopBackend()
 if err := client.SetDeadline(time.Time{}); err != nil {
  if t.root.Err() == nil { reportError(t.spec.Name+": could not clear client startup deadline", err, "Reconnect the client; use -verbose to inspect connection shutdown.") }
  return
 }
 debugf("%s: relaying client %s <-> %s; startup deadline cleared", t.spec.Name, peer, t.backend)
 done := make(chan struct{})
 go func() {
  defer close(done)
  n, err := io.Copy(backend, client)
  debugf("%s: client %s -> backend finished: bytes=%d error=%v; half-closing backend", t.spec.Name, peer, n, err)
  if err != nil && t.root.Err() == nil && !errors.Is(err, net.ErrClosed) {
   reportError(t.spec.Name+": client-to-backend relay failed", err, "Check client and backend connectivity. In-flight streams cannot be retried automatically.")
  }
  halfClose(backend)
 }()
 n, err := io.Copy(client, backend)
 debugf("%s: backend -> client %s finished: bytes=%d error=%v; half-closing client", t.spec.Name, peer, n, err)
 if err != nil && t.root.Err() == nil && !errors.Is(err, net.ErrClosed) {
  reportError(t.spec.Name+": backend-to-client relay failed", err, "Check client and backend connectivity. In-flight streams cannot be retried automatically.")
 }
 halfClose(client)
 <-done
}

func serve(ctx context.Context, specs []Spec, sshPath string, timeout time.Duration, limit int) error {
 if len(specs) == 0 { return errors.New("cannot serve without tunnels") }
 return serveReloading(ctx, specs, sshPath, timeout, limit, 0, nil, nil, "")
}

// An empty first-run configuration is not a completed job. Stay alive and
// discover tunnels when the user saves configuration, without busy-spinning.
func awaitTunnels(ctx context.Context, interval time.Duration, load func() ([]Spec, error)) ([]Spec, error) {
 waiting := false
 for {
  if err := ctx.Err(); err != nil { return nil, err }
  specs, err := load()
  if err != nil { return nil, err }
  if len(specs) != 0 { return specs, nil }
  if !waiting {
   log.Printf("no tunnels configured; staying running and checking configuration every %s. Run -wizardconfig in another terminal or add concrete SSH Host aliases with LocalForward. Ctrl+C to stop", interval)
   waiting = true
  }
  debugf("no tunnels available; waiting %s before reloading configuration", interval)
  timer := time.NewTimer(interval)
  select {
  case <-ctx.Done(): timer.Stop(); return nil, ctx.Err()
  case <-timer.C:
  }
 }
}

func run() error {
 home, err := os.UserHomeDir(); if err != nil { return err }
 // Return parsing errors to main so malformed flags also get an ASCII error
 // panel instead of flag's default immediate exit. Help remains successful.
 flags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
 configPath := flags.String("config", filepath.Join(home, ".nitro-ondemand-tunneler.rc"), "JSON config path")
 sshPath := flags.String("ssh-config", filepath.Join(home, ".ssh", "config"), "OpenSSH config path")
 wizardMode := flags.Bool("wizardconfig", false, "interactively write JSON config")
 public := flags.Bool("allow-public", false, "permit non-loopback listeners (exposes services)")
 timeout := flags.Duration("startup-timeout", 45*time.Second, "maximum startup/connection hold time")
 limit := flags.Int("max-connections", 256, "global concurrent accepted connection limit")
 verbose := flags.Bool("verbose", false, "log detailed configuration, SSH, command, connection and shutdown actions")
 if err := flags.Parse(os.Args[1:]); err != nil {
  if errors.Is(err, flag.ErrHelp) { return nil }
  return fmt.Errorf("invalid command-line options: %w; use -h for usage", err)
 }
 verboseLogging.Store(*verbose)
 if *verbose { log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds) }
 debugf("starting Nitro: pid=%d config=%q ssh-config=%q wizard=%t allow-public=%t startup-timeout=%s max-connections=%d", os.Getpid(), *configPath, *sshPath, *wizardMode, *public, *timeout, *limit)
 if flags.NArg() != 0 { return errors.New("unexpected positional arguments; use -h for usage") }
 if *timeout <= 0 || *limit <= 0 { return errors.New("timeout and connection limit must be positive") }
 ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
 defer cancel()
 load := func() (Config, []Spec, error) {
  c, err := readConfig(*configPath)
  if err != nil { return c, nil, fmt.Errorf("read config %q: %w", *configPath, err) }
  imported := []Spec{}
  if c.ImportSSH == nil || *c.ImportSSH {
   discovery, stop := context.WithTimeout(ctx, 30*time.Second)
   imported, err = importSSH(discovery, *sshPath)
   stop()
   if err != nil { return c, nil, fmt.Errorf("import SSH config %q: %w", *sshPath, err) }
  } else { debugf("SSH discovery disabled by JSON configuration") }
  return c, imported, nil
 }
 if *wizardMode {
  c, err := readConfig(*configPath)
  if err != nil { return fmt.Errorf("read config %q: %w", *configPath, err) }
  var imported []Spec
  var discoveryErr error
  if c.ImportSSH == nil || *c.ImportSSH {
   discovery, stop := context.WithTimeout(ctx, 30*time.Second)
   imported, discoveryErr = importSSH(discovery, *sshPath)
   stop()
  }
  return wizardIO(*configPath, imported, c, os.Stdin, os.Stdout, *public, discoveryErr)
 }
 loadSpecs := func() ([]Spec, error) {
  c, imported, err := load()
  if err != nil { return nil, err }
  specs, err := merge(imported, c.Tunnels)
  if err != nil { return nil, err }
  specs = firstListeners(specs)
  if err := validate(specs, *public); err != nil { return nil, err }
  debugf("configuration ready: %d enabled, unique, validated tunnels", len(specs))
  return specs, nil
 }
 fingerprint := func() string { return configurationFingerprint(*configPath, *sshPath) }
 // Snapshot before loading so edits made during discovery are checked again.
 initialFingerprint := fingerprint()
 specs, err := loadSpecs()
 if ctx.Err() != nil { log.Printf("shutdown requested"); return nil }
 if err != nil { return err }
 return serveReloading(ctx, specs, *sshPath, *timeout, *limit, 2*time.Second, loadSpecs, fingerprint, initialFingerprint)
}

func main() {
 printStatus(os.Stderr)
 if err := run(); err != nil {
  reportError("Nitro cannot continue", err, "Correct the reported options, configuration, SSH setup, or listener conflict, then restart. Use -h for usage or -verbose for detailed diagnostics.")
  os.Exit(1)
 }
 debugf("Nitro exited normally")
}
