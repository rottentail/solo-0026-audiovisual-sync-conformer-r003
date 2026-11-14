// Package ffmpeg wraps the pinned external FFmpeg toolchain used for media
// probing and decoding.
//
// The service never resolves a system FFmpeg installation. Every binary is
// checked against the version and SHA-256 digest recorded here before it is
// used, so a different build on the host cannot silently change decoded sample
// values, frame contents, or accepted container variants.
package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Version is the only FFmpeg release the service accepts.
const Version = "7.1.5"

// Toolchain is a verified pair of FFmpeg executables.
type Toolchain struct {
	dir     string
	ffmpeg  string
	ffprobe string
}

// ToolchainDirEnv names an explicit toolchain directory. The binaries found
// there are still version and digest checked.
const ToolchainDirEnv = "SYNC_FFMPEG_DIR"

// RelativeToolchainRoot is the in-repository location of the pinned binaries.
const RelativeToolchainRoot = "tools/ffmpeg"

type binaryPin struct {
	ffmpeg  string
	ffprobe string
}

// pins records the exact binaries committed under tools/ffmpeg. A digest change
// is a deliberate toolchain upgrade, not a runtime configuration detail.
var pins = map[string]binaryPin{
	"linux/amd64": {
		ffmpeg:  "8744b90e50f37ebd1eaed724f1eacf133523cc6dad8a14aebc181d5d717bef5c",
		ffprobe: "5606d6dc90e8efdafdeccf422e13ff9ec680b9ee0e7bde6946797176e410dafc",
	},
	"linux/arm64": {
		ffmpeg:  "79df0813b796bcdcf0e87215761ec2ee135ffe81c60eca84abc7b0f30a750536",
		ffprobe: "eaa03ff090caa6cc0ab849004ac155c2306985b515d0d9ab41465fab6eb3b3cb",
	},
}

func platformKey() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// Open verifies the toolchain contained in dir.
func Open(dir string) (*Toolchain, error) {
	if dir == "" {
		return nil, fmt.Errorf("ffmpeg toolchain directory is empty")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve ffmpeg toolchain directory: %w", err)
	}
	pin, ok := pins[platformKey()]
	if !ok {
		return nil, fmt.Errorf("no pinned ffmpeg toolchain for %s", platformKey())
	}
	tools := &Toolchain{
		dir:     absolute,
		ffmpeg:  filepath.Join(absolute, "ffmpeg"),
		ffprobe: filepath.Join(absolute, "ffprobe"),
	}
	if err := verifyBinary(tools.ffmpeg, pin.ffmpeg); err != nil {
		return nil, err
	}
	if err := verifyBinary(tools.ffprobe, pin.ffprobe); err != nil {
		return nil, err
	}
	if err := verifyVersion(context.Background(), tools.ffmpeg, "ffmpeg"); err != nil {
		return nil, err
	}
	if err := verifyVersion(context.Background(), tools.ffprobe, "ffprobe"); err != nil {
		return nil, err
	}
	return tools, nil
}

// Resolve locates and verifies the pinned toolchain. An explicit directory wins
// over SYNC_FFMPEG_DIR, which wins over the vendored location discovered from
// the working directory or the executable directory.
func Resolve(explicit string) (*Toolchain, error) {
	if explicit != "" {
		return Open(explicit)
	}
	if fromEnv := strings.TrimSpace(os.Getenv(ToolchainDirEnv)); fromEnv != "" {
		return Open(fromEnv)
	}
	candidates := searchRoots()
	var searched []string
	for _, root := range candidates {
		dir := filepath.Join(root, filepath.FromSlash(RelativeToolchainRoot), runtime.GOOS+"_"+runtime.GOARCH)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return Open(dir)
		}
		searched = append(searched, dir)
	}
	return nil, fmt.Errorf(
		"pinned ffmpeg %s toolchain not found; set %s or pass an explicit directory (searched %s)",
		Version,
		ToolchainDirEnv,
		strings.Join(searched, ", "),
	)
}

func searchRoots() []string {
	var roots []string
	seen := make(map[string]bool)
	add := func(dir string) {
		if dir == "" {
			return
		}
		absolute, err := filepath.Abs(dir)
		if err != nil {
			return
		}
		for current := absolute; ; current = filepath.Dir(current) {
			if !seen[current] {
				seen[current] = true
				roots = append(roots, current)
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
		}
	}
	if working, err := os.Getwd(); err == nil {
		add(working)
	}
	if executable, err := os.Executable(); err == nil {
		add(filepath.Dir(executable))
	}
	return roots
}

func verifyBinary(path, expected string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("pinned ffmpeg binary %s is unavailable: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("pinned ffmpeg binary %s is a directory", path)
	}
	handle, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open pinned ffmpeg binary %s: %w", path, err)
	}
	defer handle.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return fmt.Errorf("hash pinned ffmpeg binary %s: %w", path, err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf(
			"pinned ffmpeg binary %s has digest %s, want %s",
			path,
			actual,
			expected,
		)
	}
	return nil
}

func verifyVersion(ctx context.Context, path, program string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, path, "-version")
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("run %s -version: %w", program, err)
	}
	firstLine := strings.SplitN(string(output), "\n", 2)[0]
	if !strings.HasPrefix(firstLine, program+" version "+Version) {
		return fmt.Errorf(
			"%s at %s reports %q, want version %s",
			program,
			path,
			strings.TrimSpace(firstLine),
			Version,
		)
	}
	return nil
}

// Dir reports the verified toolchain directory.
func (t *Toolchain) Dir() string {
	return t.dir
}

func (t *Toolchain) binary(program string) string {
	switch program {
	case "ffmpeg":
		return t.ffmpeg
	case "ffprobe":
		return t.ffprobe
	default:
		return ""
	}
}

// Describe returns a stable description of the pinned toolchain.
func (t *Toolchain) Describe() string {
	names := []string{"ffmpeg", "ffprobe"}
	sort.Strings(names)
	return fmt.Sprintf("%s %s (%s)", strings.Join(names, "+"), Version, platformKey())
}
