package main

import (
 "bytes"
 "context"
 "io"
 "log"
 "net"
 "os"
 "path/filepath"
 "reflect"
 "strings"
 "testing"
 "time"
)

func TestStatusBanner(t *testing.T) {
 var output bytes.Buffer
 printStatus(&output)
 for _, value := range []string{"v1.0.0", "Antonio Vega Alvarez", "https://www.nitrobutton.com", "not@nitrobutton.com", "https://github.com/tonivega", "Lunch or dinner with me: https://comidacontoni.com", "Telegram for some chat fun: @dream_katapult", "https://t.me/dream_katapult"} {
  if !strings.Contains(output.String(), value) { t.Fatalf("status banner missing %q: %s", value, output.String()) }
 }
}

func TestProminentError(t *testing.T) {
 var diagnostics bytes.Buffer
 previousWriter := log.Writer()
 previousVerbose := verboseLogging.Load()
 log.SetOutput(&diagnostics)
 verboseLogging.Store(false)
 defer func() { log.SetOutput(previousWriter); verboseLogging.Store(previousVerbose) }()
 reportError("Backend unavailable", io.ErrUnexpectedEOF, "Check the backend and reconnect.")
 for _, value := range []string{"+==================== ERROR", "!!!  Backend unavailable", "Details: unexpected EOF", "Action: Check the backend and reconnect.", "Lunch or dinner with me: https://comidacontoni.com", "Telegram for some chat fun: @dream_katapult", "https://t.me/dream_katapult"} {
  if !strings.Contains(diagnostics.String(), value) { t.Fatalf("error panel missing %q: %s", value, diagnostics.String()) }
 }
}

func TestWords(t *testing.T) {
 got, err := words(`Include "with spaces" other # comment`)
 if err != nil || !reflect.DeepEqual(got, []string{"Include", "with spaces", "other"}) { t.Fatalf("%q %v", got, err) }
 if _, err := words(`Host "broken`); err == nil { t.Fatal("expected quote error") }
}

func TestForwardAddress(t *testing.T) {
 for _, tc := range []struct{ in, want string }{{"8080", "127.0.0.1:8080"}, {"[::1]:8080", "[::1]:8080"}, {"*:8080", "127.0.0.1:8080"}} {
  got, err := forwardAddress(tc.in, true)
  if err != nil || got != tc.want { t.Fatalf("%s: %s %v", tc.in, got, err) }
 }
}

func TestMergeAndValidation(t *testing.T) {
 got, err := merge([]Spec{{Name:"a"}}, []Spec{{Name:"a", Disabled:true}, {Name:"b", Listen:"127.0.0.1:1234", Backend:"127.0.0.1:2345", Command:[]string{"true"}}})
 if err != nil || len(got) != 1 || got[0].Name != "b" { t.Fatalf("%+v %v", got, err) }
 if err := validate(got, false); err != nil { t.Fatal(err) }
 got[0].Listen = "0.0.0.0:1234"
 if err := validate(got, false); err == nil { t.Fatal("public bind accepted") }
}

func TestFirstListeners(t *testing.T) {
 var diagnostics bytes.Buffer
 previous := log.Writer()
 log.SetOutput(&diagnostics)
 defer log.SetOutput(previous)
 specs := []Spec{
  {Name:"first", Listen:"localhost:1234", Backend:"127.0.0.1:2345", Command:[]string{"true"}},
  // Even an otherwise invalid duplicate must not prevent the first from running.
  {Name:"duplicate", Listen:"127.0.0.1:01234"},
  {Name:"other-port", Listen:"127.0.0.1:1235", Backend:"127.0.0.1:2345", Command:[]string{"true"}},
  {Name:"ipv6-first", Listen:"[::1]:1234", Backend:"127.0.0.1:2345", Command:[]string{"true"}},
  {Name:"ipv6-duplicate", Listen:"[0:0:0:0:0:0:0:1]:1234"},
  {Name:"other-ip", Listen:"127.0.0.2:1234", Backend:"127.0.0.1:2345", Command:[]string{"true"}},
 }
 got := firstListeners(specs)
 names := []string{}
 for _, s := range got { names = append(names, s.Name) }
 if !reflect.DeepEqual(names, []string{"first", "other-port", "ipv6-first", "other-ip"}) { t.Fatalf("unexpected selection: %v", names) }
 if got[0].Listen != "127.0.0.1:1234" { t.Fatalf("not normalized: %s", got[0].Listen) }
 if err := validate(got, false); err != nil { t.Fatal(err) }
 if strings.Count(diagnostics.String(), "duplicate listener") != 2 { t.Fatalf("missing duplicate warnings: %s", diagnostics.String()) }
 for _, name := range []string{"first", "duplicate", "ipv6-first", "ipv6-duplicate"} {
  if !strings.Contains(diagnostics.String(), "\""+name+"\"") { t.Fatalf("warning missing %q: %s", name, diagnostics.String()) }
 }
}

