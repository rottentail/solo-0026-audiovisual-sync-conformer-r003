package media

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media/ffmpeg"
)

type VideoClip struct {
	Info   domain.VideoInfo
	Frames [][]byte
}

// parsedHeader is the project's Y4M admission contract. The toolchain decodes
// the frames, but the accepted header shape is still defined here so FFmpeg's
// broader Y4M support (other chroma layouts, bit depths, and interlacing)
// cannot widen the boundary.
type parsedHeader struct {
	width     int
	height    int
	fpsNum    int
	fpsDen    int
	chroma    string
	aspect    string
	interlace string
}

// InspectVideo probes one C420 Y4M payload and decodes its frames through the
// pinned FFmpeg toolchain.
func InspectVideo(data []byte) (VideoClip, error) {
	return InspectVideoContext(context.Background(), data)
}

// InspectVideoContext is InspectVideo with a cancellation scope.
func InspectVideoContext(ctx context.Context, data []byte) (VideoClip, error) {
	tools, err := toolchain()
	if err != nil {
		return VideoClip{}, backendUnavailable(err)
	}

	// Admission guards: enforce the project's header and frame boundary first
	// and extract the complete frames the payload actually carries.
	header, payloadFrames, err := admitY4M(data)
	if err != nil {
		return VideoClip{}, err
	}
	frameBytes := header.width*header.height + 2*(header.width/2)*(header.height/2)
	if frameBytes <= 0 {
		return VideoClip{}, fmt.Errorf("Y4M frame dimensions are invalid")
	}

	// The decoder is handed a canonical Y4M rebuilt from the admitted header and
	// the complete frames. Anything else in the payload (extra frame tags,
	// trailing bytes) is not part of the picture and must not be able to change
	// the frame content or count the decoder produces.
	canonical, err := EncodeY4M(domain.VideoInfo{
		Width:  header.width,
		Height: header.height,
		FPSNum: header.fpsNum,
		FPSDen: header.fpsDen,
		Chroma: header.chroma,
	}, payloadFrames)
	if err != nil {
		return VideoClip{}, err
	}

	report, err := tools.Probe(ctx, ffmpeg.VideoMedia, canonical, probeOutputLimit(len(canonical)))
	if err != nil {
		return VideoClip{}, fmt.Errorf("video payload was rejected by the probe: %w", err)
	}
	if err := matchVideoReport(report, header); err != nil {
		return VideoClip{}, err
	}

	decoded, err := tools.DecodeVideo(ctx, canonical, decodeOutputLimit(len(canonical)))
	if err != nil {
		return VideoClip{}, fmt.Errorf("video decoder rejected the payload: %w", err)
	}
	if len(decoded) == 0 {
		return VideoClip{}, fmt.Errorf("Y4M payload has no frames")
	}
	if len(decoded)%frameBytes != 0 {
		return VideoClip{}, fmt.Errorf("Y4M frame payload is truncated")
	}
	frameCount := len(decoded) / frameBytes
	// The decoded frame count must equal the frames the payload carries. This
	// keeps frame order, count, and duration identical to the previous parser.
	if frameCount != len(payloadFrames) {
		return VideoClip{}, fmt.Errorf("video decoder produced a different frame count")
	}
	if frameCount > domain.MaxVideoFrames {
		return VideoClip{}, fmt.Errorf("Y4M frame count exceeds the limit")
	}
	frames := make([][]byte, frameCount)
	for index := 0; index < frameCount; index++ {
		frame := make([]byte, frameBytes)
		copy(frame, decoded[index*frameBytes:(index+1)*frameBytes])
		frames[index] = frame
	}
	info := domain.VideoInfo{
		Width:      header.width,
		Height:     header.height,
		FPSNum:     header.fpsNum,
		FPSDen:     header.fpsDen,
		FrameCount: frameCount,
		DurationMS: frameCount * 1000 * header.fpsDen / header.fpsNum,
		Chroma:     header.chroma,
		Digest:     DigestBytes(data),
		ByteCount:  len(data),
	}
	return VideoClip{Info: info, Frames: frames}, nil
}

