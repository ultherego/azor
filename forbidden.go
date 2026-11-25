package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

// forbidden is a hard limit, not a default: nothing matching it is ever
// watched, not even when a user lists it in the configuration. Keys,
// passwords, keyrings, browsers, mail, clipboard and shell history.
var forbidden = regexp.MustCompile(`(?i)(^|/)(` + strings.Join([]string{
	`\.ssh`, `ssh`, `\.gnupg`, `gnupg`, `\.gpg[^/]*`, `keyrings?`, `\.?password-store`, `\.pki`, `pki`, `kwalletd?[^/]*`, `wallets?`,
	`1password[^/]*`, `bitwarden[^/]*`, `keepass[^/]*`, `proton[^/]*pass[^/]*`, `enpass[^/]*`, `lastpass[^/]*`, `rbw`, `gopass`, `pass`,
	`\.?aws`, `\.?azure`, `gcloud`, `\.?kube`, `\.?docker`, `containers`, `rclone`, `gh`, `glab[^/]*`, `\.netrc`, `\.pgpass`, `\.git-credentials`, `op`, `sops`, `age`,
	`\.?mozilla`, `firefox[^/]*`, `zen`, `chromium[^/]*`, `google-chrome[^/]*`, `bravesoftware`, `vivaldi[^/]*`, `microsoft-edge[^/]*`, `opera[^/]*`, `thunderbird`, `evolution`,
	`cliphist`, `clipboard[^/]*`, `clipman`, `copyq`, `klipper[^/]*`, `gpaste`,
	`\.env[^/]*`, `shadow-?`, `gshadow-?`, `sudoers[^/]*`, `polkit-1`, `pam\.d`, `wireguard`, `openvpn[^/]*`, `system-connections`, `borgmatic[^/]*`, `restic[^/]*`, `cryptsetup[^/]*`, `crypttab`,
	`[^/]*history`, `[^/]*\.(pem|key|p12|pfx|jks|kdbx?|gpg|asc|age|ovpn)`, `id_(rsa|dsa|ecdsa|ed25519)[^/]*`, `known_hosts`, `authorized_keys`,
	`[^/]*(secret|credential|password|passwd|passphrase|token|cookie)[^/]*`, `signal`, `telegram[^/]*`, `\.?authenticator[^/]*`,
}, "|") + `)(/|$)`)

// allowed reports whether a path may be looked at. Symbolic links are
// followed first, so that a link cannot hide the real target.
func allowed(path string) bool {
	if forbidden.MatchString(path) {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	return err != nil || !forbidden.MatchString(real)
}