func TestAliasesPreserveDiscoveryOrder(t *testing.T) {
 dir := t.TempDir()
 included := filepath.Join(dir, "included.conf")
 if err := os.WriteFile(included, []byte("Host middle\nHost zebra\n"), 0600); err != nil { t.Fatal(err) }
 path := filepath.Join(dir, "config")
 text := "Host zebra\nInclude \""+filepath.ToSlash(included)+"\"\nHost alpha zebra\nHost *\n"
 if err := os.WriteFile(path, []byte(text), 0600); err != nil { t.Fatal(err) }
 got, err := aliases(path)
 if err != nil { t.Fatal(err) }
 if !reflect.DeepEqual(got, []string{"zebra", "middle", "alpha"}) { t.Fatalf("unexpected discovery order: %v", got) }
}

func TestConfigStrict(t *testing.T) {
 path := filepath.Join(t.TempDir(), "config.json")
 if err := os.WriteFile(path, []byte(`{"version":1,"typo":true}`), 0600); err != nil { t.Fatal(err) }
 if _, err := readConfig(path); err == nil { t.Fatal("unknown key accepted") }
}

func TestCommandHelper(t *testing.T) {
 for _, arg := range os.Args {
  if arg == "nitro-test-fail" { os.Exit(7) }
  if arg == "nitro-test-ok" { os.Exit(0) }
 }
}

func TestCommandExitStatus(t *testing.T) {
 ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second); defer cancel()
 for _, mode := range []string{"ok", "fail"} {
  err := runCommand(ctx, []string{os.Args[0], "-test.run=TestCommandHelper", "--", "nitro-test-"+mode})
  if (err == nil) != (mode == "ok") { t.Fatalf("%s: %v", mode, err) }
 }
}

func TestProxyExistingBackend(t *testing.T) {
 backend, err := net.Listen("tcp", "127.0.0.1:0"); if err != nil { t.Fatal(err) }; defer backend.Close()
 go func() {
  for { c, err := backend.Accept(); if err != nil { return }; go func() { defer c.Close(); _, _ = io.Copy(c, c) }() }
 }()
 listener, err := net.Listen("tcp", "127.0.0.1:0"); if err != nil { t.Fatal(err) }; defer listener.Close()
 ctx, cancel := context.WithCancel(context.Background()); defer cancel()
 tun := &tunnel{spec: Spec{Name:"test", Command:[]string{"must-not-run"}}, backend: backend.Addr().String(), root:ctx, timeout:time.Second}
 done := make(chan struct{})
 go func() { defer close(done); c, err := listener.Accept(); if err == nil { tun.handle(c) } }()
 c, err := net.Dial("tcp", listener.Addr().String()); if err != nil { t.Fatal(err) }
 _ = c.SetDeadline(time.Now().Add(3*time.Second))
 if _, err := io.WriteString(c, "hello"); err != nil { t.Fatal(err) }
 data := make([]byte, 5)
 if _, err := io.ReadFull(c, data); err != nil { t.Fatal(err) }
 if string(data) != "hello" { t.Fatal(string(data)) }
 c.Close()
 select { case <-done: case <-time.After(3*time.Second): t.Fatal("proxy did not stop") }
}

