//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
	"github.com/Higangssh/hyperbutler/internal/socktable"
)

func main() {
	hb := flag.String("hb", "", "hyperbutler.exe to measure (WebView2Loader.dll next to it)")
	runs := flag.Int("runs", 50, "launches")
	idle := flag.Duration("idle", 5*time.Second, "how long after the first paint to sample")
	lang := flag.String("lang", "", "--lang for the window (en, ko, pseudo)")
	checkLayout := flag.Bool("check-layout", false, "run the window's layout check and fail on overflow (use with -lang pseudo); it shows every screen, so it is also the after-every-screen memory check")
	checkWebRTC := flag.Bool("check-webrtc", false, "have the page try a WebRTC connection (a STUN server on 127.0.0.1:3478) and fail if any process of the window opens a socket meanwhile")
	detail := flag.Bool("detail", false, "print each process's private working set")
	shot := flag.String("shot", "", "save a PNG of the window at the first run's sample")
	shotPID := flag.Int("shot-pid", 0, "only save a PNG of this process's window to -out, and exit")
	out := flag.String("out", "", "with -shot-pid: the PNG to write")
	flag.Parse()
	// Without DPI awareness GetClientRect is scaled and the screenshot cropped.
	_, _, _ = user32.NewProc("SetProcessDpiAwarenessContext").Call(^uintptr(3))

	if *shotPID != 0 {
		hwnd := findWindow(uint32(*shotPID)) //nolint:gosec // G115: a PID from the command line
		if hwnd == 0 {
			fail("no visible window of process %d", *shotPID)
		}
		if err := screenshot(hwnd, *out); err != nil {
			fail("screenshot: %v", err)
		}
		fmt.Println("screenshot", *out)
		return
	}
	if *hb == "" {
		fail("-hb is required")
	}
	args := []string{"gui", "--wait", "--report-paint"}
	if *lang != "" {
		args = append(args, "--lang", *lang)
	}
	if *checkLayout {
		args = append(args, "--check-layout")
		*idle = max(*idle, 20*time.Second) // every screen is shown in turn
	}
	if *checkWebRTC {
		args = append(args, "--probe-webrtc")
		*idle = max(*idle, 25*time.Second) // the probe gathers for 5 s in the page and in a frame
	}
	fi, err := os.Stat(*hb)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("hyperbutler %s (%.1f MiB), HYPERBUTLER_HOME=%s\n", *hb, mib(uint64(fi.Size())), os.Getenv("HYPERBUTLER_HOME")) //nolint:gosec // G115: a file size
	// The very first start of a new executable holds about 30 MiB more private working
	// set in hyperbutler.exe (Windows loading the new image, not the window: the Go heap
	// is the same), which would make run 1 of every fresh build an outlier. One CLI
	// command first takes that.
	if err := exec.Command(*hb, "version").Run(); err != nil { //nolint:gosec // G204: the program the user asked to measure
		fail("%s version: %v", *hb, err)
	}
	fmt.Printf("%-4s %8s %7s %6s %8s %9s %7s %8s %7s  %s\n", "run", "paint_ms", "self_ms", "procs", "ws_MiB", "priv_MiB", "socks", "gone_ms", "left@2s", "processes")
	var all []run
	var layoutFail []string
	for i := 1; i <= *runs; i++ {
		s := ""
		if i == 1 {
			s = *shot
		}
		r, layout, err := once(*hb, args, *idle, s, *checkWebRTC)
		if err != nil {
			fail("run %d: %v", i, err)
		}
		if *detail {
			for _, p := range r.PerProc {
				fmt.Printf("     %s\n", p)
			}
		}
		if *checkWebRTC {
			fmt.Println("     webrtc:", r.Probe)
			for _, s := range r.ProbeSockets {
				fmt.Printf("     socket during the probe: %s\n", s)
			}
			if r.Probe == "" {
				layoutFail = append(layoutFail, fmt.Sprintf("run %d: the WebRTC probe did not report", i))
			}
			if len(r.ProbeSockets) > 0 {
				layoutFail = append(layoutFail, fmt.Sprintf("run %d: %d sockets opened during the WebRTC probe", i, len(r.ProbeSockets)))
			}
		}
		all = append(all, r)
		fmt.Printf("%-4d %8d %7d %6d %8.1f %9.1f %7d %8d %7d  %s\n", i, r.Paint.Milliseconds(), r.SelfMs, r.Procs, mib(r.WS), mib(r.Commit),
			r.Sockets, r.Gone.Milliseconds(), r.LeftAt2s, names(r.Names))
		if r.LeftAt2s > 0 {
			fmt.Printf("     left 2 s after close: %s\n", strings.Join(r.Left, ", "))
		}
		for _, s := range r.SocketList {
			fmt.Printf("     socket: %s\n", s)
		}
		if *checkLayout {
			fmt.Println("     layout:", layout)
			if layout != "ok" {
				layoutFail = append(layoutFail, layout)
			}
		}
		time.Sleep(time.Second)
	}
	fmt.Println()
	summary(os.Stdout, all)
	f := append(verdict(all), layoutFail...)
	if len(f) > 0 {
		fmt.Println("\nFAIL")
		for _, s := range f {
			fmt.Println("  " + s)
		}
		os.Exit(1)
	}
	fmt.Println("\nPASS: every run is within the budgets")
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "footprint: "+format+"\n", a...)
	os.Exit(1)
}

