//go:build ignore

package main

import (
 "crypto/sha256"
 "fmt"
 "os"
 "path/filepath"
 "sort"
)

func main() {
 paths, err := filepath.Glob("output/nitro-ondemand-tunneler-*")
 if err != nil { panic(err) }
 sort.Strings(paths)
 var result string
 for _, path := range paths {
  data, err := os.ReadFile(path); if err != nil { panic(err) }
  result += fmt.Sprintf("%x  %s\n", sha256.Sum256(data), filepath.Base(path))
 }
 if err := os.WriteFile("output/SHA256SUMS", []byte(result), 0644); err != nil { panic(err) }
}
