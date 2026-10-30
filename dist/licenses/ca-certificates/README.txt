ca-certificates-bundle (Alpine origin: ca-certificates)

The guest image ships only /etc/ssl/certs/ca-certificates.crt from this package. That
PEM bundle is derived from Mozilla's certdata.txt, which is licensed under the Mozilla
Public License 2.0 (MPL-2.0.txt in this directory, the text published at
https://www.mozilla.org/media/MPL/2.0/index.txt). The source form, certdata.txt at the
matching revision, is in the guest source bundle (distfiles/ and
aports/<commit>/main/ca-certificates/).

Alpine's package license also lists MIT, which covers Alpine's ca-certificates scripts
(update-ca-certificates and the build tooling). Those scripts are not part of the
guest image.

The upstream ca-certificates archive has no top-level license file, so
scripts/guest-image/fetch-sources.sh takes this directory instead.
