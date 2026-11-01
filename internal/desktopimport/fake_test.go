package desktopimport

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
)

// fakeDocker is a Docker API with an in-memory store of images, volumes and
// containers: enough of dockerd for the importer. One plays Docker Desktop, another
// hyperbutler.
type fakeDocker struct {
	t   *testing.T
	srv *httptest.Server
	id  string

	mu         sync.Mutex
	requests   []string // "METHOD /path"
	images     map[string]*fImage
	volumes    map[string]*fVolume
	containers map[string]*fContainer

	// Hooks.
	failLoad    string                // the load answer reports this error (after tagging)
	corruptPut  bool                  // PUT archive flips a byte of the first file
	onArchive   func(f *fakeDocker)   // runs (unlocked) before a GET archive answers
	onLoadRead  func()                // runs after the load body is read
	exportBytes int                   // padding of an image export
	mutateRead  func(v *fVolume) bool // runs before each GET archive, under the lock
	blockPut    chan struct{}         // PUT archive waits for it to close
	otherLoadID bool                  // a load stores the image under another id
}

type fImage struct {
	ID      string   `json:"Id"`
	Tags    []string `json:"RepoTags"`
	Size    int64    `json:"Size"`
	Created string   `json:"Created"`
	Layers  []string `json:"Layers"`
}

type fFile struct {
	Name    string // under the volume root
	Dir     bool
	Mode    int64
	Uid     int
	Content string
}

type fVolume struct {
	Name   string
	Labels map[string]string
	Files  []fFile
	Driver string
	Opts   map[string]string
}

type fContainer struct {
	ID, Name, State, Image string
	Labels                 map[string]string
	Volume                 string
	ReadOnly               bool
	Entrypoint             []string
	Started                bool
}

