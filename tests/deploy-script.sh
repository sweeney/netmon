#!/usr/bin/env bash
# Tests deploy/deploy.sh with shims for ssh and go: which host each site goes
# to, the GOARCH picked from the host's `uname -m`, the config staged for the
# site, the hub passed to remote.sh, sudo only outside --check, and the
# refusals. The sites are deploy/sites.example's, not your local deploy/sites.
# Nothing leaves this machine.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$HERE/.."
WORK="$(mktemp -d "${TMPDIR:-/tmp}/netmon-deploy-test.XXXXXX")"
fails=0
trap '[ "$fails" -gt 0 ] && echo "work dir kept: $WORK" || rm -rf "$WORK"' EXIT
pass() { echo "  PASS  $*"; }
fail() { echo "  FAIL  $*"; fails=$((fails + 1)); }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }

SHIMS="$WORK/shims"; mkdir -p "$SHIMS"
# ssh: records the host and command; plays a host whose arch is FAKE_ARCH.
# Remote dirs live under $WORK/remote; remote.sh is not run, but the staged
# config it would install is copied out for the test to inspect.
cat > "$SHIMS/ssh" <<'EOF'
#!/usr/bin/env bash
set -u
tty=""
while [ $# -gt 0 ]; do
  case "$1" in -o) shift 2 ;; -t) tty="-t "; shift ;; *) break ;; esac
done
host="$1"; shift
echo "ssh ${tty}$host $*" >> "$T/calls"
[ "${FAKE_SSH_FAIL:-}" = 1 ] && exit 255
case "$1" in
  true) exit 0 ;;
  uname) echo "$FAKE_ARCH" ;;
  mktemp) mkdir -p "$T/remote"; mktemp -d "$T/remote/netmon.XXXXXX" ;;
  tar) tar "${@:2}" ;;
  rm) exit 0 ;;
  bash|sudo)
    dir="$(dirname "$(dirname "${!#}")")"
    for a in "$@"; do case "$a" in */remote.sh) dir="$(dirname "$(dirname "$a")")" ;; esac; done
    cp "$dir/config.toml.example" "$T/staged-config.toml"
    cp "$dir/netmon" "$T/staged-netmon" ;;
