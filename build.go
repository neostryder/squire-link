//go:build ignore

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func main() {
	version := "dev"
	if len(os.Args) > 2 {
		fail("usage: go run build.go [version]")
	}
	if len(os.Args) == 2 {
		version = os.Args[1]
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._+-]+$`).MatchString(version) {
		fail("version must contain only letters, digits, period, underscore, plus, or hyphen")
	}
	if err := os.MkdirAll("dist", 0755); err != nil {
		fail("create dist: " + err.Error())
	}
	var sums strings.Builder
	for _, platform := range []struct{ goos, goarch string }{
		{"windows", "amd64"}, {"windows", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"linux", "amd64"}, {"linux", "arm64"},
	} {
		name := fmt.Sprintf("squire-link-%s-%s", platform.goos, platform.goarch)
		if platform.goos == "windows" {
			name += ".exe"
		}
		output := filepath.Join("dist", name)
		// Without VCS stamping the binary depends only on the source and the
		// version, so the same tree always gives the same checksum.
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -X main.version="+version, "-o", output, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+platform.goos, "GOARCH="+platform.goarch)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		fmt.Println("Building", output)
		if err := cmd.Run(); err != nil {
			fail("build " + name + ": " + err.Error())
		}
		data, err := os.ReadFile(output)
		if err != nil {
			fail("read " + name + ": " + err.Error())
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	if err := os.WriteFile(filepath.Join("dist", "SHA256SUMS"), []byte(sums.String()), 0644); err != nil {
		fail("write SHA256SUMS: " + err.Error())
	}
	fmt.Println("Wrote", filepath.Join("dist", "SHA256SUMS"))
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
