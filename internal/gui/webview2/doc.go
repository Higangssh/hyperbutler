// Package webview2 is a small binding of the Microsoft Edge WebView2 COM interfaces the
// hyperbutler GUI uses (ADR 0008, "Toolkit spike results"), in pure Go (no cgo).
//
// It replaces github.com/jchv/go-webview2, which the spike used, for the reasons the
// ADR lists, and differs from it in these ways:
//
//   - No callback panics or calls log.Fatal: a failure in a COM callback is recorded
//     and reported through Config.OnReady or the return value.
//   - Every interface pointer it obtains (arguments of events, requests, responses,
//     streams, settings, the controller, the environment) is released.
//   - The handler objects it hands to native code are Go values pinned with
//     runtime.Pinner for as long as WebView2 may call them, and unpinned on Close.
//   - Close calls ICoreWebView2Controller::Close before releasing anything, so the
//     browser processes are told to go before the host exits.
//   - WebView2Loader.dll is loaded from the directory of the executable, by full path,
//     never from the search path and never mapped from memory.
//   - The environment options are a COM object of its own, so browser arguments
//     (software compositing: --disable-gpu) go to this WebView only, not through the
//     process environment into every child process.
//
// The page is served from memory through WebResourceRequested: every request the page
// makes, to any URL, goes to Config.Serve, so nothing reaches the network; navigation
// to any other origin, new windows, downloads and permission requests are refused.
package webview2
