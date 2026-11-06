package main

import (
 "context"
 "crypto/sha256"
 "fmt"
 "log"
 "net"
 "os"
 "reflect"
 "sync"
 "time"
)

// Hash contents, not just timestamps: this detects atomic editor replacements,
// deletions, and new/deleted Include glob matches without a platform watcher.
func configurationFingerprint(configPath, sshPath string) string {
 hash := sha256.New()
 add := func(path string) {
  data, err := os.ReadFile(path)
  fmt.Fprintf(hash, "%q:%d:%v:", path, len(data), err)
  hash.Write(data)
 }
 add(configPath)
 _, err := discoverAliases(sshPath, add)
 fmt.Fprintf(hash, "discovery:%v", err)
 return fmt.Sprintf("%x", hash.Sum(nil))
}

type liveBinding struct {
 listener net.Listener
 tunnel *tunnel // protected by liveService.mu
 retired bool  // protected by liveService.mu
}

type liveService struct {
 ctx context.Context
 cancel context.CancelFunc
 sshPath string
 timeout time.Duration
 slots chan struct{}
 mu sync.RWMutex
 bindings map[string]*liveBinding
 accepts sync.WaitGroup
 clients sync.WaitGroup
 failures chan error
}

func newLiveService(ctx context.Context, sshPath string, timeout time.Duration, limit int) *liveService {
 ctx, cancel := context.WithCancel(ctx)
 return &liveService{
  ctx: ctx, cancel: cancel, sshPath: sshPath, timeout: timeout,
  slots: make(chan struct{}, limit), bindings: map[string]*liveBinding{},
  failures: make(chan error, 1),
 }
}

// Prepare every new listener before committing. A failed reload leaves the
// previous listeners and tunnel objects untouched. Existing clients keep their
// original tunnel, even when its listener is removed or reconfigured.
func (s *liveService) apply(specs []Spec) error {
 s.mu.Lock()
 defer s.mu.Unlock()
 next := make(map[string]*liveBinding, len(specs))
 prepared := map[string]*tunnel{}
 created := []*liveBinding{}
 rollback := func() {
  for _, b := range created { b.listener.Close() }
 }
 for _, spec := range specs {
  old := s.bindings[spec.Listen]
  if old != nil && spec.SSH == nil && reflect.DeepEqual(old.tunnel.spec, spec) {
   next[spec.Listen] = old
   continue
  }
  backend := spec.Backend
  if spec.SSH != nil {
   debugf("%s: reserving private loopback SSH endpoint", spec.Name)
   reservation, err := net.Listen("tcp", "127.0.0.1:0")
   if err != nil { rollback(); return err }
   backend = reservation.Addr().String()
   reservation.Close()
   debugf("%s: selected private SSH endpoint %s", spec.Name, backend)
  }
  tun := &tunnel{spec: spec, backend: backend, sshConfig: s.sshPath, timeout: s.timeout, root: s.ctx}
  if old != nil {
   next[spec.Listen] = old
   prepared[spec.Listen] = tun
   continue
  }
  debugf("%s: binding TCP listener %s", spec.Name, spec.Listen)
  listener, err := net.Listen("tcp", spec.Listen)
  if err != nil { rollback(); return fmt.Errorf("%s: %w", spec.Name, err) }
  b := &liveBinding{listener: listener, tunnel: tun}
  created = append(created, b)
  next[spec.Listen] = b
 }
 for address, b := range s.bindings {
  if next[address] == nil {
   b.retired = true
   b.listener.Close()
   log.Printf("%s: listener removed from %s; existing clients retained", b.tunnel.spec.Name, address)
  }
 }
 for address, tun := range prepared {
  next[address].tunnel = tun
  log.Printf("%s: listener %s reconfigured -> %s (new connections only)", tun.spec.Name, address, tun.backend)
 }
 s.bindings = next
 for _, b := range created {
  log.Printf("%s: listening on %s -> %s (on demand)", b.tunnel.spec.Name, b.listener.Addr(), b.tunnel.backend)
  s.accepts.Add(1)
  go s.accept(b)
 }
 return nil
}

