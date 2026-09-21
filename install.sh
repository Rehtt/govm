#!/bin/sh
# Install govm from the official GitHub Releases. No Go toolchain required.
set -eu
fail() { printf 'govm: %s\n' "$*" >&2; exit 1; }
base=https://github.com/Rehtt/govm/releases
case $(uname -s) in Linux) os=linux ;; Darwin) os=darwin ;; *) fail 'Unsupported operating system (use install.ps1 on Windows).' ;; esac
case $(uname -m) in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) fail 'Unsupported architecture; expected amd64 or arm64.' ;; esac
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fLsS --retry 3 "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
else fail 'Install curl or wget first.'; fi
if command -v sha256sum >/dev/null 2>&1; then
    digest() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
    digest() { shasum -a 256 "$1" | awk '{print $1}'; }
else fail 'Install sha256sum or shasum first.'; fi
command -v tar >/dev/null 2>&1 || fail 'Install tar first.'
: "${HOME:?HOME must be set}"
SHELL=${SHELL:-}
dir=${GOVM_INSTALL_DIR:-$HOME/.govm/bin}
case $dir in /*) ;; *) dir=$PWD/$dir ;; esac
# PATH cannot represent a directory containing a colon or newline.
case $dir in *:*|*'
'*) fail 'Installation directory cannot contain a colon or newline.' ;; esac
tmp=$(mktemp -d)
staged=
trap 'rm -rf "$tmp"; if [ -n "$staged" ]; then rm -f "$staged"; fi' 0
trap 'exit 1' HUP INT TERM
version=${GOVM_VERSION:-}
if [ -z "$version" ]; then
    fetch https://api.github.com/repos/Rehtt/govm/releases/latest "$tmp/latest.json" || fail 'Could not resolve latest Release.'
    version=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/latest.json")
fi
case $version in ''|*[!a-zA-Z0-9._-]*) fail 'Invalid Release tag; use letters, digits, dots, underscores or hyphens.' ;; esac
asset=govm_${os}_${arch}.tar.gz
printf 'Downloading govm %s (%s/%s)...\n' "$version" "$os" "$arch"
fetch "$base/download/$version/$asset" "$tmp/$asset" || fail 'Archive download failed; existing installation preserved.'
fetch "$base/download/$version/checksums.txt" "$tmp/checksums.txt" || fail 'Checksum download failed; existing installation preserved.'
expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name {print $1}' "$tmp/checksums.txt")
[ ${#expected} -eq 64 ] || fail 'Missing or ambiguous checksum entry.'
case $expected in *[!0-9a-fA-F]*) fail 'Invalid checksum entry.' ;; esac
actual=$(digest "$tmp/$asset")
[ "$actual" = "$(printf '%s' "$expected" | tr A-F a-f)" ] || fail 'SHA-256 mismatch; existing installation preserved.'
tar -xzf "$tmp/$asset" -C "$tmp" govm || fail 'Could not extract govm.'
[ -f "$tmp/govm" ] && [ ! -L "$tmp/govm" ] || fail 'Archive does not contain a regular govm binary.'
mkdir -p "$dir"
staged=$(mktemp "$dir/.govm-install.XXXXXX")
cp "$tmp/govm" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$dir/govm" || fail 'Could not replace govm; check directory permissions.'
staged=
# Separate markers and backup suffix avoid interfering with `govm init`.
start='# >>> govm installer PATH >>>'
end='# <<< govm installer PATH <<<'
quote=$(printf '%s' "$dir" | sed "s/'/'\\\\''/g")
printf '%s\n' "$start" > "$tmp/block"
case ${SHELL##*/} in
    fish)
        fishquote=$(printf '%s' "$dir" | sed "s/\\\\/\\\\\\\\/g; s/'/\\\\'/g")
        printf "if not contains -- '%s' \$PATH\n    set -gx PATH '%s' \$PATH\nend\n" "$fishquote" "$fishquote" >> "$tmp/block"
        ;;
    *) printf "case \":\$PATH:\" in\n    *:'%s':*) ;;\n    *) export PATH='%s':\"\$PATH\" ;;\nesac\n" "$quote" "$quote" >> "$tmp/block" ;;
esac
printf '%s\n' "$end" >> "$tmp/block"
configure() {
    file=$1
    mkdir -p "$(dirname "$file")"
    if [ -f "$file" ]; then
        [ -e "$file.govm-install.bak" ] || cp -p "$file" "$file.govm-install.bak"
        awk -v start="$start" -v end="$end" '$0 == start {skip=1; next} $0 == end {skip=0; next} !skip {print}' "$file" > "$tmp/config"
    else : > "$tmp/config"; fi
    cat "$tmp/block" >> "$tmp/config"
    cat "$tmp/config" > "$file"
}
case ${SHELL##*/} in
    bash)
        configure "$HOME/.bashrc"
        if [ -f "$HOME/.bash_profile" ]; then configure "$HOME/.bash_profile"
        elif [ -f "$HOME/.bash_login" ]; then configure "$HOME/.bash_login"
        else configure "$HOME/.profile"; fi ;;
    zsh) configure "${ZDOTDIR:-$HOME}/.zshrc" ;;
    fish) configure "${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish" ;;
    *) printf 'Unknown shell; add the following directory to your shell PATH: %s\n' "$dir" ;;
esac
printf 'Installed %s/govm (%s). Open a new terminal, or run:\n' "$dir" "$version"
if [ "${SHELL##*/}" = fish ]; then
    printf "  set -gx PATH '%s' \$PATH\n" "$fishquote"
else printf "  export PATH='%s':\"\$PATH\"\n" "$quote"; fi
printf 'Then run: govm version\nInstall Go separately with: govm install latest\n'
