// hyperbutler GUI WebRTC probe: added to the page only by `hyperbutler gui
// --probe-webrtc` (bench/footprint -check-webrtc), never in normal use. It tries what a
// script made to run in the page could: create an RTCPeerConnection with a STUN server,
// make an offer and set it, which starts ICE gathering over UDP, in the page and in a
// new frame. It reports what happened through the "layout" operation; bench/footprint
// watches the socket table meanwhile.
"use strict";
(() => {
  const report = (text) => {
    try { window.chrome.webview.postMessage({ id: 9007199254740000, op: "layout", args: { text: "webrtc " + text } }); } catch (_) { /* nothing */ }
  };
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  async function probe(w, where) {
    let C;
    try { C = w.RTCPeerConnection || w.webkitRTCPeerConnection; } catch (e) { return where + ": not reachable (" + e.name + ")"; }
    if (typeof C !== "function") return where + ": RTCPeerConnection unavailable";
    let pc;
    try { pc = new C({ iceServers: [{ urls: "stun:127.0.0.1:3478" }] }); } catch (e) { return where + ": constructor threw " + e.name; }
    let cands = 0;
    pc.onicecandidate = (e) => { if (e.candidate) cands++; };
    try {
      pc.createDataChannel("probe");
      await pc.setLocalDescription(await pc.createOffer());
    } catch (e) { pc.close(); return where + ": offer failed " + e.name; }
    await wait(5000);
    const st = pc.iceGatheringState;
    pc.close();
    return where + ": created, " + cands + " candidates, gathering " + st;
  }
  window.addEventListener("load", async () => {
    if (window !== window.top) return;
    const parts = [await probe(window, "page")];
    try {
      const f = document.createElement("iframe");
      document.body.append(f);
      parts.push(await probe(f.contentWindow, "frame"));
      f.remove();
    } catch (e) { parts.push("frame: not created (" + e.name + ")"); }
    report(parts.join("; "));
  });
})();
