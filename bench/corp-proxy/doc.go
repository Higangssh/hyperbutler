// Command corp-proxy checks REQ-UX-009 end to end: behind a TLS-inspecting proxy, docker
// pull fails until dockerd trusts the proxy's CA, and works once it does; the proxy
// settings are honored, and their credentials leak nowhere.
//
//	corp-proxy -hyperbutler hyperbutler.exe -docker docker.exe [-image busybox:1.37]
//
// hyperbutler must be running a guest image with the dockerd environment feature; the
// bench uses the engine, settings and docker endpoint of the current HYPERBUTLER_HOME,
// DOCKER_CONFIG and DOCKER_HOST (point all three at a test install: it changes proxy.*
// and ca.extraFile and unsets them at the end). It needs Internet access to Docker Hub.
//
// It never touches a Windows certificate store. The proxy's root CA is made for the run
// and handed to hyperbutler through ca.extraFile, which goes through the same path as
// the certificates of the Windows stores (ca.import): the CertSet, the control channel,
// the guest's combined bundle and dockerd's SSL_CERT_FILE. The proxy listens on
// 127.0.0.1 only and requires Basic credentials; the guest reaches it through the host
// gateway, since a proxy URL on this host's loopback is rewritten to the host gateway
// address, as for a local NTLM relay (px, cntlm).
//
// Checks:
//
//   - with proxy.http/https set (user:password@127.0.0.1:PORT) and no CA, docker pull
//     fails with an x509 error, and the proxy saw CONNECT registry-1.docker.io:443;
//   - with ca.extraFile set to the proxy's CA, the pull succeeds, through the proxy (its
//     TLS handshakes with dockerd succeeded and it forwarded requests);
//   - the password is in none of: config list/get/set output, status --json,
//     state.json, the engine log, the guest console log, docker info, /proc/cmdline
//     seen from a container, a container's environment;
//   - in the guest, dockerd.env is 0600 in a 0700 directory and names the host gateway;
//   - containers get no proxy variables (they are dockerd's only);
//   - config set hostGateway.enabled false is refused while the proxy is on loopback;
//   - after the settings are unset and applied, the pull works without the proxy.
//
// It prints PASS/FAIL per check and exits 1 if any failed.
package main
