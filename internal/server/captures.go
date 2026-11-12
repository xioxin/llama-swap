package server

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/klauspost/compress/zstd"

	"github.com/mostlygeek/llama-swap/internal/comfy"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// ReqRespCapture is a stored request/response pair for a single metered
// request. ID is the activity row's ID.
type ReqRespCapture struct {
	ID          int               `json:"id"`
	ReqPath     string            `json:"req_path"`
	ReqHeaders  map[string]string `json:"req_headers"`
	ReqBody     []byte            `json:"req_body"`
	RespHeaders map[string]string `json:"resp_headers"`
	RespBody    []byte            `json:"resp_body"`
}

// captureFields is a bitmask controlling what a route stores in a ReqRespCapture.
type captureFields uint

const (
	captureReqHeaders captureFields = 1 << iota
	captureReqBody
	captureRespHeaders
	captureRespBody
)

const (
	captureReqAll  = captureReqHeaders | captureReqBody
	captureRespAll = captureRespHeaders | captureRespBody
	captureAll     = captureReqAll | captureRespAll
)

// captureFieldsByPath overrides the default capture mask for routes carrying
// large binary payloads (audio/image) where storing the full body is wasteful.
var captureFieldsByPath = map[string]captureFields{
	"/v1/audio/speech":         captureReqAll | captureRespHeaders,
	"/v1/audio/voices":         captureReqHeaders | captureRespAll,
	"/v1/audio/transcriptions": captureReqHeaders | captureRespHeaders | captureRespBody,
	"/v1/images/generations":   captureReqAll | captureRespHeaders,
	"/v1/images/edits":         captureReqHeaders | captureRespHeaders,
	"/sdapi/v1/txt2img":        captureReqAll | captureRespHeaders,
	"/sdapi/v1/img2img":        captureReqHeaders | captureRespHeaders,
}

// captureFieldsFor returns the capture mask for a request path. Unlisted routes
// (the OpenAI-compatible JSON endpoints) capture everything.
func captureFieldsFor(path string) captureFields {
	if cf, ok := captureFieldsByPath[path]; ok {
		return cf
	}
	return captureAll
}

// zstdConcurrency bounds how many captures compress (or decompress) at once.
// Each in-flight EncodeAll holds one sub-encoder, so this is also the number
// of sub-encoders the shared encoder allocates.
var zstdConcurrency = max(1, min(runtime.GOMAXPROCS(0), 4))

// zstdEncOptions are the shared zstd encoder options.
//
// Memory retained by a sub-encoder is dominated by its match tables (~4 MiB at
// SpeedBetterCompression) and a history buffer of twice the window size. The
// library defaults create GOMAXPROCS sub-encoders with an 8 MiB window, so a
// single encoder retained ~20 MiB per core once every sub-encoder had been
// used (#1106). Captures are mostly JSON bodies of a few MB at most, which a
// 1 MiB window compresses just as well.
var zstdEncOptions = []zstd.EOption{
	zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
	zstd.WithEncoderConcurrency(zstdConcurrency),
	zstd.WithWindowSize(1 << 20),
}

// zstdEnc is the shared capture encoder. EncodeAll is safe for concurrent use
// and blocks while every sub-encoder is busy, which caps compression memory
// at zstdConcurrency sub-encoders (~6 MiB each) regardless of how many
// requests finish at once. A sync.Pool of encoders instead grew to one
// encoder per peak concurrent request and held them until the GC drained it.
var zstdEnc = mustZstdEncoder()

// zstdDec is the shared capture decoder. DecodeAll is safe for concurrent use
// and bounded by the decoder concurrency the same way.
var zstdDec = mustZstdDecoder()

func mustZstdEncoder() *zstd.Encoder {
	enc, err := zstd.NewWriter(nil, zstdEncOptions...)
	if err != nil {
		panic(fmt.Sprintf("capture zstd encoder: %v", err))
	}
	return enc
}

