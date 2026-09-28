//go:build ignore

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", output, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+platform.goos, "GOARCH="+platform.goarch)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		fmt.Println("Building", output)
		if err := cmd.Run(); err != nil {
			fail("build " + name + ": " + err.Error())
		}
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
