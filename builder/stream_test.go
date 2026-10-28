package builder_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/build/builder"
)

// stamped records when each line of progress output arrived.
type stamped struct {
	mu    sync.Mutex
	buf   strings.Builder
	times map[string]time.Time
}

func (s *stamped) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	for _, l := range strings.Split(string(p), "\n") {
		if _, seen := s.times[l]; l != "" && !seen {
			s.times[l] = time.Now()
		}
	}
	return len(p), nil
}

// TestRunOutputStreams: a step's output reaches the progress writer while
// the step runs, not when it ends.
func TestRunOutputStreams(t *testing.T) {
	e := newEngine(t)
	out := &stamped{times: map[string]time.Time{}}
	const pause = 1500 * time.Millisecond
	df := "FROM scratch\nCOPY tool.wasm /t.wasm\nRUN [\"/t.wasm\", \"slow\", \"1500\"]\n"
	_, err := builder.Build(context.Background(), builder.Options{Containerfile: []byte(df),
		Context: contextOf(t, map[string]string{"tool.wasm": string(tool(t))}), Engine: e.Driver(), Out: out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.buf.String())
	}
	start, okS := out.times["slow: start"]
	end, okE := out.times["slow: end"]
	if !okS || !okE {
		t.Fatalf("output missing:\n%s", out.buf.String())
	}
	// Collected after the exit, both lines would arrive together.
	if gap := end.Sub(start); gap < pause/2 {
		t.Fatalf("the first line arrived only %v before the last: output is not streamed", gap)
	}
}
