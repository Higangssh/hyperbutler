package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestFullName(t *testing.T) {
	tests := map[string]string{
		"busybox":                         "docker.io/library/busybox",
		"busybox:1":                       "docker.io/library/busybox:1",
		"hyperbutler-test/busybox:latest": "docker.io/hyperbutler-test/busybox:latest",
		"a/b/c":                           "docker.io/a/b/c",
		"localhost/x":                     "localhost/x",
		"reg.io/x:1":                      "reg.io/x:1",
		"reg:5000/x":                      "reg:5000/x",
	}
	for in, want := range tests {
		if got := fullName(in); got != want {
			t.Errorf("fullName(%q) = %q, want %q", in, got, want)
		}
	}
}

func readTar(t *testing.T, b []byte) (map[string][]byte, map[string]*tar.Header) {
	t.Helper()
	files := map[string][]byte{}
	hdrs := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, hdrs
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name], hdrs[h.Name] = body, h
	}
}

// TestBuildImage checks every descriptor against the blob it names, the way docker load
// does, and the layer's contents.
func TestBuildImage(t *testing.T) {
	bin := []byte("\x7fELF fake busybox")
	img, err := buildImage(bin, "hyperbutler-test/busybox:latest")
	if err != nil {
		t.Fatal(err)
	}
	files, _ := readTar(t, img)

	blob := func(d descriptor) []byte {
		t.Helper()
		b, ok := files[blobPath(d.Digest)]
		if !ok {
			t.Fatalf("missing blob %s", d.Digest)
		}
		if digest(b) != d.Digest || len(b) != d.Size {
			t.Fatalf("descriptor %+v does not match its blob (%d bytes, %s)", d, len(b), digest(b))
		}
		return b
	}
	if string(files["oci-layout"]) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Errorf("oci-layout %q", files["oci-layout"])
	}
	var index struct{ Manifests []descriptor }
	if err := json.Unmarshal(files["index.json"], &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("index.json: %v %s", err, files["index.json"])
	}
	if got := index.Manifests[0].Annotations["io.containerd.image.name"]; got != "docker.io/hyperbutler-test/busybox:latest" {
		t.Errorf("image name annotation %q", got)
	}
	var manifest struct {
		Config descriptor
		Layers []descriptor
	}
	if err := json.Unmarshal(blob(index.Manifests[0]), &manifest); err != nil || len(manifest.Layers) != 1 {
		t.Fatalf("manifest: %v", err)
	}
	var config struct {
		Architecture, OS string
		RootFS           struct {
			DiffIDs []string `json:"diff_ids"`
		}
	}
	if err := json.Unmarshal(blob(manifest.Config), &config); err != nil {
		t.Fatal(err)
	}
	layer := blob(manifest.Layers[0])
	if config.Architecture != "amd64" || config.OS != "linux" || len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0] != digest(layer) {
		t.Fatalf("config %+v", config)
	}

	var legacy []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(files["manifest.json"], &legacy); err != nil || len(legacy) != 1 {
		t.Fatalf("manifest.json: %v", err)
	}
	if legacy[0].Config != blobPath(manifest.Config.Digest) || len(legacy[0].Layers) != 1 || legacy[0].Layers[0] != blobPath(manifest.Layers[0].Digest) {
		t.Errorf("manifest.json %+v", legacy[0])
	}

	lf, lh := readTar(t, layer)
	if len(lh) != 3 || lh["bin/"].Typeflag != tar.TypeDir || !bytes.Equal(lf["bin/busybox"], bin) ||
		lh["bin/busybox"].Mode != 0o755 || lh["bin/sh"].Typeflag != tar.TypeSymlink || lh["bin/sh"].Linkname != "busybox" {
		t.Fatalf("layer entries %v", lh)
	}

	again, err := buildImage(bin, "hyperbutler-test/busybox:latest")
	if err != nil || !bytes.Equal(again, img) {
		t.Error("image is not reproducible")
	}
	if _, err := buildImage(bin, ""); err == nil {
		t.Error("accepted an empty tag")
	}
}
