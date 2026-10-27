package builder_test

import (
	"context"
	"testing"

	"github.com/go-wasmcontainers/build/builder"
)

const benchFile = `FROM scratch
COPY tool.wasm /bin/tool.wasm
ENV A=1
WORKDIR /w
RUN ["/bin/tool.wasm", "write", "/w/out", "x"]
COPY data/ /data/
CMD ["/bin/tool.wasm", "cat", "/w/out"]
`

// BenchmarkBuildCold builds with an empty cache each time (the module's
// first compile is excluded: the engine caches compiled modules).
func BenchmarkBuildCold(b *testing.B) {
	e := newEngine(b)
	files := map[string]string{"tool.wasm": string(tool(b)), "data/a": "a", "data/b": "b"}
	ctx := context.Background()
	if _, err := builder.Build(ctx, builder.Options{Containerfile: []byte(benchFile), Context: contextOf(b, files), Engine: e.Driver()}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := builder.Build(ctx, builder.Options{Containerfile: []byte(benchFile), Context: contextOf(b, files), Engine: e.Driver(), NoCache: true}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuildCached rebuilds an unchanged Containerfile: every step is
// a cache hit (one image inspect each).
func BenchmarkBuildCached(b *testing.B) {
	e := newEngine(b)
	files := map[string]string{"tool.wasm": string(tool(b)), "data/a": "a", "data/b": "b"}
	ctx := context.Background()
	cache := builder.NewMemoryCache(0)
	if _, err := builder.Build(ctx, builder.Options{Containerfile: []byte(benchFile), Context: contextOf(b, files), Engine: e.Driver(), Cache: cache}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := builder.Build(ctx, builder.Options{Containerfile: []byte(benchFile), Context: contextOf(b, files), Engine: e.Driver(), Cache: cache})
		if err != nil {
			b.Fatal(err)
		}
		if res.Cached != 6 {
			b.Fatalf("%d cached steps, want 6", res.Cached)
		}
	}
}
