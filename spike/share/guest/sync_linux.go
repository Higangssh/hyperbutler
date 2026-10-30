package main

import (
	"archive/tar"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Guest side of the sync option (d): the host pushes file contents into guest ext4.

func syncPath(root, rel string) (string, error) {
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." {
			return "", fmt.Errorf("bad path %q", rel)
		}
	}
	return filepath.Join(root, rel), nil
}

func init() {
	extraOps["put"] = func(a map[string]string) (any, error) {
		p, err := syncPath(a["root"], a["path"])
		if err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(a["data"])
		if err != nil {
			return nil, err
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		return nil, os.WriteFile(p, data, 0o644)
	}
	extraOps["del"] = func(a map[string]string) (any, error) {
		p, err := syncPath(a["root"], a["path"])
		if err != nil {
			return nil, err
		}
		return nil, os.RemoveAll(p)
	}
	extraOps["mv"] = func(a map[string]string) (any, error) {
		p, err := syncPath(a["root"], a["path"])
		if err != nil {
			return nil, err
		}
		q, err := syncPath(a["root"], a["to"])
		if err != nil {
			return nil, err
		}
		return nil, os.Rename(p, q)
	}
	// recvtar extracts a tar stream from the host into root (initial sync).
	extraOps["recvtar"] = func(a map[string]string) (any, error) {
		var port uint32
		fmt.Sscan(a["port"], &port)
		c, err := dialVsock(port, 5*time.Second)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		t := time.Now()
		tr := tar.NewReader(c)
		n := 0
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			p, err := syncPath(a["root"], strings.TrimSuffix(h.Name, "/"))
			if err != nil {
				return nil, err
			}
			if h.Typeflag == tar.TypeDir {
				os.MkdirAll(p, 0o755)
				continue
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			f, err := os.Create(p)
			if err != nil {
				return nil, err
			}
			io.Copy(f, tr)
			f.Close()
			n++
		}
		return map[string]float64{"sync_s": time.Since(t).Seconds(), "files": float64(n)}, nil
	}
}