func TestAwaitTunnelsWaitsAndReloads(t *testing.T) {
 ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
 defer cancel()
 checked := make(chan struct{}, 1)
 available := make(chan struct{})
 done := make(chan error, 1)
 go func() {
  specs, err := awaitTunnels(ctx, 5*time.Millisecond, func() ([]Spec, error) {
   select { case checked <- struct{}{}: default: }
   select {
   case <-available: return []Spec{{Name:"added-later"}}, nil
   default: return nil, nil
   }
  })
  if err == nil && (len(specs) != 1 || specs[0].Name != "added-later") { err = io.ErrUnexpectedEOF }
  done <- err
 }()
 select { case <-checked: case <-ctx.Done(): t.Fatal("configuration was not checked") }
 select {
 case err := <-done: t.Fatalf("empty configuration stopped the service: %v", err)
 case <-time.After(30*time.Millisecond):
 }
 close(available)
 select {
 case err := <-done: if err != nil { t.Fatal(err) }
 case <-ctx.Done(): t.Fatal("new configuration was not loaded")
 }
}

func TestAwaitTunnelsCancellation(t *testing.T) {
 ctx, cancel := context.WithCancel(context.Background())
 defer cancel()
 done := make(chan error, 1)
 go func() {
  _, err := awaitTunnels(ctx, time.Hour, func() ([]Spec, error) { cancel(); return nil, nil })
  done <- err
 }()
 select {
 case err := <-done: if err != context.Canceled { t.Fatalf("unexpected result: %v", err) }
 case <-time.After(time.Second): t.Fatal("idle service did not stop on cancellation")
 }
}

func TestAwaitTunnelsReportsLoadErrors(t *testing.T) {
 _, err := awaitTunnels(context.Background(), time.Millisecond, func() ([]Spec, error) { return nil, io.ErrUnexpectedEOF })
 if err != io.ErrUnexpectedEOF { t.Fatalf("configuration error hidden: %v", err) }
}

func TestVerboseLogging(t *testing.T) {
 var diagnostics bytes.Buffer
 previousWriter := log.Writer()
 previousVerbose := verboseLogging.Load()
 log.SetOutput(&diagnostics)
 defer func() { verboseLogging.Store(previousVerbose); log.SetOutput(previousWriter) }()
 verboseLogging.Store(false)
 debugf("quiet action")
 if diagnostics.Len() != 0 { t.Fatal("debug logs emitted without verbose") }
 verboseLogging.Store(true)
 debugf("test action: count=%d", 2)
 if !strings.Contains(diagnostics.String(), "DEBUG test action: count=2") { t.Fatalf("missing detailed log: %s", diagnostics.String()) }
}

func TestServeStaysRunningUntilCanceled(t *testing.T) {
 // A listener on port zero is useful here: no client traffic is necessary to
 // keep the service alive, and a real bound listener must close on shutdown.
 ctx, cancel := context.WithCancel(context.Background())
 defer cancel()
 done := make(chan error, 1)
 go func() {
  done <- serve(ctx, []Spec{{Name:"idle", Listen:"127.0.0.1:0", Backend:"127.0.0.1:1"}}, "", time.Second, 2)
 }()
 select {
 case err := <-done: t.Fatalf("idle service exited immediately: %v", err)
 case <-time.After(50*time.Millisecond):
 }
 cancel()
 select {
 case err := <-done: if err != nil { t.Fatal(err) }
 case <-time.After(3*time.Second): t.Fatal("service did not stop after cancellation")
 }
}

func TestFailedStartup(t *testing.T) {
 l, err := net.Listen("tcp", "127.0.0.1:0"); if err != nil { t.Fatal(err) }
 address := l.Addr().String(); l.Close()
 ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second); defer cancel()
 tun := &tunnel{spec:Spec{Command:[]string{os.Args[0], "-test.run=TestCommandHelper", "--", "nitro-test-fail"}}, backend:address, root:ctx, timeout:time.Second}
 if err := tun.ensure(ctx); err == nil || !strings.Contains(err.Error(), "command failed") { t.Fatalf("unexpected startup result: %v", err) }
}