func newFake(t *testing.T, id string) *fakeDocker {
	f := &fakeDocker{t: t, id: id, images: map[string]*fImage{}, volumes: map[string]*fVolume{}, containers: map[string]*fContainer{}, exportBytes: 3000}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func randID() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (f *fakeDocker) addImage(tags ...string) *fImage {
	id := "sha256:" + randID()
	im := &fImage{ID: id, Tags: tags, Size: 1000 + int64(len(f.images)), Created: "2026-10-01T00:00:00Z", Layers: []string{"sha256:" + randID()}}
	f.images[id] = im
	return im
}

func (f *fakeDocker) addVolume(name string, files ...fFile) *fVolume {
	v := &fVolume{Name: name, Driver: "local", Labels: map[string]string{"com.docker.compose.volume": name}, Files: files}
	f.volumes[name] = v
	return v
}

func (f *fakeDocker) addContainer(name, state, volume string) *fContainer {
	c := &fContainer{ID: randID(), Name: name, State: state, Volume: volume, Image: "sha256:x", Labels: map[string]string{}}
	f.containers[c.ID] = c
	return c
}

func (f *fakeDocker) reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeDocker) helperCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.containers {
		if c.Labels[source.HelperLabel] == source.HelperLabelValue {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func msg(w http.ResponseWriter, code int, m string) {
	writeJSON(w, code, map[string]string{"message": m})
}

// findImage finds an image by id or tag; the caller holds f.mu.
func (f *fakeDocker) findImage(ref string) *fImage {
	if im, ok := f.images[ref]; ok {
		return im
	}
	for _, im := range f.images {
		if slices.Contains(im.Tags, ref) || slices.Contains(im.Tags, ref+":latest") {
			return im
		}
	}
	return nil
}

func (f *fakeDocker) removeTag(tag string) {
	for id, im := range f.images {
		if i := slices.Index(im.Tags, tag); i >= 0 {
			im.Tags = slices.Delete(im.Tags, i, i+1)
			if len(im.Tags) == 0 {
				delete(f.images, id)
			}
		}
	}
}

func (v *fVolume) archive() []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "v/", Typeflag: tar.TypeDir, Mode: 0o755})
	files := slices.Clone(v.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, fl := range files {
		h := &tar.Header{Name: "v/" + fl.Name, Mode: fl.Mode, Uid: fl.Uid}
		if fl.Dir {
			h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
		} else {
			h.Typeflag, h.Size = tar.TypeReg, int64(len(fl.Content))
		}
		_ = tw.WriteHeader(h)
		_, _ = tw.Write([]byte(fl.Content))
	}
	_ = tw.Close()
	return b.Bytes()
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+path)
	f.mu.Unlock()
	q := r.URL.Query()
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "/version":
		writeJSON(w, 200, map[string]string{"Version": "29.0.0"})
	case r.Method == http.MethodGet && path == "/info":
		writeJSON(w, 200, map[string]any{"ID": f.id, "DriverStatus": [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}})
	case r.Method == http.MethodGet && path == "/images/json":
		f.mu.Lock()
		list := []fImage{}
		for _, im := range f.images {
			list = append(list, *im)
		}
		f.mu.Unlock()
		writeJSON(w, 200, list)
	case r.Method == http.MethodGet && segs[0] == "images" && segs[len(segs)-1] == "json" && len(segs) >= 3:
		f.mu.Lock()
		im := f.findImage(strings.Join(segs[1:len(segs)-1], "/"))
		var out map[string]any
		if im != nil {
			out = map[string]any{"Id": im.ID, "RepoTags": im.Tags, "Created": im.Created, "Architecture": "amd64", "Os": "linux", "RootFS": map[string]any{"Layers": im.Layers}}
		}
		f.mu.Unlock()
		if out == nil {
			msg(w, 404, "No such image")
			return
		}
		writeJSON(w, 200, out)
	case r.Method == http.MethodGet && path == "/images/get":
		f.mu.Lock()
		var ims []fImage
		for _, n := range q["names"] {
			if im := f.findImage(n); im != nil {
				c := *im
				c.Tags = []string{n}
				ims = append(ims, c)
			}
		}
		pad := f.exportBytes
		f.mu.Unlock()
		b, _ := json.Marshal(ims)
		_, _ = w.Write(append(append(b, '\n'), bytes.Repeat([]byte("x"), pad)...))
	case r.Method == http.MethodPost && path == "/images/load":
		line, err := bufio.NewReader(r.Body).ReadBytes('\n')
		_, _ = io.Copy(io.Discard, r.Body)
		if f.onLoadRead != nil {
			f.onLoadRead()
		}
		var ims []fImage
		if err != nil || json.Unmarshal(line, &ims) != nil {
			msg(w, 400, "bad archive")
			return
		}
		f.mu.Lock()
		for _, in := range ims {
			for _, t := range in.Tags {
				f.removeTag(t)
			}
			if f.otherLoadID {
				in.ID = "sha256:" + randID()
			}
			im := f.images[in.ID]
			if im == nil {
				c := in
				c.Tags = nil
				im = &c
				f.images[in.ID] = im
			}
			im.Tags = append(im.Tags, in.Tags...)
		}
		fail := f.failLoad
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail != "" {
			_, _ = fmt.Fprintf(w, `{"errorDetail":{"message":%q},"error":%q}`+"\n", fail, fail)
			return
		}
		for _, in := range ims {
			for _, t := range in.Tags {
				_, _ = fmt.Fprintf(w, `{"stream":"Loaded image: %s\n"}`+"\n", t)
			}
		}
	case r.Method == http.MethodPost && len(segs) == 3 && segs[0] == "images" && segs[2] == "tag":
		f.mu.Lock()
		defer f.mu.Unlock()
		im := f.images[segs[1]]
		if im == nil {
			msg(w, 404, "No such image")
			return
		}
		t := q.Get("repo") + ":" + q.Get("tag")
		f.removeTag(t)
		im.Tags = append(im.Tags, t)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete && segs[0] == "images":
		f.mu.Lock()
		defer f.mu.Unlock()
		ref := strings.Join(segs[1:], "/")
		if im, ok := f.images[ref]; ok {
			delete(f.images, im.ID)
		} else if f.findImage(ref) != nil {
			f.removeTag(ref)
		} else {
			msg(w, 404, "No such image")
			return
		}
		writeJSON(w, 200, []any{})
	case r.Method == http.MethodGet && path == "/volumes":
		f.mu.Lock()
		var list []map[string]any
		for _, v := range f.volumes {
			list = append(list, map[string]any{"Name": v.Name, "Driver": v.Driver, "Labels": v.Labels, "Options": v.Opts})
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"Volumes": list})
	case r.Method == http.MethodGet && path == "/system/df":
		f.mu.Lock()
		var list []map[string]any
		for _, v := range f.volumes {
			list = append(list, map[string]any{"Name": v.Name, "UsageData": map[string]int64{"Size": int64(len(v.archive()))}})
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"Volumes": list})
	case r.Method == http.MethodGet && len(segs) == 2 && segs[0] == "volumes":
		f.mu.Lock()
		v := f.volumes[segs[1]]
		f.mu.Unlock()
		if v == nil {
			msg(w, 404, "no such volume")
			return
		}
		writeJSON(w, 200, map[string]any{"Name": v.Name, "Labels": v.Labels})
	case r.Method == http.MethodPost && path == "/volumes/create":
		var in struct {
			Name   string
			Labels map[string]string
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.volumes[in.Name] = &fVolume{Name: in.Name, Driver: "local", Labels: in.Labels}
		f.mu.Unlock()
		writeJSON(w, 201, map[string]string{"Name": in.Name})
	case r.Method == http.MethodDelete && len(segs) == 2 && segs[0] == "volumes":
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.containers {
			if c.Volume == segs[1] {
				msg(w, 409, "volume is in use")
				return
			}
		}
		delete(f.volumes, segs[1])
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/containers/json":
		f.mu.Lock()
		var list []map[string]any
		for _, c := range f.containers {
			if q.Get("all") != "1" && c.State != "running" {
				continue
			}
			if fl := q.Get("filters"); fl != "" && !strings.Contains(fl, source.HelperLabel) {
				continue
			}
			if q.Get("filters") != "" && c.Labels[source.HelperLabel] != source.HelperLabelValue {
				continue
			}
			var mounts []map[string]string
			if c.Volume != "" {
				mounts = append(mounts, map[string]string{"Type": "volume", "Name": c.Volume})
			}
			list = append(list, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "State": c.State, "ImageID": c.Image, "Labels": c.Labels, "Mounts": mounts})
		}
		f.mu.Unlock()
		writeJSON(w, 200, list)
	case r.Method == http.MethodPost && path == "/containers/create":
		var in struct {
			Image      string
			Entrypoint []string
			Labels     map[string]string
			HostConfig struct {
				Mounts []struct {
					Type, Source, Target string
					ReadOnly             bool
				}
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.findImage(in.Image) == nil {
			msg(w, 404, "No such image: "+in.Image)
			return
		}
		c := &fContainer{ID: randID(), Name: q.Get("name"), State: "created", Image: in.Image, Labels: in.Labels, Entrypoint: in.Entrypoint}
		for _, m := range in.HostConfig.Mounts {
			c.Volume, c.ReadOnly = m.Source, m.ReadOnly
			if f.volumes[m.Source] == nil {
				f.volumes[m.Source] = &fVolume{Name: m.Source, Driver: "local"} // as dockerd does
			}
		}
		f.containers[c.ID] = c
		writeJSON(w, 201, map[string]string{"Id": c.ID})
	case r.Method == http.MethodGet && len(segs) == 3 && segs[0] == "containers" && segs[2] == "json":
		f.mu.Lock()
		c := f.containers[segs[1]]
		f.mu.Unlock()
		if c == nil {
			msg(w, 404, "No such container")
			return
		}
		started := "0001-01-01T00:00:00Z"
		if c.Started {
			started = "2026-10-01T00:00:00Z"
		}
		writeJSON(w, 200, map[string]any{"Config": map[string]any{"Labels": c.Labels, "Entrypoint": c.Entrypoint}, "State": map[string]any{"Status": c.State, "StartedAt": started}})
	case r.Method == http.MethodGet && len(segs) == 3 && segs[0] == "containers" && segs[2] == "archive":
		if f.onArchive != nil {
			f.onArchive(f)
		}
		f.mu.Lock()
		c := f.containers[segs[1]]
		var b []byte
		if c != nil && f.volumes[c.Volume] != nil {
			v := f.volumes[c.Volume]
			if f.mutateRead != nil && f.mutateRead(v) {
				f.mutateRead = nil
			}
			b = v.archive()
		}
		f.mu.Unlock()
		if b == nil {
			msg(w, 404, "No such container")
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(b)
	case r.Method == http.MethodPut && len(segs) == 3 && segs[0] == "containers" && segs[2] == "archive":
		if f.blockPut != nil {
			select {
			case <-f.blockPut:
			case <-r.Context().Done():
				return
			}
		}
		tr := tar.NewReader(r.Body)
		var files []fFile
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				msg(w, 400, err.Error())
				return
			}
			name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "v/"), "/")
			if name == "v" || name == "" {
				continue
			}
			b, _ := io.ReadAll(tr)
			files = append(files, fFile{Name: name, Dir: h.Typeflag == tar.TypeDir, Mode: h.Mode, Uid: h.Uid, Content: string(b)})
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.containers[segs[1]]
		if c == nil || f.volumes[c.Volume] == nil {
			msg(w, 404, "No such container")
			return
		}
		if c.ReadOnly {
			msg(w, 403, "read-only")
			return
		}
		if f.corruptPut {
			for i := range files {
				if !files[i].Dir && files[i].Content != "" {
					files[i].Content = "X" + files[i].Content[1:]
					break
				}
			}
		}
		f.volumes[c.Volume].Files = files
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete && len(segs) == 2 && segs[0] == "containers":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.containers[segs[1]] == nil {
			msg(w, 404, "No such container")
			return
		}
		delete(f.containers, segs[1])
		w.WriteHeader(http.StatusNoContent)
	default:
		msg(w, 404, "page not found")
	}
}

// pair is a fake Docker Desktop and a fake hyperbutler, with Options that reach them.
type pair struct {
	src, dst *fakeDocker
	o        Options
}

const (
	srcPipe = `\\.\pipe\fakeDesktop`
	dstPipe = `\\.\pipe\fakeHyperbutler`
)

func newPair(t *testing.T) *pair {
	p := &pair{src: newFake(t, "desktop"), dst: newFake(t, "hyperbutler")}
	p.o = Options{
		SourcePipe: srcPipe, TargetPipe: dstPipe,
		Dial: func(ctx context.Context, pipe string) (net.Conn, error) {
			var d net.Dialer
			switch pipe {
			case srcPipe:
				if p.src == nil {
					return nil, errors.New("the system cannot find the file specified")
				}
				return d.DialContext(ctx, "tcp", p.src.srv.Listener.Addr().String())
			case dstPipe:
				return d.DialContext(ctx, "tcp", p.dst.srv.Listener.Addr().String())
			}
			return nil, errors.New("the system cannot find the file specified")
		},
	}
	return p
}

func (p *pair) open(t *testing.T) *Importer {
	t.Helper()
	im, err := Open(context.Background(), p.o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(im.Close)
	return im
}