// admitY4M applies the project's Y4M boundary and returns the parsed header
// plus the complete frame bodies the payload carries.
func admitY4M(data []byte) (parsedHeader, [][]byte, error) {
	lineEnd := bytes.IndexByte(data, '\n')
	if lineEnd < 0 {
		return parsedHeader{}, nil, fmt.Errorf("Y4M header line is incomplete")
	}
	header, err := parseY4MHeader(string(data[:lineEnd]))
	if err != nil {
		return parsedHeader{}, nil, err
	}
	frameBytes := header.width*header.height + 2*(header.width/2)*(header.height/2)
	if frameBytes <= 0 {
		return parsedHeader{}, nil, fmt.Errorf("Y4M frame dimensions are invalid")
	}
	frames := make([][]byte, 0, 24)
	cursor := lineEnd + 1
	for cursor < len(data) {
		headerEnd := bytes.IndexByte(data[cursor:], '\n')
		if headerEnd < 0 {
			return parsedHeader{}, nil, fmt.Errorf("Y4M frame header is incomplete")
		}
		headerEnd += cursor
		frameHeader := string(data[cursor:headerEnd])
		fields := strings.Fields(frameHeader)
		if len(fields) == 0 || fields[0] != "FRAME" {
			return parsedHeader{}, nil, fmt.Errorf("expected a Y4M FRAME marker")
		}
		bodyStart := headerEnd + 1
		bodyEnd := bodyStart + frameBytes
		if bodyEnd > len(data) {
			return parsedHeader{}, nil, fmt.Errorf("Y4M frame payload is truncated")
		}
		frame := make([]byte, frameBytes)
		copy(frame, data[bodyStart:bodyEnd])
		frames = append(frames, frame)
		if len(frames) > domain.MaxVideoFrames {
			return parsedHeader{}, nil, fmt.Errorf("Y4M frame count exceeds the limit")
		}
		cursor = bodyEnd
	}
	if len(frames) == 0 {
		return parsedHeader{}, nil, fmt.Errorf("Y4M payload has no frames")
	}
	return header, frames, nil
}

func parseY4MHeader(line string) (parsedHeader, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "YUV4MPEG2" {
		return parsedHeader{}, fmt.Errorf("payload is not Y4M")
	}
	header := parsedHeader{fpsDen: 1, chroma: "C420jpeg", aspect: "0:0", interlace: "p"}
	for _, token := range fields[1:] {
		if len(token) < 2 {
			return parsedHeader{}, fmt.Errorf("invalid Y4M header token")
		}
		value := token[1:]
		switch token[0] {
		case 'W':
			number, err := strconv.Atoi(value)
			if err != nil {
				return parsedHeader{}, fmt.Errorf("invalid Y4M width")
			}
			header.width = number
		case 'H':
			number, err := strconv.Atoi(value)
			if err != nil {
				return parsedHeader{}, fmt.Errorf("invalid Y4M height")
			}
			header.height = number
		case 'F':
			parts := strings.Split(value, ":")
			if len(parts) != 2 {
				return parsedHeader{}, fmt.Errorf("invalid Y4M frame rate")
			}
			num, errNum := strconv.Atoi(parts[0])
			den, errDen := strconv.Atoi(parts[1])
			if errNum != nil || errDen != nil || num <= 0 || den <= 0 {
				return parsedHeader{}, fmt.Errorf("invalid Y4M frame rate")
			}
			header.fpsNum = num
			header.fpsDen = den
		case 'C':
			header.chroma = token
		case 'A':
			header.aspect = value
		case 'I':
			header.interlace = value
		}
	}
	if header.width <= 0 || header.height <= 0 {
		return parsedHeader{}, fmt.Errorf("Y4M width and height are required")
	}
	if header.width%2 != 0 || header.height%2 != 0 {
		return parsedHeader{}, fmt.Errorf("C420 dimensions must be even")
	}
	if header.width > domain.MaxVideoWidth || header.height > domain.MaxVideoHeight {
		return parsedHeader{}, fmt.Errorf("Y4M dimensions exceed the limit")
	}
	if header.fpsNum <= 0 || header.fpsNum > 120 || header.fpsDen > 1001 {
		return parsedHeader{}, fmt.Errorf("Y4M frame rate is outside the accepted range")
	}
	if header.chroma != "C420jpeg" && header.chroma != "C420mpeg2" && header.chroma != "C420" {
		return parsedHeader{}, fmt.Errorf("only planar C420 Y4M is accepted")
	}
	if header.interlace != "p" {
		return parsedHeader{}, fmt.Errorf("only progressive Y4M is accepted")
	}
	return header, nil
}

