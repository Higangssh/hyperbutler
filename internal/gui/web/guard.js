// hyperbutler GUI guard. The host adds it to every document (and frame) before any of
// the page's own scripts (WebView2's AddScriptToExecuteOnDocumentCreated); it is not one
// of the page's files.
//
// WebRTC is the one network API the CSP does not govern: an RTCPeerConnection gathers
// ICE candidates over UDP (STUN) whatever connect-src says, and WebView2 has no
// permission or policy that turns it off. The page never uses it, so it is removed:
// the constructors are made undefined, not writable and not configurable, so a script
// made to run in the page cannot put them back. A new frame would be a fresh realm with
// its own constructors, so the page cannot create frames either (it never does; the CSP
// already refuses to load anything into one).
"use strict";
(() => {
  const gone = ["RTCPeerConnection", "webkitRTCPeerConnection", "RTCDataChannel", "RTCSessionDescription",
    "RTCIceCandidate", "RTCRtpSender", "RTCRtpReceiver", "RTCRtpTransceiver", "RTCDtlsTransport",
    "RTCIceTransport", "RTCSctpTransport", "RTCCertificate", "RTCDTMFSender", "RTCRtpScriptTransform",
    "RTCEncodedAudioFrame", "RTCEncodedVideoFrame", "RTCPeerConnectionIceEvent", "RTCTrackEvent",
    "RTCDataChannelEvent"];
  for (const name of gone) {
    try {
      Object.defineProperty(window, name, { value: undefined, writable: false, configurable: false, enumerable: false });
    } catch (_) { /* not there in this runtime */ }
  }
  const framing = new Set(["iframe", "frame", "frameset", "object", "embed", "portal", "fencedframe"]);
  const refuse = (name) => {
    if (framing.has(String(name).toLowerCase())) throw new DOMException("frames are not allowed in this page", "NotSupportedError");
  };
  const create = Document.prototype.createElement;
  const createNS = Document.prototype.createElementNS;
  Object.defineProperty(Document.prototype, "createElement", {
    value: function createElement(name, options) { refuse(name); return create.call(this, name, options); },
    writable: false, configurable: false,
  });
  Object.defineProperty(Document.prototype, "createElementNS", {
    value: function createElementNS(ns, name, options) {
      refuse(String(name).replace(/^.*:/, ""));
      return createNS.call(this, ns, name, options);
    },
    writable: false, configurable: false,
  });
})();
