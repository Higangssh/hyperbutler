package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func mkimageCmd(args []string) error {
	fs := flag.NewFlagSet("mkimage", flag.ContinueOnError)
	busybox := fs.String("busybox", "", "static busybox binary for linux/amd64 (required)")
	out := fs.String("o", "busybox-image.tar", "output tar for `docker load`")
	tag := fs.String("tag", "hyperbutler-test/busybox:latest", "image name:tag")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *busybox == "" || fs.NArg() != 0 {
		fs.Usage()
		return errors.New("mkimage: -busybox is required")
	}
	bin, err := os.ReadFile(*busybox)
	if err != nil {
		return err
	}
	img, err := buildImage(bin, *tag)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, img, 0o600)
}

// epoch is the fixed mtime of every entry, so the image is reproducible.
var epoch = time.Unix(0, 0).UTC()

// buildImage returns a `docker load` archive in the layout `docker save` writes since
// Docker 25: an OCI image layout (oci-layout, index.json, blobs/sha256/...) plus the
// legacy manifest.json, so both the classic and the containerd image store accept it.
// The image is FROM scratch with /bin/busybox and /bin/sh -> busybox.
func buildImage(busybox []byte, tag string) ([]byte, error) {
	if tag == "" {
		return nil, errors.New("mkimage: empty tag")
	}
	layer, err := tarball([]entry{
		{name: "bin/", mode: 0o755, typ: tar.TypeDir},
		{name: "bin/busybox", mode: 0o755, typ: tar.TypeReg, body: busybox},
		{name: "bin/sh", mode: 0o777, typ: tar.TypeSymlink, link: "busybox"},
	})
	if err != nil {
		return nil, err
	}
	layerDigest := digest(layer)

	config, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      epoch.Format(time.RFC3339),
		"config": map[string]any{
			"Env": []string{"PATH=/bin"},
			"Cmd": []string{"/bin/sh", "-c", "echo hello from hyperbutler"},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{layerDigest}},
		"history": []map[string]any{{
			"created":    epoch.Format(time.RFC3339),
			"created_by": "hyperbutler-dev mkimage",
		}},
	})
	if err != nil {
		return nil, err
	}
	configDigest := digest(config)

	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: len(config),
		},
		"layers": []descriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: len(layer),
		}},
	})
	if err != nil {
		return nil, err
	}
	manifestDigest := digest(manifest)

	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []descriptor{{
			MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest, Size: len(manifest),
			Annotations: map[string]string{"io.containerd.image.name": fullName(tag)},
		}},
	})
	if err != nil {
		return nil, err
	}
	legacy, err := json.Marshal([]map[string]any{{
		"Config":   blobPath(configDigest),
		"RepoTags": []string{tag},
		"Layers":   []string{blobPath(layerDigest)},
	}})
	if err != nil {
		return nil, err
	}

	return tarball([]entry{
		{name: "blobs/", mode: 0o755, typ: tar.TypeDir},
		{name: "blobs/sha256/", mode: 0o755, typ: tar.TypeDir},
		{name: blobPath(layerDigest), mode: 0o644, typ: tar.TypeReg, body: layer},
		{name: blobPath(configDigest), mode: 0o644, typ: tar.TypeReg, body: config},
		{name: blobPath(manifestDigest), mode: 0o644, typ: tar.TypeReg, body: manifest},
		{name: "index.json", mode: 0o644, typ: tar.TypeReg, body: index},
		{name: "manifest.json", mode: 0o644, typ: tar.TypeReg, body: legacy},
		{name: "oci-layout", mode: 0o644, typ: tar.TypeReg, body: []byte(`{"imageLayoutVersion":"1.0.0"}`)},
	})
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int               `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type entry struct {
	name string
	mode int64
	typ  byte
	body []byte
	link string
}

func tarball(entries []entry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{
			Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link,
			Size: int64(len(e.body)), ModTime: epoch, Format: tar.FormatPAX,
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, bytes.NewReader(e.body)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fullName adds the docker.io domain that the containerd image store expects in
// io.containerd.image.name, the way `docker save` writes it.
func fullName(tag string) string {
	first, _, found := strings.Cut(tag, "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return tag
	}
	if !found {
		return "docker.io/library/" + tag
	}
	return "docker.io/" + tag
}

func blobPath(d string) string { return fmt.Sprintf("blobs/sha256/%s", d[len("sha256:"):]) }
