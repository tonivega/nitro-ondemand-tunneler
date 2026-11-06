package main

import (
 "bufio"
 "context"
 "errors"
 "fmt"
 "net"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "unicode"
)

// Tokenize the config discovery subset: quoted paths, escaped characters and comments.
// OpenSSH itself evaluates Host/Match and all connection options via ssh -G.
func words(line string) ([]string, error) {
 var result []string
 var word strings.Builder
 quote := rune(0)
 escaped, started := false, false
 for _, c := range line {
  if escaped { word.WriteRune(c); escaped = false; started = true; continue }
  if c == '\\' { escaped = true; started = true; continue }
  if quote != 0 { if c == quote { quote = 0 } else { word.WriteRune(c) }; continue }
  if c == '\'' || c == '"' { quote = c; started = true; continue }
  if c == '#' { break }
  if unicode.IsSpace(c) {
   if started { result = append(result, word.String()); word.Reset(); started = false }
  } else { word.WriteRune(c); started = true }
 }
 if escaped || quote != 0 { return nil, errors.New("unterminated SSH config quote/escape") }
 if started { result = append(result, word.String()) }
 return result, nil
}

func aliases(path string) ([]string, error) {
 return discoverAliases(path, nil)
}

// onFile also visits missing roots, allowing reload detection on creation or deletion.
func discoverAliases(path string, onFile func(string)) ([]string, error) {
 debugf("SSH discovery: scanning aliases from %q", path)
 home, err := os.UserHomeDir(); if err != nil { return nil, err }
 seen, names := map[string]bool{}, map[string]bool{}
 result := []string{}
 var visit func(string, int) error
 visit = func(path string, depth int) error {
  if depth > 16 { return errors.New("SSH Include nesting exceeds 16") }
  path, err := filepath.Abs(path); if err != nil { return err }
  if seen[path] { debugf("SSH discovery: skipping already visited file %q", path); return nil }; seen[path] = true
  if onFile != nil { onFile(path) }
  debugf("SSH discovery: opening %q (include depth=%d)", path, depth)
  file, err := os.Open(path)
  if errors.Is(err, os.ErrNotExist) { debugf("SSH discovery: file %q does not exist", path); return nil }; if err != nil { return err }
  defer file.Close()
  scan := bufio.NewScanner(file)
  scan.Buffer(make([]byte, 4096), 1024*1024)
  for scan.Scan() {
   // Accept both Keyword value and Keyword=value syntax.
   line := strings.TrimSpace(scan.Text())
   if i := strings.IndexAny(line, "= \t"); i >= 0 && line[i] == '=' { line = line[:i]+" "+line[i+1:] }
   fields, err := words(line); if err != nil { return fmt.Errorf("%s: %w", path, err) }
   if len(fields) < 2 { continue }
   values := fields[1:]
   if values[0] == "=" { values = values[1:] }
   switch strings.ToLower(fields[0]) {
   case "host":
    for _, alias := range values {
     if !strings.ContainsAny(alias, "*?!") && !names[alias] {
      names[alias] = true
      result = append(result, alias)
      debugf("SSH discovery: found concrete Host alias %q in %q", alias, path)
     }
    }
   case "include":
    for _, pattern := range values {
     if strings.HasPrefix(pattern, "~/") { pattern = filepath.Join(home, pattern[2:]) }
     if !filepath.IsAbs(pattern) { pattern = filepath.Join(home, ".ssh", pattern) }
     matches, err := filepath.Glob(pattern); if err != nil { return err }
     debugf("SSH discovery: Include %q matched %d files", pattern, len(matches))
     for _, match := range matches { if err := visit(match, depth+1); err != nil { return err } }
    }
   }
  }
  return scan.Err()
 }
 if err := visit(path, 0); err != nil { return nil, err }
 debugf("SSH discovery: found %d concrete aliases", len(result))
 return result, nil
}

func effective(ctx context.Context, path, alias string) ([]byte, error) {
 if strings.HasPrefix(alias, "-") { return nil, errors.New("invalid SSH alias") }
 debugf("SSH: resolving effective configuration with ssh -G -F %q for alias %q", path, alias)
 cmd := exec.CommandContext(ctx, "ssh", "-G", "-F", path, alias)
 cmd.WaitDelay = 1000000000
 out := &tailBuffer{}
 // Effective configs can exceed the diagnostic tail size; use Output for stdout.
 cmd.Stderr = out
 b, err := cmd.Output()
 if err != nil { debugf("SSH: effective config lookup for %q failed: %v", alias, err); return nil, fmt.Errorf("ssh -G %s: %w: %s", alias, err, out.String()) }
 debugf("SSH: effective config resolved for %q (%d bytes; contents omitted)", alias, len(b))
 return b, nil
}

