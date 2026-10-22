package spec_test

import (
	"os"
	"testing"

	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/wasm"
)

func BenchmarkDecode(b *testing.B) {
	module, err := os.ReadFile("testdata/zkvote.wasm")
	if err != nil {
		b.Fatal(err)
	}

	decoder := spec.XDRDecoder{}
	b.ReportAllocs()
	b.SetBytes(int64(len(module)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decoder.Decode(module); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCustomSections(b *testing.B) {
	module, err := os.ReadFile("testdata/zkvote.wasm")
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(module)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := wasm.CustomSectionNames(module); err != nil {
			b.Fatal(err)
		}
	}
}
