package devruntime

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// GGUFMemoryProfile is the memory-relevant geometry read from a GGUF header.
// KVBytesPerToken is the full-precision KV cache cost of one context token
// across all layers, which the runtime allocates exactly because it runs with
// auto-fitting and the host-RAM cache mirror disabled.
type GGUFMemoryProfile struct {
	Architecture string
	BlockCount   int32
	// KVCacheLayers is how many blocks actually hold a growing KV cache. It
	// equals BlockCount for plain attention, and a fraction of it for hybrid
	// architectures that interleave attention with constant-state layers.
	KVCacheLayers        int32
	KVBytesPerToken      int64
	TrainedContextLength int32
}

// ggufKVArchitectures lists the architectures whose KV cache follows the
// standard attention layout of block_count times kv heads times head size.
// Recurrent and latent-attention families cache differently and must fall
// back to the conservative default rather than get a wrong figure.
var ggufKVArchitectures = map[string]bool{
	"llama": true, "llama4": true,
	"qwen2": true, "qwen2moe": true,
	"qwen3": true, "qwen3moe": true, "qwen35": true,
	"gemma2": true, "gemma3": true,
	"phi3": true, "mistral": true, "mixtral": true,
}

// ggufWantedSuffixes are the metadata keys collected per architecture prefix.
var ggufWantedSuffixes = map[string]bool{
	"block_count":             true,
	"embedding_length":        true,
	"context_length":          true,
	"attention.head_count":    true,
	"attention.head_count_kv": true,
	"attention.key_length":    true,
	"attention.value_length":  true,
	// Hybrid architectures cache only every nth block; the rest carry
	// constant-size recurrent state that does not grow with context.
	"full_attention_interval": true,
}

const (
	ggufMaxMetadataKVs  = 65536
	ggufMaxKeyLength    = 4096
	ggufMaxStringValue  = 4096
	ggufMaxArrayEntries = 4096
	ggufMaxBlockCount   = 4096
	ggufMaxHeadCount    = 1024
	ggufMaxHeadLength   = 65536
	ggufMinKVPerToken   = int64(1 << 10)
	ggufMaxKVPerToken   = int64(8 << 20)
	ggufKVBytesPerValue = int64(2)
)

// GGUF metadata value types.
const (
	ggufTypeUint8   = 0
	ggufTypeInt8    = 1
	ggufTypeUint16  = 2
	ggufTypeInt16   = 3
	ggufTypeUint32  = 4
	ggufTypeInt32   = 5
	ggufTypeFloat32 = 6
	ggufTypeBool    = 7
	ggufTypeString  = 8
	ggufTypeArray   = 9
	ggufTypeUint64  = 10
	ggufTypeInt64   = 11
	ggufTypeFloat64 = 12
)

var ggufScalarSizes = map[uint32]int{
	ggufTypeUint8: 1, ggufTypeInt8: 1, ggufTypeBool: 1,
	ggufTypeUint16: 2, ggufTypeInt16: 2,
	ggufTypeUint32: 4, ggufTypeInt32: 4, ggufTypeFloat32: 4,
	ggufTypeUint64: 8, ggufTypeInt64: 8, ggufTypeFloat64: 8,
}

// ggufScanner is a push parser for the GGUF metadata section. It is fed the
// same byte stream a hashing pass is already reading, consumes only the small
// prefix it needs, skips large payloads such as tokenizer arrays without
// buffering them, and stops asking for input as soon as every wanted key is
// resolved. Any structural surprise fails the scan; a failed scan means no
// profile, never a broken discovery.
type ggufScanner struct {
	pending []byte
	need    int
	skip    int64
	step    func([]byte)
	failed  bool
	done    bool

	kvRemaining uint64
	reason      string
	arch        string
	recurrent   bool
	scalars     map[string]map[string]int64
	arrays      map[string]map[string][]int64

	// per-KV scratch
	currentKey     string
	currentType    uint32
	arrayElemType  uint32
	arrayRemaining uint64
	arrayValues    []int64
	arrayWanted    bool
}