func (s *liveService) accept(b *liveBinding) {
 defer s.accepts.Done()
 for {
  client, err := b.listener.Accept()
  s.mu.RLock()
  tun, retired := b.tunnel, b.retired
  s.mu.RUnlock()
  if err != nil {
   if !retired && s.ctx.Err() == nil {
    select {
    case s.failures <- fmt.Errorf("%s: listener failed: %w", tun.spec.Name, err):
    default:
    }
   }
   debugf("%s: accept loop stopped: %v", tun.spec.Name, err)
   return
  }
  if retired || s.ctx.Err() != nil { client.Close(); continue }
  debugf("%s: accepted TCP connection from %s", tun.spec.Name, client.RemoteAddr())
  select {
  case s.slots <- struct{}{}:
   debugf("%s: admitted connection; occupied slots=%d/%d", tun.spec.Name, len(s.slots), cap(s.slots))
   s.clients.Add(1)
   go func(tun *tunnel, client net.Conn) {
    defer s.clients.Done()
    defer func() { <-s.slots; debugf("%s: released connection slot", tun.spec.Name) }()
    tun.handle(client)
   }(tun, client)
  default:
   client.Close()
   reportError(tun.spec.Name+": client rejected", fmt.Errorf("connection limit %d reached; rejected client %s", cap(s.slots), client.RemoteAddr()), "Close unused connections or raise -max-connections within available resource limits.")
  }
 }
}

func (s *liveService) close() {
 s.cancel()
 s.mu.Lock()
 for _, b := range s.bindings {
  b.retired = true
  b.listener.Close()
  debugf("%s: listener closed", b.tunnel.spec.Name)
 }
 s.mu.Unlock()
 // Accept loops must finish adding clients before waiting for client workers.
 s.accepts.Wait()
 s.clients.Wait()
 log.Printf("service stopped")
}

func serveReloading(ctx context.Context, specs []Spec, sshPath string, timeout time.Duration, limit int, interval time.Duration, load func() ([]Spec, error), fingerprint func() string, previous string) error {
 service := newLiveService(ctx, sshPath, timeout, limit)
 defer service.close()
 if err := service.apply(specs); err != nil { return err }
 if len(specs) == 0 {
  log.Printf("no tunnels configured; staying running and watching configuration. Run -wizardconfig in another terminal or add concrete SSH Host aliases with LocalForward. Ctrl+C to stop")
 } else {
  log.Printf("ready: %d tunnel(s); waiting for connections (Ctrl+C to stop)", len(specs))
 }
 var ticks <-chan time.Time
 if interval > 0 {
  ticker := time.NewTicker(interval)
  defer ticker.Stop()
  ticks = ticker.C
  log.Printf("configuration hot reload enabled; checking JSON and SSH Include files every %s", interval)
 }
 for {
  select {
  case <-ctx.Done():
   debugf("service context canceled: %v", ctx.Err())
   return nil
  case err := <-service.failures:
   return err
  case <-ticks:
   current := fingerprint()
   if current == previous { continue }
   previous = current
   log.Printf("configuration files changed; reloading")
   debugf("hot reload: loading, merging and validating configuration before changing listeners")
   next, err := load()
   if ctx.Err() != nil { return nil }
   if err == nil { err = service.apply(next) }
   if err != nil {
    reportError("configuration reload failed; keeping previous listeners", err, "Correct the configuration or listener conflict and save a configuration file again to retry. No restart is required.")
    continue
   }
   log.Printf("configuration reloaded: %d tunnel(s); existing connections retained", len(next))
   if len(next) == 0 { log.Printf("no tunnels configured; continuing to watch configuration") }
   debugf("hot reload: listener update committed successfully")
  }
 }
}
