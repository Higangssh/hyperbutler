# The MSI version of a hyperbutler version, for build-msi.ps1 and build-sandbox-msi.ps1: X.Y.Z of a
# release tag or one of its pre-releases (-rc.1 and the like), and anything else (a `git
# describe` of a later commit, v0.2.0-5-gabc1234, or a dirty tree, which would share the
# tag's version; a snapshot) 0.0.0, which any release upgrades.
function MsiVersion([string]$version) {
    if ($version -match '^v(\d{1,3})\.(\d{1,3})\.(\d{1,5})(?:-(?:alpha|beta|rc|preview)(?:\.?\d+)?)?$') {
        if ([int]$Matches[1] -gt 255 -or [int]$Matches[2] -gt 255 -or [int]$Matches[3] -gt 65535) {
            throw "version $version does not fit an MSI version"
        }
        return "$($Matches[1]).$($Matches[2]).$($Matches[3])"
    }
    return '0.0.0'
}
