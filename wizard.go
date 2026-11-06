package main

import (
 "bufio"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "net"
 "os"
 "path/filepath"
 "runtime"
 "strconv"
 "strings"
)

// Numeric ports avoid service-name surprises and reject zero, overflow and
// malformed entries before either the wizard or service attempts to bind.
func normalizeEndpoint(raw string) (string, error) {
 host, port, err := net.SplitHostPort(strings.TrimSpace(raw))
 if err != nil { return "", errors.New("use host:port (IPv6: [::1]:15432)") }
 if host == "" || strings.ContainsAny(host, " \t\r\n\x00/\\") { return "", errors.New("a nonempty host without whitespace is required") }
 if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 { return "", errors.New("port must be a number from 1 to 65535") }
 n, err := strconv.Atoi(port)
 if err != nil || n < 1 || n > 65535 { return "", errors.New("port must be a number from 1 to 65535") }
 if strings.EqualFold(host, "localhost") { host = "127.0.0.1" }
 if ip := net.ParseIP(host); ip != nil { host = ip.String() }
 return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// Shell interpretation is explicitly selected, never applied to JSON argv.
func startupArgv(s Spec) ([]string, error) {
 if s.ShellCommand != "" {
  if len(s.Command) != 0 { return nil, errors.New("choose command argv OR shell_command, not both") }
  if strings.TrimSpace(s.ShellCommand) == "" || strings.ContainsRune(s.ShellCommand, 0) { return nil, errors.New("shell command must be nonempty and contain no NUL") }
  if runtime.GOOS == "windows" { return []string{"cmd.exe", "/D", "/S", "/C", s.ShellCommand}, nil }
  return []string{"/bin/sh", "-c", s.ShellCommand}, nil
 }
 if len(s.Command) == 0 || strings.TrimSpace(s.Command[0]) == "" { return nil, errors.New("command argv or shell_command is required") }
 for _, arg := range s.Command {
  if strings.ContainsRune(arg, 0) { return nil, errors.New("command arguments cannot contain NUL") }
 }
 return s.Command, nil
}

func parseWizardCommand(raw string) ([]string, string, error) {
 raw = strings.TrimSpace(raw)
 if strings.HasPrefix(raw, "[") {
  var argv []string
  if err := json.Unmarshal([]byte(raw), &argv); err != nil { return nil, "", fmt.Errorf("invalid JSON argument array: %w", err) }
  if _, err := startupArgv(Spec{Command: argv}); err != nil { return nil, "", err }
  return argv, "", nil
 }
 // The prefix also permits shell commands that themselves begin with '['.
 raw = strings.TrimSpace(strings.TrimPrefix(raw, "shell:"))
 if _, err := startupArgv(Spec{ShellCommand: raw}); err != nil { return nil, "", err }
 return nil, raw, nil
}

type wizardPrompt struct {
 reader *bufio.Reader
 out io.Writer
}

func (p *wizardPrompt) ask(label string) (string, error) {
 fmt.Fprint(p.out, label)
 line, err := p.reader.ReadString('\n')
 // A final non-newline-terminated answer is still a valid answer. EOF with
 // no answer cancels; in particular it must never imply consent to save.
 if errors.Is(err, io.EOF) && len(line) != 0 { err = nil }
 return strings.TrimSpace(line), err
}

func (p *wizardPrompt) yes(label string) (bool, error) {
 for {
  answer, err := p.ask(label + " [y/N]: ")
  if err != nil { return false, err }
  switch strings.ToLower(answer) {
  case "y", "yes": return true, nil
  case "", "n", "no": return false, nil
  default: fmt.Fprintln(p.out, "Please enter yes or no.")
  }
 }
}

func wizard(path string, imported []Spec, existing Config) error {
 return wizardIO(path, imported, existing, os.Stdin, os.Stdout, false, nil)
}

func wizardIO(path string, imported []Spec, existing Config, input io.Reader, output io.Writer, public bool, discoveryErr error) (result error) {
 p := &wizardPrompt{reader: bufio.NewReader(input), out: output}
 defer func() {
  if errors.Is(result, io.EOF) {
   fmt.Fprintln(output, "\nWizard finished: unsaved changes discarded; previously saved data is unchanged.")
   result = nil
  }
 }()
 if discoveryErr != nil {
  fmt.Fprintf(output, "SSH discovery failed: %v\n", discoveryErr)
  ok, err := p.yes("Continue without importing SSH forwards? Existing JSON entries will be retained")
  if err != nil { return err }
  if !ok { return nil }
  imported = nil
 }
 specs, err := merge(imported, existing.Tunnels)
 if err != nil { return err }
 specs = firstListeners(specs)
 if err := validate(specs, public); err != nil { return fmt.Errorf("existing/imported tunnels are invalid: %w", err) }
 fmt.Fprintf(output, "Configuration destination: %s\n%d existing/imported tunnel(s). SSH forwards will be saved as a snapshot (import_ssh=false).\n", path, len(specs))
 fmt.Fprintln(output, "Enter a new custom tunnel, or leave its name blank to review/save the current snapshot. EOF/Ctrl+D finishes without saving pending changes.")
 for {
  candidate := append([]Spec{}, specs...)
  var name string
  for {
   name, err = p.ask("New tunnel name (Enter to review snapshot): ")
   if err != nil { return err }
   if strings.ContainsAny(name, "\r\n\x00") { fmt.Fprintln(output, "Name cannot contain control delimiters."); continue }
   duplicate := false
   for _, s := range specs { if s.Name == name { duplicate = true; break } }
   if duplicate { fmt.Fprintln(output, "That name already exists; choose a unique name."); continue }
   break
  }
  if name != "" {
   s := Spec{Name: name}
   for {
    raw, err := p.ask("Listen IP:port (e.g. 127.0.0.1:15432): ")
    if err != nil { return err }
    address, err := normalizeEndpoint(raw)
    if err != nil { fmt.Fprintln(output, err); continue }
    host, _, _ := net.SplitHostPort(address)
    ip := net.ParseIP(host)
    if !public && (ip == nil || !ip.IsLoopback()) { fmt.Fprintln(output, "Use a loopback IP, or restart with -allow-public to expose a service."); continue }
    duplicate := false
    for _, other := range specs { if other.Listen == address { duplicate = true; break } }
    if duplicate { fmt.Fprintln(output, "That listener is already configured; choose a different IP or port."); continue }
    s.Listen = address
    break
   }
   for {
    raw, err := p.ask("Backend host:port (must differ, e.g. 127.0.0.1:25432): ")
    if err != nil { return err }
    address, err := normalizeEndpoint(raw)
    if err != nil { fmt.Fprintln(output, err); continue }
    if address == s.Listen { fmt.Fprintln(output, "Backend must not equal listen."); continue }
    s.Backend = address
    break
   }
   fmt.Fprintln(output, "Startup: JSON argv executes directly; plain text executes through /bin/sh on Unix or cmd.exe on Windows. Use shell: to force shell mode. Only enter trusted commands that exit after startup.")
   for {
    raw, err := p.ask("Startup command or JSON argv: ")
    if err != nil { return err }
    argv, shell, err := parseWizardCommand(raw)
    if err != nil { fmt.Fprintln(output, err); continue }
    s.Command, s.ShellCommand = argv, shell
    break
   }
   candidate = append(candidate, s)
  }
  if err := validate(candidate, public); err != nil { return err }
  no := false
  data, err := json.MarshalIndent(Config{Version: 1, ImportSSH: &no, Tunnels: candidate}, "", "  ")
  if err != nil { return err }
  fmt.Fprintf(output, "\nProposed configuration (commands may contain secrets):\n%s\n", data)
  save, err := p.yes("Save this configuration to " + path + " (replaces it if present)?")
  if err != nil { return err }
  if save {
   for {
    if err := saveWizardConfig(path, data); err != nil {
     fmt.Fprintf(output, "Could not save configuration: %v\nThe previous file has not been deleted.\n", err)
     retry, askErr := p.yes("Retry saving? If replacement is refused, finish and use a new -config path")
     if askErr != nil { return askErr }
     if !retry { return err }
     continue
    }
    specs = candidate
    fmt.Fprintln(output, "Saved", path)
    break
   }
  } else { fmt.Fprintln(output, "Not saved; pending changes discarded.") }
  more, err := p.yes("Add another tunnel, or finish wizard mode? Enter yes to add another")
  if err != nil { return err }
  if !more { fmt.Fprintln(output, "Wizard finished."); return nil }
 }
}

func saveWizardConfig(path string, data []byte) error {
 if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil { return fmt.Errorf("create config directory: %w", err) }
 file, err := os.CreateTemp(filepath.Dir(path), ".nitro-config-*")
 if err != nil { return err }
 defer os.Remove(file.Name())
 if _, err := file.Write(append(data, '\n')); err != nil { file.Close(); return err }
 if err := file.Sync(); err != nil { file.Close(); return err }
 if err := file.Close(); err != nil { return err }
 // Never delete the old destination to work around a failed replacement.
 if err := os.Rename(file.Name(), path); err != nil { return fmt.Errorf("replace config: %w", err) }
 return nil
}