func mustZstdDecoder() *zstd.Decoder {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(zstdConcurrency))
	if err != nil {
		panic(fmt.Sprintf("capture zstd decoder: %v", err))
	}
	return dec
}

// compressCapture marshals a ReqRespCapture to CBOR and compresses it with zstd.
// Returns the compressed bytes and the original CBOR byte count for logging.
func compressCapture(c *ReqRespCapture) ([]byte, int, error) {
	cborBytes, err := cbor.Marshal(c)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal capture: %w", err)
	}
	return zstdEnc.EncodeAll(cborBytes, nil), len(cborBytes), nil
}

// decompressCapture decompresses zstd-compressed CBOR into a ReqRespCapture.
func decompressCapture(data []byte) (*ReqRespCapture, error) {
	cborBytes, err := zstdDec.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress capture: %w", err)
	}
	var capture ReqRespCapture
	if err := cbor.Unmarshal(cborBytes, &capture); err != nil {
		return nil, fmt.Errorf("unmarshal capture: %w", err)
	}
	return &capture, nil
}

// addCapture compresses a capture and stores it under captureID, then asks the
// background pruner to delete the oldest captures if the total is now over
// the capture size limit. Returns true if the capture was stored.
func (mp *metricsMonitor) addCapture(captureID string, capture ReqRespCapture) bool {
	if !mp.enableCaptures {
		return false
	}

	compressed, uncompressedBytes, err := compressCapture(&capture)
	if err != nil {
		mp.logger.Warnf("failed to compress capture: %v, skipping", err)
		return false
	}

	// captureBuffer limits compressed bytes, the size actually stored.
	if int64(len(compressed)) > mp.maxCaptureBytes {
		mp.logger.Warnf("capture %s too large (%d bytes), skipping: exceeds captureBuffer of %d bytes", captureID, len(compressed), mp.maxCaptureBytes)
		return false
	}

	// Like queueMetrics, this runs after the handler returned, so it does not
	// use the request context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	captures := mp.store.Captures()
	if err := captures.Put(ctx, store.CaptureEntry{
		ID:                captureID,
		Data:              compressed,
		UncompressedBytes: int64(uncompressedBytes),
	}); err != nil {
		mp.logger.Warnf("failed to store capture %s: %v", captureID, err)
		return false
	}
	mp.requestCapturePrune()

	compressionRatio := (1 - float64(len(compressed))/float64(uncompressedBytes)) * 100
	mp.logger.Debugf("Capture %s compressed and saved: %d bytes -> %d bytes (%.1f%% compression)", captureID, uncompressedBytes, len(compressed), compressionRatio)
	return true
}

// getCaptureByID decompresses and unmarshals a capture by capture ID. Returns
// nil if captures are disabled, the capture is not found, or decompression
// fails.
func (mp *metricsMonitor) getCaptureByID(id string) *ReqRespCapture {
	if !mp.enableCaptures {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entry, found, err := mp.store.Captures().Get(ctx, id)
	if err != nil {
		mp.logger.Warnf("failed to read capture %s: %v", id, err)
		return nil
	}
	if !found {
		return nil
	}
	capture, err := decompressCapture(entry.Data)
	if err != nil {
		mp.logger.Warnf("failed to decompress capture %s: %v", id, err)
		return nil
	}
	return capture
}

// sensitiveHeaders lists headers that are redacted in captures.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
}

// headerMap flattens an http.Header to a single-value map.
func headerMap(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for key, values := range h {
		if len(values) > 0 {
			m[key] = values[0]
		}
	}
	return m
}

// redactHeaders replaces sensitive header values in-place with "[REDACTED]".
func redactHeaders(headers map[string]string) {
	for key := range headers {
		if sensitiveHeaders[strings.ToLower(key)] {
			headers[key] = "[REDACTED]"
		}
	}
}

// CaptureKind identifies which capture shape a stored entry holds.
type CaptureKind string

const (
	CaptureKindReqResp CaptureKind = "reqresp"
	CaptureKindComfy   CaptureKind = "comfy"
)

