// Command mkinitramfs builds a gzip-compressed newc initramfs from a spec file, on any OS
// Go runs on (in particular on Windows without WSL). It can also list an existing
// archive. See package github.com/Higangssh/winbox/guest/cpio for the spec format.
//
//	mkinitramfs -spec initramfs.spec -base DIR -D NAME=path ... -o out.cpio.gz
//	mkinitramfs -list initramfs-virt
//
// Output is deterministic for a given spec, input files, -mtime, and Go version.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Higangssh/winbox/guest/cpio"
)

type vars map[string]string

func (v vars) String() string { return "" }
func (v vars) Set(s string) error {
	k, val, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return errors.New("want NAME=value")
	}
	v[k] = val
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mkinitramfs:", err)
		os.Exit(1)
	}
}

func run() error {
	vs := vars{}
	specPath := flag.String("spec", "", "spec file to build")
	base := flag.String("base", ".", "directory that relative sources resolve against")
	out := flag.String("o", "", "output file")
	list := flag.String("list", "", "list the entries of this archive (gzip or plain) and exit")
	level := flag.Int("level", gzip.BestCompression, "gzip level (0 = store uncompressed cpio)")
	mtimeFlag := flag.String("mtime", "", "mtime for every entry (default: $SOURCE_DATE_EPOCH, else 0)")
	flag.Var(vs, "D", "define NAME=value for ${NAME} in the spec (repeatable)")
	flag.Parse()

	if *list != "" {
		return listArchive(*list)
	}
	if *specPath == "" || *out == "" {
		flag.Usage()
		return errors.New("-spec and -o are required")
	}
	mt := *mtimeFlag
	if mt == "" {
		mt = os.Getenv("SOURCE_DATE_EPOCH")
	}
	var mtime uint64
	if mt != "" {
		var err error
		if mtime, err = strconv.ParseUint(mt, 10, 32); err != nil {
			return fmt.Errorf("bad mtime %q", mt)
		}
	}
	sf, err := os.Open(*specPath)
	if err != nil {
		return err
	}
	spec, err := cpio.ParseSpec(sf, vs)
	sf.Close()
	if err != nil {
		return err
	}

	tmp := *out + ".tmp"
	of, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	bw := bufio.NewWriterSize(of, 1<<20)
	var sink io.Writer = bw
	var gz *gzip.Writer
	if *level != 0 {
		// The zero gzip header (no name, no mtime) keeps the output reproducible.
		if gz, err = gzip.NewWriterLevel(bw, *level); err != nil {
			return err
		}
		sink = gz
	}
	w := cpio.NewWriter(sink)
	if err := spec.Build(w, *base, uint32(mtime)); err != nil {
		of.Close()
		return err
	}
	if err := w.Close(); err != nil {
		of.Close()
		return err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			of.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		of.Close()
		return err
	}
	if err := of.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, *out)
}

func listArchive(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, _ := br.Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		r = gz
	}
	cr := cpio.NewReader(r)
	for {
		h, err := cr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("%06o %4d %4d %10d %s\n", h.Mode, h.UID, h.GID, h.Size, h.Name)
	}
}
