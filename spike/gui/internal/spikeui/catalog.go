// Package spikeui is the part the two GUI toolkit spikes (spike/gui/webview2 and
// spike/gui/native) share: the engine status read through the CLI, a small message
// catalog in English and Korean, and the first-paint report the measuring harness reads.
// It is spike code: the real GUI gets a proper catalog and the internal status call.
package spikeui

import "strings"

// Message keys. Every user-facing string of the spikes comes from Catalog, as
// REQ-UX-013 asks of the real GUI; the Korean text is written, not translated
// literally, and the layouts leave room for text about 30% longer than the English.
const (
	KeyTitle        = "title"
	KeyGreeting     = "greeting"
	KeyGreetingSub  = "greeting.sub"
	KeyTabHome      = "tab.home"
	KeyTabContainer = "tab.containers"
	KeyTabStorage   = "tab.storage"
	KeyTabSettings  = "tab.settings"
	KeyEngine       = "card.engine"
	KeyMemory       = "card.memory"
	KeyCPUs         = "card.cpus"
	KeyDisk         = "card.disk"
	KeyStart        = "button.start"
	KeyStarting     = "button.starting"
	KeyChecking     = "status.checking"
	KeyRunning      = "status.running"
	KeyStopped      = "status.stopped"
	KeyNotSetUp     = "status.notsetup"
	KeyNoCLI        = "status.nocli"
	KeyStubbed      = "status.stubbed"
	KeyUnitMB       = "unit.mb"
	KeyUnitGB       = "unit.gb"
)

// Catalog maps a language to its messages. English is the source text.
var Catalog = map[string]map[string]string{
	"en": {
		KeyTitle:        "hyperbutler",
		KeyGreeting:     "Hello! I'm your butler.",
		KeyGreetingSub:  "Docker is ready when you are. Nothing runs in the background while this window is closed.",
		KeyTabHome:      "Home",
		KeyTabContainer: "Containers",
		KeyTabStorage:   "Storage",
		KeyTabSettings:  "Settings",
		KeyEngine:       "Engine",
		KeyMemory:       "Memory",
		KeyCPUs:         "CPUs",
		KeyDisk:         "Disk used",
		KeyStart:        "Start",
		KeyStarting:     "Starting...",
		KeyChecking:     "Checking...",
		KeyRunning:      "Running",
		KeyStopped:      "Stopped",
		KeyNotSetUp:     "Not set up",
		KeyNoCLI:        "hyperbutler.exe not found",
		KeyStubbed:      "Start is stubbed in the spike (use -real-start)",
		KeyUnitMB:       "MB",
		KeyUnitGB:       "GB",
	},
	"ko": {
		KeyTitle:        "hyperbutler",
		KeyGreeting:     "안녕하세요! 저는 여러분의 집사예요.",
		KeyGreetingSub:  "필요할 때 바로 Docker를 준비해 드릴게요. 이 창을 닫으면 아무것도 뒤에서 돌지 않아요.",
		KeyTabHome:      "홈",
		KeyTabContainer: "컨테이너",
		KeyTabStorage:   "저장 공간",
		KeyTabSettings:  "설정",
		KeyEngine:       "엔진",
		KeyMemory:       "메모리",
		KeyCPUs:         "CPU",
		KeyDisk:         "디스크 사용량",
		KeyStart:        "시작하기",
		KeyStarting:     "시작하는 중...",
		KeyChecking:     "확인하는 중...",
		KeyRunning:      "실행 중",
		KeyStopped:      "멈춤",
		KeyNotSetUp:     "설정 전",
		KeyNoCLI:        "hyperbutler.exe를 찾지 못했어요",
		KeyStubbed:      "스파이크에서는 시작이 흉내만 내요 (-real-start)",
		KeyUnitMB:       "MB",
		KeyUnitGB:       "GB",
	},
}

// Messages returns the catalog for a BCP 47 tag such as "ko-KR", falling back to
// English for unknown languages and, key by key, for keys a language lacks.
func Messages(tag string) map[string]string {
	lang := strings.ToLower(tag)
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	out := make(map[string]string, len(Catalog["en"]))
	for k, v := range Catalog["en"] {
		out[k] = v
	}
	for k, v := range Catalog[lang] {
		out[k] = v
	}
	return out
}
