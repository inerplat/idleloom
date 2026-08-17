package devruntime

import (
	"encoding/binary"
	"testing"
)

type ggufBuilder struct {
	version uint32
	entries [][]byte
}

func (b *ggufBuilder) bytes() []byte {
	version := b.version
	if version == 0 {
		version = 3
	}
	header := make([]byte, 24)
	copy(header, "GGUF")
	binary.LittleEndian.PutUint32(header[4:], version)
	binary.LittleEndian.PutUint64(header[8:], 0)
	binary.LittleEndian.PutUint64(header[16:], uint64(len(b.entries)))
	out := header
	for _, entry := range b.entries {
		out = append(out, entry...)
	}
	return out
}

func ggufKeyHeader(key string, valueType uint32) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, uint64(len(key)))
	out = append(out, key...)
	return binary.LittleEndian.AppendUint32(out, valueType)
}

func (b *ggufBuilder) addString(key, value string) *ggufBuilder {
	entry := ggufKeyHeader(key, ggufTypeString)
	entry = binary.LittleEndian.AppendUint64(entry, uint64(len(value)))
	b.entries = append(b.entries, append(entry, value...))
	return b
}

func (b *ggufBuilder) addUint32(key string, value uint32) *ggufBuilder {
	entry := ggufKeyHeader(key, ggufTypeUint32)
	b.entries = append(b.entries, binary.LittleEndian.AppendUint32(entry, value))
	return b
}

func (b *ggufBuilder) addUint32Array(key string, values []uint32) *ggufBuilder {
	entry := ggufKeyHeader(key, ggufTypeArray)
	entry = binary.LittleEndian.AppendUint32(entry, ggufTypeUint32)
	entry = binary.LittleEndian.AppendUint64(entry, uint64(len(values)))
	for _, value := range values {
		entry = binary.LittleEndian.AppendUint32(entry, value)
	}
	b.entries = append(b.entries, entry)
	return b
}

func (b *ggufBuilder) addStringArray(key string, values []string) *ggufBuilder {
	entry := ggufKeyHeader(key, ggufTypeArray)
	entry = binary.LittleEndian.AppendUint32(entry, ggufTypeString)
	entry = binary.LittleEndian.AppendUint64(entry, uint64(len(values)))
	for _, value := range values {
		entry = binary.LittleEndian.AppendUint64(entry, uint64(len(value)))
		entry = append(entry, value...)
	}
	b.entries = append(b.entries, entry)
	return b
}

func scanGGUF(t *testing.T, payload []byte, chunk int) *ggufScanner {
	t.Helper()
	scanner := newGGUFScanner()
	for start := 0; start < len(payload); start += chunk {
		end := start + chunk
		if end > len(payload) {
			end = len(payload)
		}
		scanner.Feed(payload[start:end])
	}
	return scanner
}

// qwen35Builder mirrors the metadata a real Qwen3.8 27B GGUF carries: a
// hybrid stack where only every fourth block holds a KV cache and the rest
// keep constant-size recurrent state.
func qwen35Builder() *ggufBuilder {
	builder := &ggufBuilder{}
	builder.addString("general.architecture", "qwen35").
		addUint32("qwen35.block_count", 65).
		addUint32("qwen35.context_length", 262144).
		addUint32("qwen35.embedding_length", 5120).
		addUint32("qwen35.attention.head_count", 24).
		addUint32("qwen35.attention.head_count_kv", 4).
		addUint32("qwen35.attention.key_length", 256).
		addUint32("qwen35.attention.value_length", 256).
		addUint32("qwen35.ssm.state_size", 128).
		addUint32("qwen35.ssm.inner_size", 6144).
		addUint32("qwen35.full_attention_interval", 4)
	return builder
}

func TestGGUFScannerDerivesQwenKVGeometry(t *testing.T) {
	payload := qwen35Builder().bytes()
	// Byte-at-a-time feeding proves every state survives chunk boundaries.
	for _, chunk := range []int{1, 7, 4 << 10} {
		profile, ok := scanGGUF(t, payload, chunk).Profile()
		if !ok {
			t.Fatalf("chunk %d: no profile", chunk)
		}
		// llama.cpp reports 512 MiB of KV cache for 8192 tokens on this model,
		// which is exactly 65536 bytes per token across its 16 caching blocks.
		if profile.KVBytesPerToken != 65536 || profile.BlockCount != 65 || profile.KVCacheLayers != 16 ||
			profile.TrainedContextLength != 262144 || profile.Architecture != "qwen35" {
			t.Fatalf("chunk %d: profile = %+v", chunk, profile)
		}
	}
}

func TestGGUFScannerSumsPerLayerKVHeads(t *testing.T) {
	builder := &ggufBuilder{}
	heads := make([]uint32, 40)
	for layer := range heads {
		heads[layer] = 8
		if layer%2 == 1 {
			heads[layer] = 4
		}
	}
	builder.addString("general.architecture", "llama").
		addUint32("llama.block_count", 40).
		addUint32("llama.context_length", 8192).
		addUint32("llama.embedding_length", 4096).
		addUint32("llama.attention.head_count", 32).
		addUint32Array("llama.attention.head_count_kv", heads)
	profile, ok := scanGGUF(t, builder.bytes(), 5).Profile()
	if !ok {
		t.Fatal("no profile from per-layer head counts")
	}
	// 20 layers at 8 heads and 20 at 4 heads, head size 4096/32=128, K+V, f16.
	want := int64((20*8 + 20*4) * (128 + 128) * 2)
	if profile.KVBytesPerToken != want {
		t.Fatalf("kv bytes per token = %d, want %d", profile.KVBytesPerToken, want)
	}
}

