package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media/ffmpeg"
)

type AudioClip struct {
	Info    domain.AudioInfo
	Samples []int16
}

// waveFormat is the project's WAV admission contract. FFmpeg is forced to the
// wav demuxer for decoding, but the accepted shape is still defined here so the
// toolchain's much broader WAV support cannot widen the boundary.
type waveFormat struct {
	audioFormat   uint16
	channels      uint16
	sampleRate    uint32
	byteRate      uint32
	blockAlign    uint16
	bitsPerSample uint16
}

// InspectAudio probes one PCM WAV payload and decodes its samples through the
// pinned FFmpeg toolchain.
func InspectAudio(data []byte) (AudioClip, error) {
	return InspectAudioContext(context.Background(), data)
}

// InspectAudioContext is InspectAudio with a cancellation scope. Cancelling ctx
// kills the decoder process group before returning.
func InspectAudioContext(ctx context.Context, data []byte) (AudioClip, error) {
	tools, err := toolchain()
	if err != nil {
		return AudioClip{}, backendUnavailable(err)
	}

	// Admission guards: the project's WAV boundary is enforced before the tool
	// sees the payload, and the admitted data-chunk bytes are extracted so the
	// decoded sample count and values can be checked against them.
	format, pcm, err := admitWAVE(data)
	if err != nil {
		return AudioClip{}, err
	}

	// The decoder is handed a canonical single-stream WAV rebuilt from the
	// admitted header and chunk. Anything else in the payload (duplicate or
	// trailing chunks, padding) is not part of the media and must not be able to
	// change the sample values the decoder produces.
	canonical, err := EncodePCM16Bytes(int(format.sampleRate), int(format.channels), pcm)
	if err != nil {
		return AudioClip{}, err
	}

	report, err := tools.Probe(ctx, ffmpeg.AudioMedia, canonical, probeOutputLimit(len(canonical)))
	if err != nil {
		return AudioClip{}, fmt.Errorf("audio payload was rejected by the probe: %w", err)
	}
	if err := matchAudioReport(report, format); err != nil {
		return AudioClip{}, err
	}

	decoded, err := tools.DecodeAudio(ctx, canonical, decodeOutputLimit(len(canonical)))
	if err != nil {
		return AudioClip{}, fmt.Errorf("audio decoder rejected the payload: %w", err)
	}
	blockAlign := int(format.channels) * 2
	if len(decoded) == 0 || len(decoded)%blockAlign != 0 {
		return AudioClip{}, fmt.Errorf("WAV sample bytes are incomplete")
	}
	// Decoded bytes must equal the admitted data chunk exactly. This preserves
	// the previous sample values for existing material: no packet padding, no
	// container priming, no channel mixing.
	if len(decoded) != len(pcm) {
		return AudioClip{}, fmt.Errorf("audio decoder produced a different sample count")
	}
	samples := make([]int16, len(decoded)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(decoded[index*2 : index*2+2]))
	}
	info := describeAudio(
		int(format.sampleRate),
		int(format.channels),
		len(samples)/int(format.channels),
		samples,
		len(data),
		DigestBytes(data),
	)
	return AudioClip{Info: info, Samples: samples}, nil
}

// admitWAVE applies the project's WAV boundary and returns the parsed format
// plus the exact data-chunk bytes.
func admitWAVE(data []byte) (waveFormat, []byte, error) {
	if len(data) < 12 {
		return waveFormat{}, nil, fmt.Errorf("audio payload is shorter than a RIFF header")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return waveFormat{}, nil, fmt.Errorf("audio payload is not RIFF/WAVE")
	}
	riffSize := int(binary.LittleEndian.Uint32(data[4:8]))
	if riffSize < 4 || riffSize+8 > len(data) {
		return waveFormat{}, nil, fmt.Errorf("RIFF size does not fit the payload")
	}

	var format waveFormat
	var pcm []byte
	haveFormat := false
	offset := 12
	for offset < len(data) {
		if offset+8 > len(data) {
			return waveFormat{}, nil, fmt.Errorf("truncated WAV chunk header")
		}
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if chunkSize < 0 || offset+8+chunkSize > len(data) {
			return waveFormat{}, nil, fmt.Errorf("WAV chunk %q exceeds payload", chunkID)
		}
		body := data[offset+8 : offset+8+chunkSize]
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return waveFormat{}, nil, fmt.Errorf("WAV format chunk is too short")
			}
			format = waveFormat{
				audioFormat:   binary.LittleEndian.Uint16(body[0:2]),
				channels:      binary.LittleEndian.Uint16(body[2:4]),
				sampleRate:    binary.LittleEndian.Uint32(body[4:8]),
				byteRate:      binary.LittleEndian.Uint32(body[8:12]),
				blockAlign:    binary.LittleEndian.Uint16(body[12:14]),
				bitsPerSample: binary.LittleEndian.Uint16(body[14:16]),
			}
			haveFormat = true
		case "data":
			pcm = body
		}
		offset += 8 + chunkSize
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if !haveFormat {
		return waveFormat{}, nil, fmt.Errorf("WAV format chunk is missing")
	}
	if pcm == nil {
		return waveFormat{}, nil, fmt.Errorf("WAV data chunk is missing")
	}
	if format.audioFormat != 1 {
		return waveFormat{}, nil, fmt.Errorf("only integer PCM audio is accepted")
	}
	if format.channels != 1 && format.channels != 2 {
		return waveFormat{}, nil, fmt.Errorf("audio must have one or two channels")
	}
	if format.bitsPerSample != 16 {
		return waveFormat{}, nil, fmt.Errorf("audio must use 16-bit samples")
	}
	if format.sampleRate < 8000 || format.sampleRate > 192000 {
		return waveFormat{}, nil, fmt.Errorf("sample rate is outside the accepted range")
	}
	blockAlign := int(format.channels) * 2
	if int(format.blockAlign) != blockAlign {
		return waveFormat{}, nil, fmt.Errorf("WAV block alignment is inconsistent")
	}
	if len(pcm) == 0 || len(pcm)%blockAlign != 0 {
		return waveFormat{}, nil, fmt.Errorf("WAV sample bytes are incomplete")
	}
	return format, pcm, nil
}

