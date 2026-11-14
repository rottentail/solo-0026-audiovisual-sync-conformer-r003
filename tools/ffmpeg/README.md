# Pinned FFmpeg toolchain

`internal/media` uses FFmpeg for container probing and sample/frame decoding.
The service never resolves a system `ffmpeg`: it runs the exact binaries in
`linux_amd64/` or `linux_arm64/` after checking their SHA-256 digests and
reported version against the pins in `internal/media/ffmpeg/toolchain.go`.

## Contents

```text
tools/ffmpeg/
├── README.md
├── LICENSE.LGPL-2.1              FFmpeg license for the pinned build
├── build/build.sh                reproducible build script
├── source/
│   ├── SHA256SUMS                digest of the source archive
│   └── ffmpeg-7.1.5.tar.xz       pinned upstream source
├── linux_amd64/{ffmpeg,ffprobe}  static binaries + SHA256SUMS
└── linux_arm64/{ffmpeg,ffprobe}  static binaries + SHA256SUMS
```

The binaries are static, stripped, and built with a deliberately minimal
configuration:

- demuxers: `wav`, `yuv4mpegpipe`
- decoders: `pcm_s16le`, `rawvideo`
- muxers: `rawvideo`, `pcm_s16le`
- protocols: `file`, `pipe`
- no network protocols, no external libraries, no avfilter graph

That keeps the tool from offering rescaling, resampling, or format support the
project does not admit. The project boundary itself is enforced in
`internal/media`, not by the tool.

## Refreshing the pin

1. Download the new source archive and update `source/SHA256SUMS`.
2. Update `Version` and the digest map in
   `internal/media/ffmpeg/toolchain.go`.
3. Build both targets:

   ```bash
   tools/ffmpeg/build/build.sh linux_amd64
   tools/ffmpeg/build/build.sh linux_arm64
   ```

4. Copy the printed `SHA256SUMS` digests into the pin map and run the workflow
   checks.

The build needs a C cross toolchain per target (an `x86_64-linux-musl-` compiler
plus `nasm` for amd64; `aarch64-linux-gnu-gcc` for arm64) and `make`.
