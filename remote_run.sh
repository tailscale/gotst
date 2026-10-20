# gotst -remote guest script; see remote.go.
#
# The local gotst sends a tar of this script, meta.env, and optionally
# src.bundle, then (for helpers) one line with the
# leader's tailcat address. This script syncs a checkout to the local
# snapshot commit, obtains gotst, and runs it as the leader or a helper.
# Its own output goes to stderr; only gotst's stdout goes to stdout.
#
# The remote command can set these to adapt to its environment:
#
#   GOTST_SRC_URL  git URL to fetch the repository's commits from
#                  (default: the local origin remote, as https for GitHub)
#   GOTST_BIN      an installed gotst to use instead of installing one
#   GOTST_GOTST_SRC_URL  git URL to fetch -remote-gotst's ref from
#                  (default: https://github.com/tailscale/gotst)
#   GOTST_REMOTE_DIR  directory for checkouts and gotst builds
#                  (default: $HOME/.cache/gotst-remote)
set -eu
exec 3<&0 </dev/null 4>&1 1>&2
dir=$(pwd)
. "$dir/meta.env"

say() { echo "gotst-remote[$NAME]: $*"; }
fail() { say "error: $*"; exit 1; }
# The local gotst shows the remote shell command's earlier output only if
# this script never starts, so always say something first.
say "started on $(hostname) with $(nproc) CPUs"

if [ -z "${GOCACHEPROG:-}" ]; then
	say "warning: GOCACHEPROG is unset; machines will not share build outputs"
fi
# Identical test binaries on every machine regardless of checkout path.
GOFLAGS="-trimpath${GOFLAGS:+ $GOFLAGS}"
export GOFLAGS

srcurl=${GOTST_SRC_URL:-$SRC_URL}
base=${GOTST_REMOTE_DIR:-$HOME/.cache/gotst-remote}
src="$base/src/$CHECKOUT"
if [ ! -d "$src/.git" ]; then
	git init -q "$src"
	git -C "$src" remote add origin "$srcurl"
else
	git -C "$src" remote set-url origin "$srcurl"
fi
cd "$src"
need=
for c in $BASES; do
	git cat-file -e "$c^{commit}" 2>/dev/null || need="$need $c"
done
if [ -n "$need" ]; then
	say "fetching$need from $srcurl"
	git fetch -q --depth=1 origin $need || fail "fetching$need from $srcurl; are they pushed?"
fi
if [ -f "$dir/src.bundle" ]; then
	git fetch -q "$dir/src.bundle" "$BUNDLE_REF" || fail "fetching local changes from bundle"
fi
git -c advice.detachedHead=false checkout -q --force --detach "$SNAP"
git clean -q -fd
say "checked out $SNAP"

go=go
if [ -x "$src/tool/go" ]; then
	go="$src/tool/go"
fi
# gotst is built with the Go version its go.mod asks for, not the
# checkout's toolchain: a fork such as Tailscale's adds build tags
# (tailscale_go) that can break it. Switching toolchains unpacks one into
# the module cache, so use a private writable one, in case the default is
# read-only (as with gomodfs).
gotstenv() {
	env GOMODCACHE="$base/gotst-modcache" GOFLAGS="${GOFLAGS:+$GOFLAGS }-modcacherw" "$@"
}
# modtoolchain GOMOD prints the GOTOOLCHAIN for the Go version that go.mod
# file asks for.
modtoolchain() {
	t=$(sed -n 's/^toolchain \([^ ]*\).*/\1/p' "$1" | head -n 1)
	[ -n "$t" ] || t="go$(sed -n 's/^go \([^ ]*\).*/\1/p' "$1" | head -n 1)"
	echo "$t"
}
if [ -n "${GOTST_BIN:-}" ]; then
	gotst=$GOTST_BIN
elif [ -n "$GOTST_REF" ]; then
	# Shallow-fetch the ref on every run, so a pushed branch's latest
	# commit is used, but install each commit only once.
	gs="$base/gotst-src"
	[ -d "$gs/.git" ] || git init -q "$gs"
	gsurl=${GOTST_GOTST_SRC_URL:-https://github.com/tailscale/gotst}
	git -C "$gs" fetch -q --depth=1 "$gsurl" "$GOTST_REF" || fail "fetching gotst $GOTST_REF from $gsurl"
	git -C "$gs" -c advice.detachedHead=false checkout -q --force --detach FETCH_HEAD
	rev=$(git -C "$gs" rev-parse HEAD)
	bin="$base/bin/$rev"
	gotst="$bin/gotst"
	if [ ! -x "$gotst" ]; then
		say "installing gotst $GOTST_REF ($rev)"
		(cd "$gs" && gotstenv GOTOOLCHAIN="$(modtoolchain go.mod)" GOBIN="$bin" "$go" install .) >"$dir/gotst-install.log" 2>&1 ||
			{ cat "$dir/gotst-install.log"; fail "installing gotst"; }
	fi
else
	case "$GOTST_VERSION" in
	unknown:*) fail "${GOTST_VERSION#unknown: }; push it to a branch of github.com/tailscale/gotst and use -remote-gotst=BRANCH" ;;
	esac
	bin="$base/bin/$GOTST_VERSION"
	gotst="$bin/gotst"
	if [ ! -x "$gotst" ]; then
		say "installing github.com/tailscale/gotst@$GOTST_VERSION"
		(cd "$base" &&
			mod=$(gotstenv "$go" mod download -json "github.com/tailscale/gotst@$GOTST_VERSION" | sed -n 's/^[[:space:]]*"GoMod": "\(.*\)",*$/\1/p') &&
			gotstenv GOTOOLCHAIN="$(modtoolchain "$mod")" GOBIN="$bin" "$go" install "github.com/tailscale/gotst@$GOTST_VERSION") >"$dir/gotst-install.log" 2>&1 ||
			{ cat "$dir/gotst-install.log"; fail "installing gotst; for an unpublished build, push it to a branch and use -remote-gotst=BRANCH"; }
	fi
fi

cd "$src/$REL"
jobs=${JOBS:-$(nproc)}
if [ "$ROLE" = leader ]; then
	say "starting gotst leader with -j=$jobs"
	exec "$gotst" -dist -j="$jobs" "$@" >&4 3<&- 4>&-
fi
say "waiting for the leader"
read -r addr <&3 || exit 0
say "joining leader with -j=$jobs"
exec "$gotst" -helper="$addr" -helper-name="$NAME" -j="$jobs" >&4 3<&- 4>&-