esac
EOF
# go: records GOARCH and writes a placeholder binary to -o.
cat > "$SHIMS/go" <<'EOF'
#!/usr/bin/env bash
echo "go GOOS=$GOOS GOARCH=$GOARCH $*" >> "$T/calls"
while [ $# -gt 0 ]; do [ "$1" = -o ] && { echo "netmon for $GOARCH" > "$2"; }; shift; done
EOF
chmod +x "$SHIMS/ssh" "$SHIMS/go"

run() {  # run NAME ARCH -- deploy.sh args
  T="$WORK/$1"; rm -rf "$T"; mkdir -p "$T"; : > "$T/calls"
  local arch="$2"; shift 3
  env T="$T" FAKE_ARCH="$arch" ${FAKE_SSH_FAIL:+FAKE_SSH_FAIL=$FAKE_SSH_FAIL} PATH="$SHIMS:$PATH" \
    NETMON_SITES="${SITES_FILE:-$SRC/deploy/sites.example}" \
    bash "$SRC/deploy/deploy.sh" "$@" > "$T/out" 2>&1
}
called() { grep -qF -- "$1" "$T/calls"; }
out_has() { grep -qF -- "$1" "$T/out"; }

echo "1. --site schoolhouse: the Pi, arm64"
run default aarch64 -- --site schoolhouse --check && pass "exits 0" || fail "failed: $(cat "$T/out")"
check "ssh to the Pi" called "ssh pi@100.64.0.10 uname -m"
check "built for arm64" called "go GOOS=linux GOARCH=arm64"
check "staged the schoolhouse config" cmp -s "$SRC/configs/schoolhouse.toml" "$T/staged-config.toml"
check "--check runs remote.sh without sudo, with the site's hub" bash -c "grep -q 'ssh pi@100.64.0.10 bash .*/remote.sh --hub 100.64.0.20 --check' '$T/calls' && ! grep -q sudo '$T/calls'"

echo "2. --site home: garibaldi, amd64"
run home x86_64 -- --site home --check && pass "exits 0" || fail "failed: $(cat "$T/out")"
check "ssh to garibaldi" called "ssh admin@100.64.0.20 uname -m"
check "built for amd64" called "go GOOS=linux GOARCH=amd64"
check "staged the home config" cmp -s "$SRC/configs/home.toml" "$T/staged-config.toml"
check "says what it is doing" out_has "site home on admin@100.64.0.20: building netmon"

echo "3. install uses sudo with a tty, and passes arguments through"
run inst x86_64 -- --site home --yes && pass "exits 0" || fail "failed: $(cat "$T/out")"
check "sudo over ssh -t; the hub verifies itself on 127.0.0.1" bash -c "grep -q 'ssh -t admin@100.64.0.20 sudo bash .*/remote.sh --hub 127.0.0.1 --yes' '$T/calls'"
run rb aarch64 -- --site schoolhouse rollback --check || fail "rollback --check failed: $(cat "$T/out")"
check "rollback --check passed through" bash -c "grep -q 'remote.sh --hub 100.64.0.20 rollback --check' '$T/calls'"

echo "4. --host and --hub override the site's"
run host aarch64 -- --site schoolhouse --host me@10.0.0.9 --check || fail "failed: $(cat "$T/out")"
check "ssh to the override" bash -c "grep -q 'ssh me@10.0.0.9 uname' '$T/calls' && ! grep -q 100.64.0.10 '$T/calls'"
check "hub still the site's" bash -c "grep -q 'remote.sh --hub 100.64.0.20 --check' '$T/calls'"
SITES_FILE=/nonexistent run both aarch64 -- --site schoolhouse --host me@10.0.0.9 --hub 10.0.0.1 --check || fail "both given, no sites file: $(cat "$T/out")"
check "both given: no sites file needed" bash -c "grep -q 'remote.sh --hub 10.0.0.1 --check' '$T/calls'"

echo "5. refusals"
run nosite-given aarch64 -- --check && fail "no --site: should refuse" || { out_has "--site NAME is required (sites: home schoolhouse)" && pass "no --site refused, lists the sites" || fail "no --site: $(cat "$T/out")"; }
check "no --site: no ssh, no build" bash -c "! grep -qE '^(ssh|go) ' '$T/calls'"
run nosite-rb aarch64 -- rollback && fail "rollback without --site: should refuse" || { out_has "--site NAME is required" && pass "rollback without --site refused" || fail "rollback, no --site: $(cat "$T/out")"; }
run nosite aarch64 -- --site nowhere --check && fail "unknown site: should refuse" || { out_has "no config for site 'nowhere'" && pass "unknown site refused"; }
check "unknown site: no ssh, no build" bash -c "! grep -qE '^(ssh|go) ' '$T/calls'"
run arch armv6l -- --site schoolhouse --check && fail "armv6l: should refuse" || { out_has "netmon is built for arm64 and amd64 only" && pass "unsupported arch refused"; }
check "unsupported arch: nothing built" bash -c "! grep -q '^go ' '$T/calls'"
SITES_FILE=/nonexistent run nofile aarch64 -- --site schoolhouse --check && fail "no sites file: should refuse" || { out_has "copy deploy/sites.example to deploy/sites" && pass "missing sites file refused, says how to fix" || fail "no sites file: $(cat "$T/out")"; }
printf 'schoolhouse pi@100.64.0.10\n' > "$WORK/sites-nohub"
SITES_FILE="$WORK/sites-nohub" run nohub aarch64 -- --site schoolhouse --check && fail "no hub: should refuse" || { out_has "has no hub" && pass "site without a hub refused" || fail "no hub: $(cat "$T/out")"; }
FAKE_SSH_FAIL=1 run nossh aarch64 -- --site schoolhouse --check && fail "no ssh: should refuse" || { out_has "cannot ssh to pi@100.64.0.10" && pass "unreachable host refused"; }

echo "6. every site in deploy/sites.example has a config, and vice versa"
sites="$(awk '!/^#/ && NF {print $1}' "$SRC/deploy/sites.example" | sort)"
configs="$(cd "$SRC/configs" && ls *.toml | sed 's/\.toml$//' | sort)"
check "deploy/sites.example matches configs/ ($(echo $sites))" test "$sites" = "$configs"

echo "7. every config ever committed is in shipped-configs.sha256, or is current"
missing=""
for c in $(git -C "$SRC" log --format=%h -- config.toml.example configs); do
  for f in $(git -C "$SRC" ls-tree --name-only -r "$c" -- config.toml.example configs); do
    h="$(git -C "$SRC" show "$c:$f" | shasum -a 256 | cut -d' ' -f1)"
    [ -f "$SRC/$f" ] && [ "$h" = "$(shasum -a 256 < "$SRC/$f" | cut -d' ' -f1)" ] && continue
    grep -q "^$h " "$SRC/deploy/shipped-configs.sha256" || missing="$missing $f@$c"
  done
done
check "no shipped config missing from the list${missing:+ (missing:$missing)}" test -z "$missing"

echo
if [ $fails -eq 0 ]; then echo "all passed"; else echo "$fails failed"; exit 1; fi
