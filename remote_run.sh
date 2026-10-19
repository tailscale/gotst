# gotst -remote guest script; see remote.go.
#
# The local gotst sends a tar of this script, meta.env, and optionally
# src.bundle and a gotst binary, then (for helpers) one line with the
# leader's tailcat address. This script syncs a checkout to the local
# snapshot commit, obtains gotst, and runs it as the leader or a helper.
# Its own output goes to stderr; only gotst's stdout goes to stdout.
#
# The remote command can set these to adapt to its environment:
#
#   GOTST_SRC_URL  git URL to fetch the repository's commits from
#                  (default: the local origin remote, as https for GitHub)
#   GOTST_BIN      an installed gotst to use instead of one uploaded or
#                  installed with "go install"
#   GOTST_REMOTE_DIR  directory for checkouts and gotst builds
#                  (default: $HOME/.cache/gotst-remote)
set -eu
exec 3<&0 </dev/null 4>&1 1>&2
dir=$(pwd)
. "$dir/meta.env"

say() { echo "gotst-remote[$NAME]: $*"; }
fail() { say "error: $*"; exit 1; }

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
if [ -n "${GOTST_BIN:-}" ]; then
	gotst=$GOTST_BIN
elif [ -f "$dir/gotst" ]; then
	gotst="$dir/gotst"
	chmod +x "$gotst"
else
	bin="$base/bin/$GOTST_VERSION"
	gotst="$bin/gotst"
	if [ ! -x "$gotst" ]; then
		say "installing github.com/tailscale/gotst@$GOTST_VERSION"
		(cd "$base" && GOBIN="$bin" "$go" install "github.com/tailscale/gotst@$GOTST_VERSION") >"$dir/gotst-install.log" 2>&1 ||
			{ cat "$dir/gotst-install.log"; fail "installing gotst; for unpublished builds use -remote-gotst=FILE"; }
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