// windowSockets returns the sockets the processes of root hold, by "name proto
// local->remote state".
func windowSockets(root uint32) ([]string, error) {
	ps, err := hostmon.Snapshot()
	if err != nil {
		return nil, err
	}
	desc := hostmon.Descendants(ps, root)
	name := map[uint32]string{}
	for _, p := range ps {
		name[p.PID] = p.Name
	}
	var tr socktable.Reader
	socks, err := tr.All()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range socks {
		if desc[s.PID] {
			out = append(out, fmt.Sprintf("%s(%d) %s %s->%s %s", name[s.PID], s.PID, s.Proto, s.Local, s.Remote, s.State))
		}
	}
	return out, nil
}

// watchProbe polls the socket table of root's processes every 50 ms until the probe
// reports (or timeout), and returns its report and every socket seen meanwhile.
func watchProbe(root uint32, layout <-chan string, timeout time.Duration) (string, []string) {
	seen := map[string]bool{}
	var list []string
	end := time.After(timeout)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case l := <-layout:
			if rest, ok := strings.CutPrefix(l, "webrtc "); ok {
				return rest, list
			}
		case <-end:
			return "", list
		case <-tick.C:
			socks, err := windowSockets(root)
			if err != nil {
				continue
			}
			for _, s := range socks {
				if !seen[s] {
					seen[s] = true
					list = append(list, s)
				}
			}
		}
	}
}