// matchAudioReport confirms that the toolchain saw the same single PCM stream
// the admission guards accepted.
func matchAudioReport(report ffmpeg.Report, format waveFormat) error {
	if report.Format.FormatName != "wav" {
		return fmt.Errorf("audio payload was not probed as WAV")
	}
	streams, ok := report.Format.Streams.Int()
	if !ok || streams != 1 || len(report.Streams) != 1 {
		return fmt.Errorf("audio payload must contain exactly one stream")
	}
	stream := report.Streams[0]
	if stream.CodecType != "audio" {
		return fmt.Errorf("audio payload does not contain an audio stream")
	}
	if stream.CodecName != "pcm_s16le" || stream.SampleFormat != "s16" {
		return fmt.Errorf("only integer PCM audio is accepted")
	}
	if bits, ok := stream.BitsPerSample.Int(); !ok || bits != 16 {
		return fmt.Errorf("audio must use 16-bit samples")
	}
	if stream.Channels != int(format.channels) {
		return fmt.Errorf("audio channel count does not match the format chunk")
	}
	sampleRate, ok := stream.SampleRate.Int()
	if !ok || sampleRate != int(format.sampleRate) {
		return fmt.Errorf("audio sample rate does not match the format chunk")
	}
	return nil
}

func describeAudio(sampleRate, channels, frames int, samples []int16, byteCount int, digest string) domain.AudioInfo {
	peak := 0.0
	var squareSum float64
	for _, sample := range samples {
		value := float64(sample)
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
		squareSum += value * value
	}
	rms := 0.0
	if len(samples) > 0 {
		rms = math.Sqrt(squareSum / float64(len(samples)))
	}
	return domain.AudioInfo{
		SampleRate: sampleRate,
		Channels:   channels,
		BitDepth:   16,
		FrameCount: frames,
		DurationMS: frames * 1000 / sampleRate,
		PeakDBFS:   amplitudeDBFS(peak),
		RMSDBFS:    amplitudeDBFS(rms),
		Digest:     digest,
		ByteCount:  byteCount,
	}
}

func amplitudeDBFS(value float64) float64 {
	if value <= 0 {
		return -120
	}
	db := 20 * math.Log10(value/32768)
	if db < -120 {
		return -120
	}
	return math.Round(db*100) / 100
}

func EncodePCM16(sampleRate, channels int, samples []int16) ([]byte, error) {
	pcm := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[index*2:index*2+2], uint16(sample))
	}
	return EncodePCM16Bytes(sampleRate, channels, pcm)
}

// EncodePCM16Bytes wraps already interleaved little-endian 16-bit PCM in a
// canonical 44-byte RIFF/WAVE header.
func EncodePCM16Bytes(sampleRate, channels int, pcm []byte) ([]byte, error) {
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, fmt.Errorf("unsupported sample rate")
	}
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("unsupported channel count")
	}
	blockAlign := channels * 2
	if len(pcm)%blockAlign != 0 {
		return nil, fmt.Errorf("sample bytes are not channel aligned")
	}
	dataSize := len(pcm)
	payload := make([]byte, 44+dataSize)
	copy(payload[0:4], "RIFF")
	binary.LittleEndian.PutUint32(payload[4:8], uint32(36+dataSize))
	copy(payload[8:12], "WAVE")
	copy(payload[12:16], "fmt ")
	binary.LittleEndian.PutUint32(payload[16:20], 16)
	binary.LittleEndian.PutUint16(payload[20:22], 1)
	binary.LittleEndian.PutUint16(payload[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(payload[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(payload[28:32], uint32(sampleRate*blockAlign))
	binary.LittleEndian.PutUint16(payload[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(payload[34:36], 16)
	copy(payload[36:40], "data")
	binary.LittleEndian.PutUint32(payload[40:44], uint32(dataSize))
	copy(payload[44:], pcm)
	return payload, nil
}