func newGGUFScanner() *ggufScanner {
	scanner := &ggufScanner{
		scalars: make(map[string]map[string]int64),
		arrays:  make(map[string]map[string][]int64),
	}
	scanner.expect(24, scanner.stepHeader)
	return scanner
}

// Feed consumes the next chunk of the file stream. It is safe to keep feeding
// after the scanner has finished or failed; the input is simply ignored.
func (s *ggufScanner) Feed(input []byte) {
	for len(input) > 0 && !s.failed && !s.done {
		if s.skip > 0 {
			consumed := int64(len(input))
			if consumed > s.skip {
				consumed = s.skip
			}
			input = input[consumed:]
			s.skip -= consumed
			continue
		}
		take := s.need - len(s.pending)
		if take > len(input) {
			take = len(input)
		}
		s.pending = append(s.pending, input[:take]...)
		input = input[take:]
		if len(s.pending) < s.need {
			return
		}
		data := s.pending
		s.pending = nil
		step := s.step
		s.step = nil
		step(data)
	}
}

// Profile resolves the collected metadata into a memory profile. ok is false
// whenever anything needed is missing, out of bounds, or not an architecture
// the KV formula is known to describe.
func (s *ggufScanner) Profile() (GGUFMemoryProfile, bool) {
	if s.failed || s.arch == "" || !ggufKVArchitectures[s.arch] {
		return GGUFMemoryProfile{}, false
	}
	scalars := s.scalars[s.arch]
	blockCount, ok := scalars["block_count"]
	if !ok || blockCount < 1 || blockCount > ggufMaxBlockCount {
		return GGUFMemoryProfile{}, false
	}
	contextLength, ok := scalars["context_length"]
	if !ok || contextLength < 1 || contextLength > 1<<24 {
		return GGUFMemoryProfile{}, false
	}
	keyLength, keyOK := scalars["attention.key_length"]
	valueLength, valueOK := scalars["attention.value_length"]
	if !keyOK || !valueOK {
		headCount, headOK := scalars["attention.head_count"]
		embedding, embeddingOK := scalars["embedding_length"]
		if !headOK || !embeddingOK || headCount < 1 || headCount > ggufMaxHeadCount || embedding%headCount != 0 {
			return GGUFMemoryProfile{}, false
		}
		headLength := embedding / headCount
		if !keyOK {
			keyLength = headLength
		}
		if !valueOK {
			valueLength = headLength
		}
	}
	if keyLength < 1 || keyLength > ggufMaxHeadLength || valueLength < 1 || valueLength > ggufMaxHeadLength {
		return GGUFMemoryProfile{}, false
	}
	kvHeads := make([]int64, 0, blockCount)
	if perLayer, ok := s.arrays[s.arch]["attention.head_count_kv"]; ok {
		if int64(len(perLayer)) != blockCount {
			return GGUFMemoryProfile{}, false
		}
		kvHeads = perLayer
	} else {
		uniform, ok := scalars["attention.head_count_kv"]
		if !ok {
			// Without a kv head count the model uses full multi-head attention.
			if uniform, ok = scalars["attention.head_count"]; !ok {
				return GGUFMemoryProfile{}, false
			}
		}
		for layer := int64(0); layer < blockCount; layer++ {
			kvHeads = append(kvHeads, uniform)
		}
	}
	// Hybrid models interleave attention with constant-state layers, so only
	// every nth block holds a growing cache. Without the interval the layout of
	// a recurrent model is unknown and guessing it would understate the cost.
	interval, hasInterval := scalars["full_attention_interval"]
	if s.recurrent && !hasInterval {
		return GGUFMemoryProfile{}, false
	}
	cacheLayers := blockCount
	if hasInterval {
		if interval < 1 || interval > blockCount {
			return GGUFMemoryProfile{}, false
		}
		cacheLayers = blockCount / interval
		if cacheLayers < 1 {
			return GGUFMemoryProfile{}, false
		}
		kvHeads = kvHeads[:cacheLayers]
	}
	var bytesPerToken int64
	for _, heads := range kvHeads {
		if heads < 0 || heads > ggufMaxHeadCount {
			return GGUFMemoryProfile{}, false
		}
		bytesPerToken += heads * (keyLength + valueLength) * ggufKVBytesPerValue
	}
	if bytesPerToken < ggufMinKVPerToken || bytesPerToken > ggufMaxKVPerToken {
		return GGUFMemoryProfile{}, false
	}
	return GGUFMemoryProfile{
		Architecture:         s.arch,
		BlockCount:           int32(blockCount),
		KVCacheLayers:        int32(cacheLayers),
		KVBytesPerToken:      bytesPerToken,
		TrainedContextLength: int32(contextLength),
	}, true
}

