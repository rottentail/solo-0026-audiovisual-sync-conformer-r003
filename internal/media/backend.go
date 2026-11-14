package media

import (
	"fmt"
	"sync"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media/ffmpeg"
)

// The media package delegates container probing to ffprobe and sample/frame
// decoding to ffmpeg, both from a pinned toolchain. The in-repository parsers
// that read RIFF chunks or Y4M frame markers are kept only as admission
// guards: they enforce the project's narrow media boundary and let the package
// assert that the toolchain reproduced the exact same payload bytes. They never
// supply sample values or picture content to the service.
var (
	backendMu sync.Mutex
	backend   *ffmpeg.Toolchain
)

// SetBackend installs the verified toolchain used by every media entry point.
// Call it once during startup so a misconfigured host fails before serving.
func SetBackend(toolchain *ffmpeg.Toolchain) {
	backendMu.Lock()
	defer backendMu.Unlock()
	backend = toolchain
}

// Backend returns the installed toolchain, if any.
func Backend() *ffmpeg.Toolchain {
	backendMu.Lock()
	defer backendMu.Unlock()
	return backend
}

// EnsureBackend resolves and installs the pinned toolchain when no backend has
// been installed yet. An empty explicit directory means "search the vendored
// location and the environment".
func EnsureBackend(explicitDir string) (*ffmpeg.Toolchain, error) {
	backendMu.Lock()
	defer backendMu.Unlock()
	if backend != nil {
		return backend, nil
	}
	toolchain, err := ffmpeg.Resolve(explicitDir)
	if err != nil {
		return nil, err
	}
	backend = toolchain
	return toolchain, nil
}

func toolchain() (*ffmpeg.Toolchain, error) {
	if installed := Backend(); installed != nil {
		return installed, nil
	}
	return EnsureBackend("")
}

// decodeOutputLimit bounds the decoded output of one media payload. The limit
// is derived from the payload size and never from caller input.
func decodeOutputLimit(length int) int {
	limit := length*2 + 1<<20
	if limit < 1<<20 {
		limit = 1 << 20
	}
	return limit
}

// probeOutputLimit bounds the JSON report of one ffprobe invocation. The report
// carries one JSON object per packet, so the cap scales with the payload and
// has a generous floor for short media.
func probeOutputLimit(length int) int {
	limit := length/2 + 4<<20
	if limit < 1<<20 {
		limit = 1 << 20
	}
	return limit
}

func backendUnavailable(err error) error {
	return fmt.Errorf("media backend is unavailable: %w", err)
}