// captureTagComfy prefixes a compressed capture blob to mark it as a ComfyUI
// snapshot. The tag sits in front of the CBOR document inside the zstd frame: a
// CBOR map always starts with a 0xa* byte, so 0x01 is unambiguous.
// Request/response captures keep the upstream format (untagged CBOR), so a
// capture written by upstream code still decodes as a reqresp capture.
const captureTagComfy byte = 0x01

// storedComfyCapture is the CBOR payload of a ComfyUI capture snapshot.
type storedComfyCapture struct {
	Capture *comfy.Capture `json:"capture"`
}

// addComfyCapture stores a ComfyUI capture snapshot under captureID, the
// capture ID recorded on its activity row. Only the small JSON/metadata
// snapshot is stored; output media stays on ComfyUI and is proxied live.
// Returns true when stored.
func (mp *metricsMonitor) addComfyCapture(captureID string, capture *comfy.Capture) bool {
	if !mp.enableCaptures || capture == nil {
		return false
	}

	cborBytes, err := cbor.Marshal(storedComfyCapture{Capture: capture})
	if err != nil {
		mp.logger.Warnf("failed to marshal comfy capture %s: %v", captureID, err)
		return false
	}
	// Tag inside the zstd frame so a comfy capture is distinguishable from an
	// untagged reqresp capture without an extra store column.
	tagged := make([]byte, 0, len(cborBytes)+1)
	tagged = append(tagged, captureTagComfy)
	tagged = append(tagged, cborBytes...)
	compressed := zstdEnc.EncodeAll(tagged, nil)

	// captureBuffer limits compressed bytes, the size actually stored.
	if int64(len(compressed)) > mp.maxCaptureBytes {
		mp.logger.Warnf("comfy capture %s too large (%d bytes), skipping: exceeds captureBuffer of %d bytes", captureID, len(compressed), mp.maxCaptureBytes)
		return false
	}

	// Like queueMetrics, this runs after the handler returned, so it does not
	// use the request context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mp.store.Captures().Put(ctx, store.CaptureEntry{
		ID:                captureID,
		Data:              compressed,
		UncompressedBytes: int64(len(tagged)),
	}); err != nil {
		mp.logger.Warnf("failed to store comfy capture %s: %v", captureID, err)
		return false
	}
	mp.requestCapturePrune()

	mp.logger.Debugf("Comfy capture %s saved: %d -> %d bytes", captureID, len(tagged), len(compressed))
	return true
}

// readCapture loads a stored capture of either shape. Exactly one of comfyCap
// and reqResp is non-nil when found is true. It is the read side of
// addComfyCapture and getCaptureByID's shared store.
func (mp *metricsMonitor) readCapture(id string) (kind CaptureKind, comfyCap *comfy.Capture, reqResp *ReqRespCapture, found bool) {
	if !mp.enableCaptures {
		return "", nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entry, ok, err := mp.store.Captures().Get(ctx, id)
	if err != nil {
		mp.logger.Warnf("failed to read capture %s: %v", id, err)
		return "", nil, nil, false
	}
	if !ok {
		return "", nil, nil, false
	}
	raw, err := zstdDec.DecodeAll(entry.Data, nil)
	if err != nil {
		mp.logger.Warnf("failed to decompress capture %s: %v", id, err)
		return "", nil, nil, false
	}
	if len(raw) > 0 && raw[0] == captureTagComfy {
		var wrapped storedComfyCapture
		if err := cbor.Unmarshal(raw[1:], &wrapped); err != nil || wrapped.Capture == nil {
			mp.logger.Warnf("failed to unmarshal comfy capture %s: %v", id, err)
			return "", nil, nil, false
		}
		return CaptureKindComfy, wrapped.Capture, nil, true
	}
	var capture ReqRespCapture
	if err := cbor.Unmarshal(raw, &capture); err != nil {
		mp.logger.Warnf("failed to unmarshal capture %s: %v", id, err)
		return "", nil, nil, false
	}
	return CaptureKindReqResp, nil, &capture, true
}