func (s *ggufScanner) expect(need int, step func([]byte)) {
	s.need = need
	s.step = step
}

func (s *ggufScanner) fail(format string, args ...any) {
	s.reason = fmt.Sprintf(format, args...)
	s.failed = true
}

func (s *ggufScanner) stepHeader(data []byte) {
	if string(data[:4]) != "GGUF" {
		s.fail("missing GGUF magic")
		return
	}
	version := binary.LittleEndian.Uint32(data[4:8])
	if version != 2 && version != 3 {
		s.fail("unsupported GGUF version %d", version)
		return
	}
	kvCount := binary.LittleEndian.Uint64(data[16:24])
	if kvCount == 0 || kvCount > ggufMaxMetadataKVs {
		s.fail("implausible metadata count %d", kvCount)
		return
	}
	s.kvRemaining = kvCount
	s.expect(8, s.stepKeyLength)
}

func (s *ggufScanner) nextKV() {
	s.kvRemaining--
	if s.kvRemaining == 0 {
		s.done = true
		return
	}
	s.expect(8, s.stepKeyLength)
}

func (s *ggufScanner) stepKeyLength(data []byte) {
	length := binary.LittleEndian.Uint64(data)
	if length == 0 || length > ggufMaxKeyLength {
		s.fail("implausible key length %d", length)
		return
	}
	s.expect(int(length), s.stepKey)
}

func (s *ggufScanner) stepKey(data []byte) {
	s.currentKey = string(data)
	if prefix, rest, found := strings.Cut(s.currentKey, "."); found && prefix != "" && strings.HasPrefix(rest, "ssm.") {
		s.recurrent = true
	}
	s.expect(4, s.stepValueType)
}

func (s *ggufScanner) stepValueType(data []byte) {
	s.currentType = binary.LittleEndian.Uint32(data)
	switch {
	case s.currentType == ggufTypeString:
		s.expect(8, s.stepStringLength)
	case s.currentType == ggufTypeArray:
		s.expect(12, s.stepArrayHeader)
	default:
		size, known := ggufScalarSizes[s.currentType]
		if !known {
			s.fail("unknown metadata type %d", s.currentType)
			return
		}
		if s.wantedScalarKey() {
			s.expect(size, s.stepScalarValue)
			return
		}
		s.skip = int64(size)
		s.nextKV()
	}
}

func (s *ggufScanner) stepStringLength(data []byte) {
	length := binary.LittleEndian.Uint64(data)
	if s.currentKey == "general.architecture" {
		if length == 0 || length > ggufMaxStringValue {
			s.fail("implausible architecture length %d", length)
			return
		}
		s.expect(int(length), s.stepArchitecture)
		return
	}
	s.skip = int64(length)
	s.nextKV()
}

func (s *ggufScanner) stepArchitecture(data []byte) {
	s.arch = string(data)
	s.nextKV()
}

func (s *ggufScanner) stepScalarValue(data []byte) {
	value, ok := ggufScalarToInt64(s.currentType, data)
	if !ok {
		// A float where an integer belongs means the key is not what the
		// formula expects; drop it rather than guess.
		s.nextKV()
		return
	}
	prefix, suffix, ok := splitGGUFKey(s.currentKey)
	if ok {
		if s.scalars[prefix] == nil {
			s.scalars[prefix] = make(map[string]int64)
		}
		s.scalars[prefix][suffix] = value
	}
	s.nextKV()
}

