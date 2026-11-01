//go:build windows

// Command measure runs a GUI spike (spike/gui/webview2 or spike/gui/native) several
// times and reports what ADR 0008 asks the toolkit spike for: the private working set
// and commit of every process the GUI creates, idle 10 s after the window shows; the
// process count; the time from launch to first paint; and whether any process is left
// 2 s after the window closes (the REQ-UX-011 / bench/footprint check).
//
// The processes are the GUI and all its descendants (WebView2's msedgewebview2.exe
// children), read in process with NtQuerySystemInformation (bench/internal/hostmon).
// Processes that appeared during the run but are not descendants are listed too, so a
// helper started some other way is not missed.
//
//	measure -app %TEMP%\hb-dev\gui-spike\bin\native.exe -runs 5 -cold
//	measure -app ...\webview2.exe -args "-data %TEMP%\hb-dev\gui-spike\wv2-{run}" -cold
//	measure -app ...\native.exe -runs 1 -shot %TEMP%\hb-dev\gui-spike\native.png
//
// -cold copies the program to a new file before every run and gives {run} in -args a
// new value, so the image and (for WebView2) the user data folder are new. It does not
// empty the Windows file cache, which would need administrator tools.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type result struct {
	paint      time.Duration // launch to "HB-SPIKE painted"
	selfMs     int           // the GUI's own since-creation figure
	procs      int
	ws, commit uint64
	names      map[string]int
	strays     []string // new processes that are not descendants
	gone       time.Duration
	leftAt2s   int
	left       map[string]int // what was left at 2 s
	hostGone   time.Duration  // close to the GUI process itself exiting
}