func TestGGUFScannerFallsBackToHeadDimFromEmbedding(t *testing.T) {
	builder := &ggufBuilder{}
	builder.addString("general.architecture", "mistral").
		addUint32("mistral.block_count", 32).
		addUint32("mistral.context_length", 32768).
		addUint32("mistral.embedding_length", 4096).
		addUint32("mistral.attention.head_count", 32).
		addUint32("mistral.attention.head_count_kv", 8)
	profile, ok := scanGGUF(t, builder.bytes(), 4096).Profile()
	if !ok {
		t.Fatal("no profile without explicit key and value lengths")
	}
	if want := int64(32 * 8 * (128 + 128) * 2); profile.KVBytesPerToken != want {
		t.Fatalf("kv bytes per token = %d, want %d", profile.KVBytesPerToken, want)
	}
}

func TestGGUFScannerSkipsTokenizerPayloads(t *testing.T) {
	vocab := make([]string, 5000)
	for index := range vocab {
		vocab[index] = "token-with-some-length"
	}
	builder := &ggufBuilder{}
	// The tokenizer array arrives before any wanted key, so the scanner must
	// stream past it rather than buffer or bail.
	builder.addStringArray("tokenizer.ggml.tokens", vocab)
	builder.addString("general.architecture", "qwen3").
		addUint32("qwen3.block_count", 28).
		addUint32("qwen3.context_length", 32768).
		addUint32("qwen3.embedding_length", 1024).
		addUint32("qwen3.attention.head_count", 16).
		addUint32("qwen3.attention.head_count_kv", 8).
		addUint32("qwen3.attention.key_length", 128).
		addUint32("qwen3.attention.value_length", 128)
	profile, ok := scanGGUF(t, builder.bytes(), 3000).Profile()
	if !ok {
		t.Fatal("tokenizer payload broke the scan")
	}
	if want := int64(28 * 8 * 256 * 2); profile.KVBytesPerToken != want {
		t.Fatalf("kv bytes per token = %d, want %d", profile.KVBytesPerToken, want)
	}
}

func TestGGUFScannerCompletesTheMetadataBlock(t *testing.T) {
	// The scan runs to the end of the metadata rather than stopping at the
	// first sufficient set of keys, because keys that change the answer can
	// trail the ones that look sufficient.
	payload := qwen35Builder().addStringArray("tokenizer.ggml.tokens", []string{"a", "b"}).bytes()
	scanner := scanGGUF(t, payload, 4096)
	if !scanner.done {
		t.Fatal("scanner did not consume the whole metadata block")
	}
	if _, ok := scanner.Profile(); !ok {
		t.Fatal("completed scan lost the profile")
	}
}

func TestGGUFScannerRejectsUnusableInputs(t *testing.T) {
	mamba := &ggufBuilder{}
	mamba.addString("general.architecture", "mamba").
		addUint32("mamba.block_count", 48).
		addUint32("mamba.context_length", 8192).
		addUint32("mamba.embedding_length", 2048).
		addUint32("mamba.attention.head_count", 16).
		addUint32("mamba.attention.head_count_kv", 16)
	hugeKVCount := qwen35Builder().bytes()
	binary.LittleEndian.PutUint64(hugeKVCount[16:], 1<<40)
	v1 := qwen35Builder()
	v1.version = 1
	for name, payload := range map[string][]byte{
		"unknown architecture":     mamba.bytes(),
		"hostile metadata count":   hugeKVCount,
		"unsupported version":      v1.bytes(),
		"truncated header":         qwen35Builder().bytes()[:20],
		"truncated metadata":       qwen35Builder().bytes()[:60],
		"legacy four byte fixture": append([]byte("GGUF"), []byte("pinned-model")...),
	} {
		if _, ok := scanGGUF(t, payload, 9).Profile(); ok {
			t.Fatalf("%s produced a profile", name)
		}
	}
}

func TestGGUFScannerRefusesRecurrentModelWithoutInterval(t *testing.T) {
	builder := &ggufBuilder{}
	builder.addString("general.architecture", "qwen35").
		addUint32("qwen35.block_count", 48).
		addUint32("qwen35.context_length", 32768).
		addUint32("qwen35.embedding_length", 4096).
		addUint32("qwen35.attention.head_count", 32).
		addUint32("qwen35.attention.head_count_kv", 8).
		addUint32("qwen35.ssm.state_size", 128)
	if _, ok := scanGGUF(t, builder.bytes(), 64).Profile(); ok {
		t.Fatal("a recurrent model with an unknown cache layout produced a profile")
	}
}

func TestGGUFScannerReadsKeysAfterTheAttentionBlock(t *testing.T) {
	// full_attention_interval trails the attention keys in real files, so the
	// scan must not stop once the attention keys alone are satisfied.
	builder := &ggufBuilder{}
	builder.addString("general.architecture", "qwen3").
		addUint32("qwen3.block_count", 32).
		addUint32("qwen3.context_length", 32768).
		addUint32("qwen3.embedding_length", 4096).
		addUint32("qwen3.attention.head_count", 32).
		addUint32("qwen3.attention.head_count_kv", 8).
		addUint32("qwen3.attention.key_length", 128).
		addUint32("qwen3.attention.value_length", 128).
		addStringArray("tokenizer.ggml.tokens", []string{"a", "b", "c"}).
		addUint32("qwen3.full_attention_interval", 2)
	profile, ok := scanGGUF(t, builder.bytes(), 16).Profile()
	if !ok {
		t.Fatal("no profile")
	}
	if profile.KVCacheLayers != 16 {
		t.Fatalf("cache layers = %d, want 16 after honouring a trailing interval", profile.KVCacheLayers)
	}
	if want := int64(16 * 8 * 256 * 2); profile.KVBytesPerToken != want {
		t.Fatalf("kv bytes per token = %d, want %d", profile.KVBytesPerToken, want)
	}
}
