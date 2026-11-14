package ffmpeg

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// MediaKind selects the forced demuxer and the expected stream type.
type MediaKind string

const (
	AudioMedia MediaKind = "audio"
	VideoMedia MediaKind = "video"
)

func demuxerFor(kind MediaKind) (string, error) {
	switch kind {
	case AudioMedia:
		return "wav", nil
	case VideoMedia:
		return "yuv4mpegpipe", nil
	default:
		return "", fmt.Errorf("unsupported media kind %q", kind)
	}
}

// probeEntries is a fixed stream-only field list so probe output size is
// independent of payload length.
const probeEntries = "format=format_name,nb_streams" +
	":stream=index,codec_name,codec_type,sample_fmt,sample_rate,channels,bits_per_sample," +
	"width,height,pix_fmt,chroma_location,field_order,r_frame_rate,avg_frame_rate"

// Stream is one stream as reported by ffprobe.
type Stream struct {
	Index            int    `json:"index"`
	CodecName        string `json:"codec_name"`
	CodecType        string `json:"codec_type"`
	SampleFormat     string `json:"sample_fmt"`
	SampleRate       Number `json:"sample_rate"`
	Channels         int    `json:"channels"`
	BitsPerSample    Number `json:"bits_per_sample"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	PixelFormat      string `json:"pix_fmt"`
	ChromaLocation   string `json:"chroma_location"`
	FieldOrder       string `json:"field_order"`
	FrameRate        string `json:"r_frame_rate"`
	AverageFrameRate string `json:"avg_frame_rate"`
}

// Report is the decoded ffprobe output.
type Report struct {
	Format struct {
		FormatName string `json:"format_name"`
		Streams    Number `json:"nb_streams"`
	} `json:"format"`
	Streams []Stream `json:"streams"`
}

// Number accepts numeric fields FFmpeg emits as JSON strings, bare numbers, or
// "N/A" / null.
type Number int64

func (value *Number) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch trimmed {
	case "null", `"N/A"`, `""`, "":
		*value = -1
		return nil
	}
	trimmed = strings.Trim(trimmed, `"`)
	if trimmed == "" || trimmed == "N/A" {
		*value = -1
		return nil
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return fmt.Errorf("decode ffprobe number %q: %w", trimmed, err)
	}
	*value = Number(parsed)
	return nil
}

// Int reports the number when it is non-negative and fits in an int.
func (value Number) Int() (int, bool) {
	if value < 0 || value > 1<<31-1 {
		return 0, false
	}
	return int(value), true
}

// Probe runs ffprobe with a forced demuxer. Forcing the format is what keeps
// the accepted container boundary identical to the project boundary even
// though the tool understands many more formats.
//
// ffprobe runs at warning level and any diagnostic is treated as a rejection:
// a payload that only "mostly" parses must not be silently interpreted.
func (t *Toolchain) Probe(ctx context.Context, kind MediaKind, data []byte, maximum int) (Report, error) {
	demuxer, err := demuxerFor(kind)
	if err != nil {
		return Report{}, err
	}
	args := []string{
		"-hide_banner",
		"-v", "warning",
		"-f", demuxer,
		"-i", "pipe:0",
		"-show_entries", probeEntries,
		"-of", "json",
	}
	result, err := t.run(ctx, "ffprobe", args, data, maximum)
	if err != nil {
		return Report{}, err
	}
	var report Report
	if err := json.Unmarshal(result.Stdout, &report); err != nil {
		return Report{}, fmt.Errorf("decode ffprobe report: %w", err)
	}
	return report, nil
}

// DecodeAudio returns interleaved little-endian 16-bit PCM in file order,
// produced without resampling or channel mixing.
func (t *Toolchain) DecodeAudio(ctx context.Context, data []byte, maximum int) ([]byte, error) {
	args := []string{
		"-hide_banner",
		"-v", "error",
		"-nostdin",
		"-f", "wav",
		"-i", "pipe:0",
		"-map", "0:a:0",
		"-vn", "-sn", "-dn",
		"-c:a", "pcm_s16le",
		"-f", "s16le",
		"pipe:1",
	}
	result, err := t.run(ctx, "ffmpeg", args, data, maximum)
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}

// DecodeVideo returns yuv420p frame bodies in demuxer order. Passthrough
// timing preserves frame count and order; no scaling or chroma conversion is
// allowed to run.
func (t *Toolchain) DecodeVideo(ctx context.Context, data []byte, maximum int) ([]byte, error) {
	args := []string{
		"-hide_banner",
		"-v", "error",
		"-nostdin",
		"-f", "yuv4mpegpipe",
		"-i", "pipe:0",
		"-map", "0:v:0",
		"-an", "-sn", "-dn",
		"-c:v", "rawvideo",
		"-pix_fmt", "yuv420p",
		"-fps_mode", "passthrough",
		"-f", "rawvideo",
		"pipe:1",
	}
	result, err := t.run(ctx, "ffmpeg", args, data, maximum)
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}