func main() {
	app := flag.String("app", "", "GUI spike executable")
	args := flag.String("args", "", "arguments for the GUI, space separated; {run} is replaced by a per-run value")
	runs := flag.Int("runs", 5, "runs")
	idle := flag.Duration("idle", 10*time.Second, "how long to wait after first paint before sampling")
	cold := flag.Bool("cold", false, "copy the program to a new file and use a new {run} value for every run")
	shot := flag.String("shot", "", "save a PNG of the window's client area on the first run")
	dir := flag.String("dir", ".", "working directory for the GUI")
	flag.Parse()
	// Without DPI awareness GetClientRect is scaled and the screenshot is cropped.
	_, _, _ = user32.NewProc("SetProcessDpiAwarenessContext").Call(^uintptr(3)) // PER_MONITOR_AWARE_V2
	if *app == "" {
		fmt.Fprintln(os.Stderr, "measure: -app is required")
		os.Exit(2)
	}
	fi, err := os.Stat(*app)
	if err != nil {
		fmt.Fprintln(os.Stderr, "measure:", err)
		os.Exit(1)
	}
	fmt.Printf("app %s (%.2f MiB)\n", *app, float64(fi.Size())/(1<<20))
	fmt.Printf("%-4s %9s %8s %6s %10s %10s %9s %8s  %s\n", "run", "paint_ms", "self_ms", "procs", "ws_MiB", "commit_MiB", "gone_ms", "left@2s", "processes")
	var all []result
	for i := 1; i <= *runs; i++ {
		tag := "warm"
		if *cold {
			tag = strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		r, err := runOnce(*app, *args, tag, *dir, *cold, *idle, map[bool]string{true: *shot, false: ""}[i == 1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "measure: run %d: %v\n", i, err)
			os.Exit(1)
		}
		all = append(all, r)
		fmt.Printf("%-4d %9d %8d %6d %10.1f %10.1f %9d %8d  %s\n", i, r.paint.Milliseconds(), r.selfMs, r.procs,
			mib(r.ws), mib(r.commit), r.gone.Milliseconds(), r.leftAt2s, names(r.names))
		if r.leftAt2s > 0 {
			fmt.Printf("     left 2 s after close: %s (the GUI process itself exited after %d ms)\n", names(r.left), r.hostGone.Milliseconds())
		}
		if len(r.strays) > 0 {
			fmt.Printf("     new non-descendant processes: %s\n", strings.Join(r.strays, ", "))
		}
		time.Sleep(2 * time.Second)
	}
	med := func(f func(result) float64) float64 {
		xs := make([]float64, len(all))
		for i, r := range all {
			xs[i] = f(r)
		}
		return median(xs)
	}
	fmt.Printf("%-4s %9.0f %8.0f %6.0f %10.1f %10.1f %9.0f %8.0f\n", "med",
		med(func(r result) float64 { return float64(r.paint.Milliseconds()) }),
		med(func(r result) float64 { return float64(r.selfMs) }),
		med(func(r result) float64 { return float64(r.procs) }),
		med(func(r result) float64 { return mib(r.ws) }),
		med(func(r result) float64 { return mib(r.commit) }),
		med(func(r result) float64 { return float64(r.gone.Milliseconds()) }),
		med(func(r result) float64 { return float64(r.leftAt2s) }))
}

func hostAlive(ps []proc, pid uint32, name string) bool {
	for _, p := range ps {
		if p.PID == pid && strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

func names(m map[string]int) string {
	var s []string
	for k, v := range m {
		s = append(s, fmt.Sprintf("%s x%d", k, v))
	}
	sort.Strings(s)
	return strings.Join(s, ", ")
}

func copyFile(dst, src string) error {
	in, err := os.Open(src) //nolint:gosec // G304: the program the user asked to measure
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // G304: a new file under %TEMP%\hb-dev
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func runOnce(app, args, tag, dir string, cold bool, idle time.Duration, shot string) (result, error) {
	r := result{names: map[string]int{}}
	exe := app
	var cleanup []string
	if cold {
		d := filepath.Join(os.TempDir(), "hb-dev", "gui-spike", "cold", tag)
		if err := os.MkdirAll(d, 0o750); err != nil {
			return r, err
		}
		cleanup = append(cleanup, d)
		exe = filepath.Join(d, filepath.Base(app))
		if err := copyFile(exe, app); err != nil {
			return r, err
		}
	}
	var argv []string
	for _, a := range strings.Fields(args) {
		a = strings.ReplaceAll(a, "{run}", tag)
		argv = append(argv, os.ExpandEnv(a))
	}
	if cold {
		for _, a := range argv {
			if strings.Contains(args, "{run}") && strings.Contains(a, tag) {
				cleanup = append(cleanup, a)
			}
		}
	}
	defer func() {
		for _, c := range cleanup {
			_ = os.RemoveAll(c)
		}
	}()

	before, err := snapshot()
	if err != nil {
		return r, err
	}
	cmd := exec.Command(exe, argv...) //nolint:gosec // G204: the program the user asked to measure
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return r, err
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return r, err
	}
	pid := uint32(cmd.Process.Pid)
	painted := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "HB-SPIKE painted") {
				ms := -1
				if i := strings.Index(line, "since_create_ms="); i >= 0 {
					ms, _ = strconv.Atoi(line[i+len("since_create_ms="):])
				}
				painted <- ms
			}
		}
	}()
	select {
	case ms := <-painted:
		r.paint, r.selfMs = time.Since(t0), ms
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return r, errors.New("no first paint within 60 s")
	}

	time.Sleep(idle)
	ps, err := snapshot()
	if err != nil {
		return r, err
	}
	desc := descendants(ps, pid)
	old := map[uint32]bool{}
	for _, p := range before {
		old[p.PID] = true
	}
	byPID := map[uint32]string{}
	var pids []uint32
	for _, p := range ps {
		switch {
		case desc[p.PID]:
			byPID[p.PID] = p.Name
			pids = append(pids, p.PID)
			r.ws += p.WSPrivate
			r.commit += p.CommitPriv
			r.names[p.Name]++
		case !old[p.PID] && strings.EqualFold(p.Name, "msedgewebview2.exe"):
			r.strays = append(r.strays, fmt.Sprintf("%s(%d, parent %d)", p.Name, p.PID, p.PPID))
		}
	}
	r.procs = len(pids)

	hwnd := findWindow(pid)
	if hwnd == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return r, errors.New("no visible window")
	}
	if shot != "" {
		if err := screenshot(hwnd, shot); err != nil {
			fmt.Fprintln(os.Stderr, "measure: screenshot:", err)
		} else {
			fmt.Println("screenshot", shot)
		}
	}

	tc := time.Now()
	_, _, _ = procPostMessageW.Call(hwnd, wmClose, 0, 0)
	r.leftAt2s = -1
	for {
		ps, err := snapshot()
		if err != nil {
			return r, err
		}
		alive := 0
		left := map[string]int{}
		for _, p := range ps {
			if n, ok := byPID[p.PID]; ok && strings.EqualFold(n, p.Name) {
				alive++
				left[fmt.Sprintf("%s(%d threads)", p.Name, p.Threads)]++
			}
		}
		el := time.Since(tc)
		if r.hostGone == 0 && !hostAlive(ps, pid, byPID[pid]) {
			r.hostGone = el
		}
		if el >= 2*time.Second && r.leftAt2s < 0 {
			r.leftAt2s, r.left = alive, left
		}
		if alive == 0 {
			r.gone = el
			if r.leftAt2s < 0 {
				r.leftAt2s = 0
			}
			break
		}
		if el > 10*time.Second {
			_ = cmd.Process.Kill()
			r.gone = el
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Wait()
	return r, nil
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
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
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
		_, _, _ = procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&p)))
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

// screenshot saves hwnd's client area as a PNG. PW_RENDERFULLCONTENT makes
// PrintWindow include DirectComposition content such as WebView2's.
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
	}{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
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
