package builder

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// scratchLayout is an OCI image layout (as a tar) holding one empty
// wasip1/wasm image: no layer, no command. FROM scratch starts from it,
// so that COPY and RUN steps always have a parent image to run in.
func scratchLayout() ([]byte, string) {
	cfg, _ := json.Marshal(map[string]any{
		"architecture": "wasm",
		"os":           "wasip1",
		"config":       map[string]any{},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{}},
		"history":      []map[string]any{},
	})
	cd := digestOf(cfg)
	man, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)},
		"layers":        []any{},
	})
	md := digestOf(man)
	idx, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    md, "size": len(man),
			"platform": map[string]string{"os": "wasip1", "architecture": "wasm"},
		}},
	})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	put := func(name string, b []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.WriteHeader(&tar.Header{Name: "blobs/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)})
	tw.WriteHeader(&tar.Header{Name: "blobs/sha256/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)})
	put("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	put("index.json", idx)
	put("blobs/sha256/"+cd[len("sha256:"):], cfg)
	put("blobs/sha256/"+md[len("sha256:"):], man)
	tw.Close()
	return buf.Bytes(), cd
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}
