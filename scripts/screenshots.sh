#!/usr/bin/env bash
# Regenerates the README screenshots in docs/ from the --fake data.
#
# Needs tmux, ffmpeg and freeze (go install github.com/charmbracelet/freeze@v0.2.2).
# Runs on its own tmux socket, so it never touches your tmux sessions.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/docs
TMP=$(mktemp -d)
T="tmux -L dnszoner-shots"
FREEZE=${FREEZE:-$(command -v freeze || echo "$(go env GOPATH)/bin/freeze")}
FONT=${FONT:-/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf}
trap '$T kill-server 2>/dev/null || true; rm -rf "$TMP"' EXIT

mkdir -p "$OUT"
(cd "$ROOT" && go build -o "$TMP/dnszoner" .)
$T kill-server 2>/dev/null || true
$T -f /dev/null new-session -d -s s -x 110 -y 28 \
	"COLORTERM=truecolor XDG_CACHE_HOME=$TMP/cache XDG_CONFIG_HOME=$TMP/config $TMP/dnszoner --fake; sleep 60"

wait_for() { timeout 10 bash -c "until $T capture-pane -p -t s | grep -qF -- \"$1\"; do sleep 0.1; done"; }
keys() { for k in "$@"; do $T send-keys -t s "$k"; sleep 0.15; done; }
lit() { $T send-keys -t s -l "$*"; sleep 0.3; }
shot() {
	sleep 0.4
	$T capture-pane -e -p -t s >"$TMP/$1.ansi"
	"$FREEZE" "$TMP/$1.ansi" --language ansi --window --font.file "$FONT" --font.size 14 \
		--padding 20 --margin 0 --border.radius 8 --background "#1e1e2e" --line-height 1.2 \
		-o "$TMP/$1.png" </dev/null >/dev/null
	ffmpeg -loglevel error -y -i "$TMP/$1.png" -vf scale=1400:-1:flags=lanczos "$OUT/$1.png"
	echo "wrote docs/$1.png"
}

wait_for "FAKE DATA"
lit "app devel"
wait_for "applepie.io"
shot search

keys Enter
wait_for "Edit record"
lit "  203.0.113.50 "
keys Tab
shot edit

keys Escape Escape
wait_for "search domains"
lit "mailhost"
keys C-a Enter
wait_for "Bulk edit"
keys Tab
lit "mailhost.net"
keys Tab
lit "newmail.io"
shot bulk

keys Enter l
wait_for "Saved"
keys Escape
lit "203.0.113.23"
keys Enter
lit "203.0.113.50"
keys Enter l
keys C-l
wait_for "Pending changes"
shot pending

keys Escape C-o
wait_for "Accounts"
keys t
wait_for "connected"
shot accounts
keys Down Down Down a Right Tab
lit "Namecheap personal"
keys Tab
lit "chris"
keys Tab
lit "0123456789abcdef"
keys Tab
shot account-form