func forwardAddress(s string, bind bool) (string, error) {
 if !strings.Contains(s, ":") && bind { return net.JoinHostPort("127.0.0.1", s), nil }
 host, port, err := net.SplitHostPort(s)
 if err != nil { return "", err }
 if bind && (host == "" || host == "*" || host == "localhost") { host = "127.0.0.1" }
 return net.JoinHostPort(host, port), nil
}

func importSSH(ctx context.Context, path string) ([]Spec, error) {
 names, err := aliases(path); if err != nil { return nil, err }
 result := []Spec{}
 for _, alias := range names {
  b, err := effective(ctx, path, alias); if err != nil { return nil, err }
  for _, line := range strings.Split(string(b), "\n") {
   fields := strings.Fields(line)
   if len(fields) == 0 || fields[0] != "localforward" { continue }
   if len(fields) != 3 { return nil, fmt.Errorf("%s: only TCP LocalForward is supported", alias) }
   listen, err := forwardAddress(fields[1], true); if err != nil { return nil, fmt.Errorf("%s LocalForward: %w", alias, err) }
   target, err := forwardAddress(fields[2], false); if err != nil { return nil, fmt.Errorf("%s: Unix-socket forwards are unsupported: %w", alias, err) }
   debugf("SSH import: alias=%q listener=%s target=%s", alias, listen, target)
   result = append(result, Spec{Name: "ssh:"+alias+":"+listen, Listen: listen, SSH: &SSH{Alias: alias, Target: target}})
  }
 }
 return result, nil
}

func startSSH(ctx context.Context, path string, spec SSH, backend string) error {
 debugf("SSH startup: alias=%q private endpoint=%s target=%s", spec.Alias, backend, spec.Target)
 b, err := effective(ctx, path, spec.Alias); if err != nil { return err }
 // Reuse OpenSSH's resolved options, but remove ALL inherited forwarding and
 // multiplexing. Otherwise the original LocalForward would collide with us.
 var config strings.Builder
 config.WriteString("Host *\n")
 removed := map[string]bool{
  "host":true, "localforward":true, "remoteforward":true, "dynamicforward":true,
  "clearallforwardings":true, "controlmaster":true, "controlpath":true,
  "controlpersist":true, "forkafterauthentication":true, "sessiontype":true,
  "requesttty":true, "stdinnull":true, "exitonforwardfailure":true,
  "batchmode":true, "localcommand":true, "permitlocalcommand":true,
 }
 for _, line := range strings.Split(string(b), "\n") {
  fields := strings.Fields(line)
  if len(fields) != 0 && !removed[strings.ToLower(fields[0])] { config.WriteString(line); config.WriteByte('\n') }
 }
 config.WriteString("BatchMode yes\nExitOnForwardFailure yes\nControlMaster no\nPermitLocalCommand no\n")
 debugf("SSH startup: sanitized inherited forwarding, multiplexing and interactive options")
 file, err := os.CreateTemp("", "nitro-ssh-*.conf"); if err != nil { return err }
 debugf("SSH startup: created private temporary config %q", file.Name())
 defer func() {
  if err := os.Remove(file.Name()); err != nil { debugf("SSH startup: temporary config cleanup failed: %v", err) } else { debugf("SSH startup: removed temporary config %q", file.Name()) }
 }()
 if _, err := file.WriteString(config.String()); err != nil { file.Close(); return err }
 if err := file.Close(); err != nil { return err }
 // -f returns zero only after authentication and local forwarding setup.
 // The detached SSH process remains available for subsequent connections.
 debugf("SSH startup: launching noninteractive detached forwarding; successful SSH processes remain running after Nitro exits")
 err = runCommand(ctx, []string{"ssh", "-F", file.Name(), "-f", "-N", "-L", backend+":"+spec.Target, spec.Alias})
 debugf("SSH startup: launcher completed for alias %q: error=%v", spec.Alias, err)
 return err
}
