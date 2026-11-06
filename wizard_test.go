package main

import (
 "bytes"
 "context"
 "errors"
 "os"
 "path/filepath"
 "runtime"
 "strings"
 "testing"
 "time"
)

func TestWizardCreatesConfigAndAddsItems(t *testing.T) {
 path := filepath.Join(t.TempDir(), "new", "nested", "config.json")
 input := strings.Join([]string{
  "first", "bad", "127.0.0.1:70000", "0.0.0.0:15432", "localhost:15432",
  "127.0.0.1:15432", "127.0.0.1:25432", "[]", "[broken", "echo ready && echo started",
  "maybe", "YES", "y",
  "first", "second", "127.0.0.1:015432", "127.0.0.1:15433",
  "127.0.0.1:25433", `["echo","ready"]`, "yes", "no",
 }, "\n")
 var output bytes.Buffer
 if err := wizardIO(path, nil, Config{Version: 1}, strings.NewReader(input), &output, false, nil); err != nil { t.Fatal(err) }
 c, err := readConfig(path)
 if err != nil { t.Fatal(err) }
 if len(c.Tunnels) != 2 || c.ImportSSH == nil || *c.ImportSSH { t.Fatalf("bad saved config: %+v", c) }
 if c.Tunnels[0].ShellCommand != "echo ready && echo started" || c.Tunnels[0].Listen != "127.0.0.1:15432" { t.Fatalf("bad shell entry: %+v", c.Tunnels[0]) }
 if len(c.Tunnels[1].Command) != 2 || c.Tunnels[1].ShellCommand != "" { t.Fatalf("bad argv entry: %+v", c.Tunnels[1]) }
 if strings.Count(output.String(), "Proposed configuration") != 2 || strings.Count(output.String(), "Saved ") != 2 { t.Fatal(output.String()) }
 if runtime.GOOS != "windows" {
  info, err := os.Stat(path)
  if err != nil { t.Fatal(err) }
  if info.Mode().Perm() != 0600 { t.Fatalf("permissions: %v", info.Mode()) }
 }
}

func TestWizardDeclineAndEOFDoNotWrite(t *testing.T) {
 for _, input := range []string{
  "\nno\nno\n",
  "\n",
  "item\n127.0.0.1:1234\n",
  "item\n127.0.0.1:1234\n127.0.0.1:2345\necho ready\n",
 } {
  t.Run(input, func(t *testing.T) {
   path := filepath.Join(t.TempDir(), "config")
   original := []byte("original contents\n")
   if err := os.WriteFile(path, original, 0600); err != nil { t.Fatal(err) }
   if err := wizardIO(path, nil, Config{Version: 1}, strings.NewReader(input), &bytes.Buffer{}, false, nil); err != nil { t.Fatal(err) }
   got, err := os.ReadFile(path)
   if err != nil || !bytes.Equal(got, original) { t.Fatalf("original changed: %s, %v", got, err) }
  })
 }
}

func TestWizardSavesEmptySnapshotWithoutNewline(t *testing.T) {
 path := filepath.Join(t.TempDir(), "config")
 if err := wizardIO(path, nil, Config{Version: 1}, strings.NewReader("\nyes"), &bytes.Buffer{}, false, nil); err != nil { t.Fatal(err) }
 data, err := os.ReadFile(path)
 if err != nil { t.Fatal(err) }
 if !strings.Contains(string(data), `"tunnels": []`) { t.Fatal(string(data)) }
}

func TestWizardDiscoveryFailureCanContinue(t *testing.T) {
 path := filepath.Join(t.TempDir(), "config")
 var output bytes.Buffer
 err := wizardIO(path, nil, Config{Version: 1}, strings.NewReader("yes\n\nyes\nno\n"), &output, false, errors.New("ssh unavailable"))
 if err != nil { t.Fatal(err) }
 if _, err := os.Stat(path); err != nil { t.Fatal(err) }
 if !strings.Contains(output.String(), "ssh unavailable") { t.Fatal(output.String()) }
}

func TestWizardSaveFailurePreservesDestination(t *testing.T) {
 path := filepath.Join(t.TempDir(), "directory")
 if err := os.Mkdir(path, 0700); err != nil { t.Fatal(err) }
 marker := filepath.Join(path, "keep")
 if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil { t.Fatal(err) }
 err := wizardIO(path, nil, Config{Version: 1}, strings.NewReader("\nyes\nno\n"), &bytes.Buffer{}, false, nil)
 if err == nil { t.Fatal("save failure hidden") }
 if _, err := os.Stat(marker); err != nil { t.Fatal("destination removed", err) }
 matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".nitro-config-*"))
 if err != nil || len(matches) != 0 { t.Fatalf("temporary files leaked: %v %v", matches, err) }
}

func TestShellCommandExecutionAndValidation(t *testing.T) {
 ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
 defer cancel()
 argv, err := startupArgv(Spec{ShellCommand: "exit 0"})
 if err != nil { t.Fatal(err) }
 if err := runCommand(ctx, argv); err != nil { t.Fatal(err) }
 argv, err = startupArgv(Spec{ShellCommand: "exit 7"})
 if err != nil { t.Fatal(err) }
 if err := runCommand(ctx, argv); err == nil { t.Fatal("nonzero shell exit accepted") }
 for _, spec := range []Spec{
  {}, {ShellCommand: " "}, {Command: []string{"echo"}, ShellCommand: "echo hi"}, {Command: []string{"echo", "\x00"}},
 } {
  if _, err := startupArgv(spec); err == nil { t.Fatalf("invalid command accepted: %+v", spec) }
 }
 s := Spec{Name: "shell", Listen: "127.0.0.1:1234", Backend: "127.0.0.1:2345", ShellCommand: "exit 0"}
 if err := validate([]Spec{s}, false); err != nil { t.Fatal(err) }
 s.Backend = "localhost:01234"
 if err := validate([]Spec{s}, false); err == nil { t.Fatal("equivalent backend/listen accepted") }
}