// matchVideoReport confirms that the toolchain saw the same single C420 stream
// the admission guards accepted.
func matchVideoReport(report ffmpeg.Report, header parsedHeader) error {
	if report.Format.FormatName != "yuv4mpegpipe" {
		return fmt.Errorf("video payload was not probed as Y4M")
	}
	streams, ok := report.Format.Streams.Int()
	if !ok || streams != 1 || len(report.Streams) != 1 {
		return fmt.Errorf("video payload must contain exactly one stream")
	}
	stream := report.Streams[0]
	if stream.CodecType != "video" {
		return fmt.Errorf("video payload does not contain a video stream")
	}
	if stream.CodecName != "rawvideo" {
		return fmt.Errorf("only raw Y4M picture is accepted")
	}
	if stream.PixelFormat != "yuv420p" {
		return fmt.Errorf("only planar C420 Y4M is accepted")
	}
	if stream.Width != header.width || stream.Height != header.height {
		return fmt.Errorf("Y4M geometry does not match the header")
	}
	switch stream.FieldOrder {
	case "", "unknown", "progressive":
	default:
		return fmt.Errorf("only progressive Y4M is accepted")
	}
	return nil
}

func EncodeY4M(info domain.VideoInfo, frames [][]byte) ([]byte, error) {
	if info.Width <= 0 || info.Height <= 0 || info.FPSNum <= 0 || info.FPSDen <= 0 {
		return nil, fmt.Errorf("invalid Y4M output geometry")
	}
	chroma := info.Chroma
	if chroma == "" {
		chroma = "C420jpeg"
	}
	frameBytes := info.Width*info.Height + 2*(info.Width/2)*(info.Height/2)
	var output bytes.Buffer
	fmt.Fprintf(
		&output,
		"YUV4MPEG2 W%d H%d F%d:%d Ip A0:0 %s\n",
		info.Width,
		info.Height,
		info.FPSNum,
		info.FPSDen,
		chroma,
	)
	for _, frame := range frames {
		if len(frame) != frameBytes {
			return nil, fmt.Errorf("Y4M frame byte count does not match geometry")
		}
		output.WriteString("FRAME\n")
		output.Write(frame)
	}
	return output.Bytes(), nil
}

func NormalizeY4M(data []byte) ([]byte, domain.VideoInfo, error) {
	return NormalizeY4MContext(context.Background(), data)
}

// NormalizeY4MContext is NormalizeY4M with a cancellation scope.
func NormalizeY4MContext(ctx context.Context, data []byte) ([]byte, domain.VideoInfo, error) {
	clip, err := InspectVideoContext(ctx, data)
	if err != nil {
		return nil, domain.VideoInfo{}, err
	}
	output, err := EncodeY4M(clip.Info, clip.Frames)
	if err != nil {
		return nil, domain.VideoInfo{}, err
	}
	info := clip.Info
	info.Digest = DigestBytes(output)
	info.ByteCount = len(output)
	return output, info, nil
}