func (s *ggufScanner) stepArrayHeader(data []byte) {
	s.arrayElemType = binary.LittleEndian.Uint32(data[:4])
	s.arrayRemaining = binary.LittleEndian.Uint64(data[4:12])
	size, scalar := ggufScalarSizes[s.arrayElemType]
	_, suffix, wanted := splitGGUFKey(s.currentKey)
	s.arrayWanted = wanted && suffix == "attention.head_count_kv" && scalar
	if s.arrayRemaining == 0 {
		s.nextKV()
		return
	}
	switch {
	case s.arrayWanted:
		if s.arrayRemaining > ggufMaxArrayEntries {
			s.fail("implausible per-layer array of %d entries", s.arrayRemaining)
			return
		}
		s.arrayValues = make([]int64, 0, s.arrayRemaining)
		s.expect(size, s.stepArrayValue)
	case scalar:
		s.skip = int64(s.arrayRemaining) * int64(size)
		s.nextKV()
	case s.arrayElemType == ggufTypeString:
		s.expect(8, s.stepSkippedArrayStringLength)
	default:
		// Nested arrays never carry the keys this scan wants; walking them is
		// not worth the states.
		s.fail("unsupported array element type %d", s.arrayElemType)
	}
}

func (s *ggufScanner) stepArrayValue(data []byte) {
	value, ok := ggufScalarToInt64(s.arrayElemType, data)
	if !ok {
		s.fail("non-integral per-layer head count")
		return
	}
	s.arrayValues = append(s.arrayValues, value)
	s.arrayRemaining--
	if s.arrayRemaining > 0 {
		s.expect(ggufScalarSizes[s.arrayElemType], s.stepArrayValue)
		return
	}
	prefix, suffix, _ := splitGGUFKey(s.currentKey)
	if s.arrays[prefix] == nil {
		s.arrays[prefix] = make(map[string][]int64)
	}
	s.arrays[prefix][suffix] = s.arrayValues
	s.arrayValues = nil
	s.nextKV()
}

func (s *ggufScanner) stepSkippedArrayStringLength(data []byte) {
	s.skip = int64(binary.LittleEndian.Uint64(data))
	s.arrayRemaining--
	if s.arrayRemaining > 0 {
		s.expect(8, s.stepSkippedArrayStringLength)
		return
	}
	s.nextKV()
}

func (s *ggufScanner) wantedScalarKey() bool {
	if _, suffix, ok := splitGGUFKey(s.currentKey); ok {
		return ggufWantedSuffixes[suffix]
	}
	return false
}

// splitGGUFKey separates an architecture-prefixed key such as
// "qwen35.attention.head_count_kv" into its prefix and wanted suffix.
func splitGGUFKey(key string) (string, string, bool) {
	prefix, rest, found := strings.Cut(key, ".")
	if !found || prefix == "" || !ggufWantedSuffixes[rest] {
		return "", "", false
	}
	return prefix, rest, true
}

func ggufScalarToInt64(valueType uint32, data []byte) (int64, bool) {
	switch valueType {
	case ggufTypeUint8:
		return int64(data[0]), true
	case ggufTypeInt8:
		return int64(int8(data[0])), true
	case ggufTypeUint16:
		return int64(binary.LittleEndian.Uint16(data)), true
	case ggufTypeInt16:
		return int64(int16(binary.LittleEndian.Uint16(data))), true
	case ggufTypeUint32:
		return int64(binary.LittleEndian.Uint32(data)), true
	case ggufTypeInt32:
		return int64(int32(binary.LittleEndian.Uint32(data))), true
	case ggufTypeUint64:
		value := binary.LittleEndian.Uint64(data)
		if value > 1<<62 {
			return 0, false
		}
		return int64(value), true
	case ggufTypeInt64:
		return int64(binary.LittleEndian.Uint64(data)), true
	default:
		return 0, false
	}
}