// once launches the window, samples it and closes it.
func once(hb string, args []string, idle time.Duration, shot string, probe bool) (run, string, error) {
	r := run{Names: map[string]int{}}
	cmd := exec.Command(hb, args...) //nolint:gosec // G204: the program the user asked to measure
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return r, "", err
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return r, "", err
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a Windows PID
	painted := make(chan int64, 1)
	layout := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "HB-GUI painted"):
				ms := int64(-1)
				if i := strings.Index(line, "since_create_ms="); i >= 0 {
					ms, _ = strconv.ParseInt(line[i+len("since_create_ms="):], 10, 64)
				}
				painted <- ms
			case strings.HasPrefix(line, "HB-GUI layout "):
				layout <- strings.TrimPrefix(line, "HB-GUI layout ")
			}
		}
	}()
	select {
	case ms := <-painted:
		r.Paint, r.SelfMs = time.Since(t0), ms
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return r, "", errors.New("no first paint within 60 s")
	}
	lay := ""
	deadline := time.After(idle)
	if probe {
		r.Probe, r.ProbeSockets = watchProbe(pid, layout, idle)
	} else if strings.Contains(strings.Join(args, " "), "--check-layout") {
		select {
		case lay = <-layout:
		case <-time.After(idle):
			lay = "no layout result"
		}
	} else {
		<-deadline
	}

	ps, err := hostmon.Snapshot()
	if err != nil {
		return r, lay, err
	}
	desc := hostmon.Descendants(ps, pid)
	var pids []uint32
	byPID := map[uint32]string{}
	for _, p := range ps {
		if desc[p.PID] {
			pids = append(pids, p.PID)
			byPID[p.PID] = p.Name
			r.Names[p.Name]++
		}
	}
	r.Procs = len(pids)
	samples, err := hostmon.SampleNT(context.Background(), pids)
	if err != nil {
		return r, lay, err
	}
	for _, s := range samples {
		r.WS += s.WorkingSetPriv
		r.Commit += s.PrivateBytes
		r.PerProc = append(r.PerProc, fmt.Sprintf("%s(%d) ws %.1f MiB, private %.1f MiB", byPID[s.PID], s.PID, mib(s.WorkingSetPriv), mib(s.PrivateBytes)))
	}
	var tr socktable.Reader
	if socks, err := tr.All(); err == nil {
		for _, s := range socks {
			if desc[s.PID] {
				r.Sockets++
				r.SocketList = append(r.SocketList, fmt.Sprintf("%s %s %s->%s %s", byPID[s.PID], s.Proto, s.Local, s.Remote, s.State))
			}
		}
	}

	hwnd := findWindow(pid)
	if hwnd == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return r, lay, errors.New("no visible window")
	}
	if shot != "" {
		if err := screenshot(hwnd, shot); err != nil {
			fmt.Fprintln(os.Stderr, "footprint: screenshot:", err)
		}
	}
	tc := time.Now()
	_, _, _ = procPostMessageW.Call(hwnd, wmClose, 0, 0)
	r.LeftAt2s = -1
	for {
		ps, err := hostmon.Snapshot()
		if err != nil {
			return r, lay, err
		}
		var left []string
		for _, p := range ps {
			if n, ok := byPID[p.PID]; ok && strings.EqualFold(n, p.Name) {
				left = append(left, fmt.Sprintf("%s(%d)", p.Name, p.PID))
			}
		}
		el := time.Since(tc)
		if el >= budgetGone && r.LeftAt2s < 0 {
			r.LeftAt2s, r.Left = len(left), left
		}
		if len(left) == 0 {
			r.Gone = el
			if r.LeftAt2s < 0 {
				r.LeftAt2s = 0
			}
			break
		}
		if el > 15*time.Second {
			_ = cmd.Process.Kill()
			r.Gone = el
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Wait()
	return r, lay, nil
}

const (
	wmClose            = 0x0010
	pwClientOnly       = 1
	pwRenderFullContnt = 2
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	gdi32                        = windows.NewLazySystemDLL("gdi32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindow                = user32.NewProc("GetWindow")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procGetClientRect            = user32.NewProc("GetClientRect")
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")
	procPrintWindow              = user32.NewProc("PrintWindow")
	procCreateCompatibleDC       = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection         = gdi32.NewProc("CreateDIBSection")
	procSelectObject             = gdi32.NewProc("SelectObject")
	procDeleteDC                 = gdi32.NewProc("DeleteDC")
	procDeleteObject             = gdi32.NewProc("DeleteObject")

	enumPID   uint32
	enumFound uintptr
	enumCB    = windows.NewCallback(func(hwnd, _ uintptr) uintptr {
		var p uint32
		_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&p)))
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		owner, _, _ := procGetWindow.Call(hwnd, 4) // GW_OWNER
		if p == enumPID && vis != 0 && owner == 0 {
			enumFound = hwnd
			return 0
		}
		return 1
	})
)

// findWindow returns pid's visible, unowned top-level window.
func findWindow(pid uint32) uintptr {
	enumPID, enumFound = pid, 0
	_, _, _ = procEnumWindows.Call(enumCB, 0)
	return enumFound
}

// screenshot saves hwnd's client area as a PNG. PW_RENDERFULLCONTENT makes PrintWindow
// include DirectComposition content such as WebView2's.
func screenshot(hwnd uintptr, path string) error {
	var rc struct{ L, T, R, B int32 }
	_, _, _ = procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	w, h := int(rc.R-rc.L), int(rc.B-rc.T)
	if w <= 0 || h <= 0 {
		return errors.New("empty client area")
	}
	screen, _, _ := procGetDC.Call(0)
	defer func() { _, _, _ = procReleaseDC.Call(0, screen) }()
	mem, _, _ := procCreateCompatibleDC.Call(screen)
	defer func() { _, _, _ = procDeleteDC.Call(mem) }()
	bi := struct {
		Size                   uint32
		Width, Height          int32
		Planes, BitCount       uint16
		Compression, SizeImage uint32
		XPels, YPels           int32
		ClrUsed, ClrImportant  uint32
	}{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32} //nolint:gosec // G115: a window's size
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	bmp, _, _ := procCreateDIBSection.Call(mem, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 {
		return errors.New("CreateDIBSection failed")
	}
	defer func() { _, _, _ = procDeleteObject.Call(bmp) }()
	old, _, _ := procSelectObject.Call(mem, bmp)
	ok, _, _ := procPrintWindow.Call(hwnd, mem, pwClientOnly|pwRenderFullContnt)
	_, _, _ = procSelectObject.Call(mem, old)
	if ok == 0 {
		return errors.New("PrintWindow failed")
	}
	px := unsafe.Slice((*byte)(bits), w*h*4)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		img.Set(i%w, i/w, color.RGBA{R: px[i*4+2], G: px[i*4+1], B: px[i*4], A: 255})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.Create(path) //nolint:gosec // G304: the path the user passed
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
