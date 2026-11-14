#!/bin/sh
# Reproducible build of the pinned FFmpeg toolchain used as the media backend.
#
# The service never resolves a system ffmpeg: it runs the exact binaries
# committed under tools/ffmpeg/<os>_<arch>/ whose digests are pinned in
# internal/media/ffmpeg/toolchain.go. This script documents and reproduces
# those binaries from source so the pin can be audited and refreshed.
#
# The configuration is intentionally minimal. It enables only the two demuxers,
# two decoders, and two raw muxers the service needs, with no network
# protocols, no filters that can rescale or resample, and no optional external
# libraries. A decode therefore cannot silently change sample values or picture
# content, and the tool cannot invent support for formats outside the project
# boundary.
#
# Requirements: a C cross toolchain for the target (gcc for arm64, a musl
# x86_64 cross toolchain for amd64), nasm for amd64, make, tar, xz.
# Usage:
#   tools/ffmpeg/build/build.sh linux_amd64
#   tools/ffmpeg/build/build.sh linux_arm64
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
target=${1:-}
case "$target" in
linux_amd64)
    cross_prefix=${X86_CROSS_PREFIX:-x86_64-linux-musl-}
    arch=x86_64
    ;;
linux_arm64)
    cross_prefix=${ARM_CROSS_PREFIX:-aarch64-linux-gnu-}
    arch=aarch64
    ;;
*)
    echo "usage: $0 linux_amd64|linux_arm64" >&2
    exit 2
    ;;
esac

work=${WORK_DIR:-$here/work-$target}
rm -rf "$work"
mkdir -p "$work/src"

# The configure prefix is embedded in the binaries (ffmpeg -version prints it),
# so it must be a fixed string rather than the work directory. Otherwise two
# builds of the same source would produce different digests.
install_prefix=/usr/local

source_tarball=$root/source/ffmpeg-7.1.5.tar.xz
expected_source_sha256=$(awk '$2 == "ffmpeg-7.1.5.tar.xz" {print $1}' "$root/source/SHA256SUMS")
actual_source_sha256=$(sha256sum "$source_tarball" | cut -d' ' -f1)
if [ "$actual_source_sha256" != "$expected_source_sha256" ]; then
    echo "source tarball digest mismatch: $actual_source_sha256" >&2
    exit 1
fi
tar -C "$work/src" --strip-components=1 -xf "$source_tarball"

cd "$work/src"
./configure \
    --prefix="$install_prefix" \
    --arch="$arch" \
    --target-os=linux \
    --cross-prefix="$cross_prefix" \
    --enable-cross-compile \
    --disable-everything \
    --disable-doc \
    --disable-debug \
    --disable-network \
    --disable-autodetect \
    --disable-avdevice \
    --disable-swresample \
    --disable-postproc \
    --enable-avfilter \
    --disable-filters \
    --disable-iconv \
    --disable-zlib \
    --enable-protocol=file,pipe \
    --enable-demuxer=wav,yuv4mpegpipe \
    --enable-muxer=rawvideo,pcm_s16le \
    --enable-decoder=pcm_s16le,rawvideo \
    --enable-encoder=pcm_s16le,rawvideo \
    --enable-static \
    --disable-shared \
    --extra-ldflags=-static

make -j"$(nproc)" ffmpeg
make -j"$(nproc)" ffprobe

mkdir -p "$root/$target"
cp ffmpeg ffprobe "$root/$target/"
chmod 0755 "$root/$target/ffmpeg" "$root/$target/ffprobe"
(cd "$root/$target" && sha256sum ffmpeg ffprobe > SHA256SUMS)
cat "$root/$target/SHA256SUMS"
