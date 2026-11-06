package main

import (
 "bytes"
 "context"
 "io"
 "log"
 "net"
 "os"
 "path/filepath"
 "strings"
 "sync/atomic"
 "testing"
 "time"
)

func TestConfigurationFingerprint(t *testing.T) {
 dir := t.TempDir()
 config := filepath.Join(dir, "config.json")
 ssh := filepath.Join(dir, "ssh_config")
 include := filepath.Join(dir, "included.conf")
 previous := configurationFingerprint(config, ssh)
 change := func(path, text string) {
  t.Helper()
  if err := os.WriteFile(path, []byte(text), 0600); err != nil { t.Fatal(err) }
  next := configurationFingerprint(config, ssh)
  if next == previous { t.Fatalf("change to %s not detected", path) }
  previous = next
 }
 change(config, `{"version":1,"import_ssh":false}`)
 change(ssh, "Include \""+filepath.ToSlash(filepath.Join(dir, "*.conf"))+"\"\n")
 change(include, "Host first\n")
 // Same-size changes must also be detected.
 change(include, "Host other\n")
 replacement := filepath.Join(dir, "replacement")
 if err := os.WriteFile(replacement, []byte("Host third\n"), 0600); err != nil { t.Fatal(err) }
 if err := os.Remove(include); err != nil { t.Fatal(err) }
 if err := os.Rename(replacement, include); err != nil { t.Fatal(err) }
 next := configurationFingerprint(config, ssh)
 if next == previous { t.Fatal("replacement not detected") }
 previous = next
 if err := os.Remove(include); err != nil { t.Fatal(err) }
 if configurationFingerprint(config, ssh) == previous { t.Fatal("Include removal not detected") }
}

func reloadEchoBackend(t *testing.T) net.Listener {
 t.Helper()
 backend, err := net.Listen("tcp", "127.0.0.1:0")
 if err != nil { t.Fatal(err) }
 t.Cleanup(func() { backend.Close() })
 go func() {
  for {
   client, err := backend.Accept()
   if err != nil { return }
   go func() { defer client.Close(); io.Copy(client, client) }()
  }
 }()
 return backend
}

func TestLiveServiceReload(t *testing.T) {
 backend := reloadEchoBackend(t)
 service := newLiveService(context.Background(), "", time.Second, 4)
 defer service.close()
 spec := Spec{Name: "first", Listen: "127.0.0.1:0", Backend: backend.Addr().String(), Command: []string{"must-not-run"}}
 if err := service.apply([]Spec{spec}); err != nil { t.Fatal(err) }
 binding := service.bindings[spec.Listen]
 address := binding.listener.Addr().String()
 client, err := net.Dial("tcp", address)
 if err != nil { t.Fatal(err) }
 defer client.Close()
 client.SetDeadline(time.Now().Add(5*time.Second))
 echo := func() {
  t.Helper()
  if _, err := io.WriteString(client, "ping"); err != nil { t.Fatal(err) }
  data := make([]byte, 4)
  if _, err := io.ReadFull(client, data); err != nil { t.Fatal(err) }
  if string(data) != "ping" { t.Fatalf("unexpected relay data %q", data) }
 }
 echo()
 original := binding.tunnel
 if err := service.apply([]Spec{spec}); err != nil { t.Fatal(err) }
 if binding.tunnel != original { t.Fatal("unchanged tunnel unnecessarily replaced") }
 spec.Name = "updated"
 if err := service.apply([]Spec{spec}); err != nil { t.Fatal(err) }
 if service.bindings[spec.Listen] != binding || binding.tunnel == original { t.Fatal("listener was not reused with updated tunnel") }
 echo()
 // Preparing a later listener fails: the earlier update must not be committed.
 occupied, err := net.Listen("tcp", "127.0.0.1:0")
 if err != nil { t.Fatal(err) }
 defer occupied.Close()
 current := binding.tunnel
 spec.Name = "must-not-commit"
 conflict := Spec{Name: "conflict", Listen: occupied.Addr().String(), Backend: backend.Addr().String()}
 if err := service.apply([]Spec{spec, conflict}); err == nil { t.Fatal("expected bind conflict") }
 if binding.tunnel != current { t.Fatal("failed reload modified live tunnel") }
 echo()
 if err := service.apply(nil); err != nil { t.Fatal(err) }
 if len(service.bindings) != 0 { t.Fatal("listener not removed") }
 echo() // Established streams survive removal too.
 if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
  conn.Close()
  t.Fatal("removed listener still accepts connections")
 }
 if err := service.apply([]Spec{spec}); err != nil { t.Fatal(err) }
 if len(service.bindings) != 1 { t.Fatal("could not add a tunnel after an empty configuration") }
}

func TestReloadLoggingAndRecovery(t *testing.T) {
 for _, verbose := range []bool{false, true} {
  t.Run(map[bool]string{false:"regular", true:"verbose"}[verbose], func(t *testing.T) {
   var diagnostics bytes.Buffer
   writer, oldVerbose := log.Writer(), verboseLogging.Load()
   log.SetOutput(&diagnostics)
   verboseLogging.Store(verbose)
   defer func() { log.SetOutput(writer); verboseLogging.Store(oldVerbose) }()
   ctx, cancel := context.WithCancel(context.Background())
   defer cancel()
   var generation atomic.Int32
   loads := make(chan int32, 4)
   load := func() ([]Spec, error) {
    current := generation.Load()
    loads <- current
    if current == 1 { return nil, io.ErrUnexpectedEOF }
    return nil, nil
   }
   done := make(chan error, 1)
   go func() {
    done <- serveReloading(ctx, nil, "", time.Second, 2, 5*time.Millisecond, load, func() string {
     return string(rune('0'+generation.Load()))
    }, "0")
   }()
   waitLoad := func(want int32) {
    t.Helper()
    select {
    case got := <-loads: if got != want { t.Fatalf("loaded generation %d, want %d", got, want) }
    case <-time.After(3*time.Second): t.Fatal("reload not attempted")
    }
   }
   generation.Store(1)
   waitLoad(1)
   generation.Store(2)
   waitLoad(2)
   // Cancellation waits for logging and worker cleanup before inspecting output.
   cancel()
   select {
   case err := <-done: if err != nil { t.Fatal(err) }
   case <-time.After(3*time.Second): t.Fatal("reload service did not stop")
   }
   output := diagnostics.String()
   for _, text := range []string{"configuration files changed; reloading", "configuration reload failed; keeping previous listeners", "configuration hot reload enabled"} {
    if !strings.Contains(output, text) { t.Fatalf("missing %q: %s", text, output) }
   }
   // The second load may be canceled before commit; the service must nevertheless
   // recover from the first error and attempt the next configuration.
   if verbose && !strings.Contains(output, "DEBUG hot reload:") { t.Fatal("missing verbose reload details") }
   if !verbose && strings.Contains(output, "DEBUG") { t.Fatal("verbose details in regular mode") }
  })
 }
}
