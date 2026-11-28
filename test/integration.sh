#!/usr/bin/env bash
# Integration tests: the real daemon and CLI in throw-away directories, with a
# fake /proc for the boot id and boot clock, and a fake systemctl.
#   bash test/integration.sh [scenario...]
set -uo pipefail
cd "$(dirname "$0")/.."
ROOT=$(mktemp -d)
trap 'pkill -f "$ROOT/azor daemon" 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$ROOT"' EXIT
[ -n "${KEEP:-}" ] && echo "kept: $ROOT"
go build -o "$ROOT/azor" . || exit 1

pass=0 fail=0
ok() { printf '  \033[32mok\033[0m   %s\n' "$*"; pass=$((pass + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; fail=$((fail + 1)); }
check() { # check "name" command...: passes when the command succeeds
	local name=$1; shift
	if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi
}

# fresh environment for one scenario
setup() {
	H="$ROOT/$1"
	mkdir -p "$H/state/azor" "$H/run" "$H/cfg/azor" "$H/proc/sys/kernel/random" "$H/bin"
	echo "11111111-1111-1111-1111-111111111111" >"$H/proc/sys/kernel/random/boot_id"
	echo "1000.00 2000.00" >"$H/proc/uptime"
	printf 'sample = 1s\ninterval = 5s\nprocess_interval = 10s\nmodules = %s\n%s\n' "${MODULES:-cpu, memory, pressure, snapshots}" "${EXTRA:-}" >"$H/cfg/azor/azor.conf"
	cat >"$H/bin/systemctl" <<'S'
#!/bin/sh
[ "$*" = "--system is-system-running" ] && { cat "$(dirname "$0")/system-state" 2>/dev/null || echo running; exit 0; }
exit 1
S
	chmod +x "$H/bin/systemctl"
}
vars() { ENV=(env HOME="$H" XDG_STATE_HOME="$H/state" XDG_RUNTIME_DIR="$H/run" XDG_CONFIG_HOME="$H/cfg" AZOR_TEST_PROC="$H/proc" PATH="$H/bin:$PATH"); }
az() { vars; "${ENV[@]}" "$ROOT/azor" "$@"; }
# The daemon is started directly, not through a function: in the background a
# function runs in a subshell, and $! would be that subshell, not the daemon.
start() {
	vars
	rm -f "$H/run/azor/state.json"
	"${ENV[@]}" "$ROOT/azor" daemon 2>>"$H/err" &
	PID=$!
	waitfor "state" test -f "$H/run/azor/state.json"
}
waitfor() { # waitfor what command...: up to 30 s
	local what=$1; shift
	for _ in $(seq 1 60); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
	echo "    (timed out waiting for $what)"; return 1
}
stopd() { kill -TERM "$PID" && wait "$PID" 2>/dev/null; true; }
killd() { kill -KILL "$PID" && wait "$PID" 2>/dev/null; true; }
events() { az events 1h; }
has() { events | grep -E "$1" >/dev/null; }  # not -q: with pipefail an early exit of grep fails the pipe
count() { events | grep -cE "$1"; }
snap_has() { az snapshots | grep -E "$1" >/dev/null; }  # not -q: with pipefail an early exit of grep fails the pipe
latest_has() { az snapshot latest | grep -E "$1" >/dev/null; }  # not -q: with pipefail an early exit of grep fails the pipe
range_has() { az range "$(date -d '-2 hours' '+%F %H:%M')" now | grep -E "$1" >/dev/null; }  # not -q: with pipefail an early exit of grep fails the pipe

scenario_stop_start() {
	echo "== stop and start in the same boot"
	setup stop; start; stopd
	check "a clean stop is recorded" has 'daemon stopped'
	start; stopd
	check "no interruption after a clean stop" test "$(count 'INTR')" = 0
	check "no new boot in the same boot" test "$(count 'BOOT')" = 0
}

scenario_interrupted() {
	echo "== the daemon is killed"
	setup intr; start; sleep 2; killd
	start; stopd
	check "the interruption is recorded" has 'INTR +Azor ended without stopping'
	check "it is not called a new boot" test "$(count 'BOOT')" = 0
}

scenario_boot() {
	echo "== a new boot after a clean shutdown, then after a power loss"
	setup boot; start
	echo stopping >"$H/bin/system-state"; stopd; rm -f "$H/bin/system-state"
	check "the shutdown is recorded" has 'system shutting down'
	echo "22222222-2222-2222-2222-222222222222" >"$H/proc/sys/kernel/random/boot_id"
	start
	check "a new boot is recorded" has 'BOOT +new boot'
	check "a recorded shutdown is not unexpected" test "$(count 'without a recorded shutdown')" = 0
	killd # power loss: nothing written at the end
	echo "33333333-3333-3333-3333-333333333333" >"$H/proc/sys/kernel/random/boot_id"
	start; stopd
	check "an unrecorded end of a boot is reported" has 'without a recorded shutdown'
	check "it is not called a freeze or crash" test "$(events | grep -ciE 'freeze[^,]|crash')" = 0
}

scenario_suspend() {
	echo "== suspend: the boot clock jumps, the monotonic clock does not"
	setup sleep; start; sleep 2
	echo "4600.00 2000.00" >"$H/proc/uptime" # 3600 s more than the real time passed
	waitfor "suspend event" has 'resumed after'
	stopd
	local secs; secs=$(events | grep -oE 'resumed after [0-9hms]+' | head -1)
	check "the suspend is recorded ($secs)" has 'resumed after 59m5[0-9]s|resumed after 1h0m'
	check "a suspend is not also a gap" test "$(count 'GAP')" = 0
	check "range shows the suspended time" range_has 'suspended'
	check "range reports data per group" range_has 'data per group'
}

scenario_snapshot() {
	echo "== a snapshot when an alert starts, and one 10 s later"
	EXTRA="alert.mem = 1" setup snap; start
	waitfor "first snapshot" snap_has 'mem$'
	waitfor "follow-up snapshot" snap_has '10 s later'
	stopd
	check "the alert itself is recorded" has 'ALERT'
	check "only one snapshot per minute (plus its follow-up)" test "$(az snapshots | grep -cE '^ +[0-9]+ ')" -le 3
	check "a snapshot names processes" latest_has 'CPU over 1 s'
}

# Old day files are written by hand: three days of samples, an update with a
# worse idle temperature after it, alerts and a failed timer.
make_history() {
	python3 - "$H/state/azor" <<'P'
import json, sys, time, datetime
d = sys.argv[1]
now = time.time()
def day(t): return datetime.datetime.fromtimestamp(t).strftime('%Y-%m-%d')
files = {}
def put(r):
    r['v'] = 1
    files.setdefault(day(r['t']), []).append(json.dumps(r))
upd = int(now - 3 * 86400)                      # the update, 3 days ago
for t in range(upd - 30 * 3600, upd + 30 * 3600, 60):
    hot = 8 if t >= upd else 0                  # idle CPU 8 C warmer after it
    put({'t': t, 'type': 'sample', 'secs': 60, 'avg': {'cpu': 5, 'cpu_temp': 40 + hot, 'mem': 30}, 'max': {'cpu': 9}})
put({'t': upd, 'type': 'event', 'kind': 'package', 'key': 'upgraded 3', 'new': 'linux-test'})
put({'t': upd + 7200, 'type': 'event', 'kind': 'package', 'key': 'installed 1', 'new': 'some-app'})  # not an update
for i in range(3):
    a = upd + 3600 * (i + 1)
    put({'t': a, 'type': 'event', 'kind': 'alert', 'key': 'psi_memory', 'val': 30})
    put({'t': a, 'type': 'snapshot', 'key': 'psi_memory', 'top_cpu': [{'name': 'hungry', 'cpu': 90}]})
    put({'t': a + 120, 'type': 'event', 'kind': 'clear', 'key': 'psi_memory', 'val': 40, 'secs': 120})
for i, res in enumerate(['ok', 'failed: exit-code', 'ok']):
    put({'t': upd + 7200 * i, 'type': 'event', 'kind': 'timer', 'key': 'backup', 'new': res, 'old': res, 'val': upd + 7200 * i})
for k, v in files.items():
    open(f'{d}/{k}.jsonl', 'w').write('\n'.join(v) + '\n')
print(len(files))
P
}

scenario_longterm() {
	echo "== old day files are summed up before they are removed"
	EXTRA="retention = 1" MODULES="cpu, memory" setup long
	make_history >/dev/null
	local before; before=$(ls "$H/state/azor" | grep -c '\.jsonl$')
	start; stopd
	local after; after=$(ls "$H/state/azor" | grep -cE '^[0-9-]+\.jsonl$')
	check "old day files removed ($before -> $after)" test "$after" -lt "$before"
	check "hourly summaries written" grep -q '"type":"hour"' "$H/state/azor/history.jsonl"
	check "daily summaries written" grep -q '"type":"day"' "$H/state/azor/history.jsonl"
	check "events kept" grep -q '"kind":"package"' "$H/state/azor/history.jsonl"
	local r; r=$(az range "$(date -d '-80 hours' '+%F %H:%M')" "$(date -d '-74 hours' '+%F %H:%M')")
	check "range for an old period has data" grep -qE '[0-9]+ hourly or daily summaries' <<<"$r"
	local c; c=$(az compare update)
	echo "$c" | sed 's/^/      /' | head -12
	check "compare finds the update" grep -q 'linux-test\|upgraded 3' <<<"$c"
	# about 48: the hour of the update mixes minutes from before and after it
	check "compare shows the warmer CPU (40 -> about 48)" grep -qE 'CPU temperature C +40\.0 +4(7\.[5-9]|8\.0)' <<<"$c"
	local p; p=$(az problems)
	echo "$p" | sed 's/^/      /'
	check "problems counts the alerts" grep -qE 'memory stall: 3x' <<<"$p"
	check "problems names the busiest program" grep -q 'hungry (3 of 3' <<<"$p"
	check "problems counts the failed run" grep -q 'backup: 1 failed of 3' <<<"$p"
	check "snapshots of removed days are listed from the history" test "$(az snapshots 30d | grep -c 'psi_memory')" = 3
	start; stopd
	check "summing up is not repeated" test "$(grep -c '"type":"day"' "$H/state/azor/history.jsonl")" -le 4
}

lacks() { ! grep -qiE "$1" <<<"$2"; }
not_flagged() { ! grep -E 'RAM used.*\(' <<<"$1"; }

scenario_compare_raw() {
	echo "== compare on day files: the idle columns"
	MODULES="cpu" setup craw
	make_history >/dev/null
	local c; c=$(az compare update)
	echo "$c" | sed 's/^/      /' | sed -n '2,7p'
	check "idle time is found (all samples are idle)" grep -qE 'before .*idle [23][0-9]h' <<<"$c"
	check "no hourly summaries are claimed without a history file" sh -c "! test -e '$H/state/azor/history.jsonl'"
	check "the source is samples on both sides" test "$(grep -c '(samples)' <<<"$c")" = 2
	check "idle columns show 40 -> 48 and the change" grep -qE 'CPU temperature C +40\.0 +48\.0 +40\.0 +48\.0 +\(\+8\.0\)' <<<"$c"
	check "unchanged metrics are not flagged" not_flagged "$c"
}

scenario_space() {
	echo "== the disk limit: oldest days go early, summed up first; today's file stays"
	EXTRA="max_disk = 10" MODULES="cpu, memory" setup space
	python3 - "$H/state/azor" <<'P'
import json, sys, time, datetime
d, now, pad = sys.argv[1], time.time(), 'x' * 200
for back in (3, 2, 1):  # three old days of about 4 MB each
    base = int(now - back * 86400)
    with open(d + '/' + datetime.datetime.fromtimestamp(base).strftime('%Y-%m-%d') + '.jsonl', 'w') as f:
        for i in range(12000):
            f.write(json.dumps({'v': 1, 't': base - 43200 + i * 3, 'type': 'sample', 'secs': 3, 'avg': {'cpu': 5, 'mem': 30}, 'facts': {'pad': pad}}) + '\n')
P
	local before; before=$(du -sm "$H/state/azor" | cut -f1)
	start; stopd
	local after; after=$(du -sm "$H/state/azor" | cut -f1)
	check "history shrank under the limit (${before} -> ${after} MB)" test "$(du -sb "$H/state/azor" | cut -f1)" -le 10000000
	check "a SPACE event names the removed day" has 'SPACE +day [0-9-]+ removed before the retention'
	check "today's file is kept" test -f "$H/state/azor/$(date +%F).jsonl"
	check "the removed day was summed up first" grep -q '"type":"hour"' "$H/state/azor/history.jsonl"
	check "status shows the share of the limit" grep -qE 'disk limit: [0-9]+% of max_disk = 10 MB' <<<"$(az status)"
	local r; r=$(az range "$(date -d '-3 days -6 hours' '+%F %H:%M')" "$(date -d '-3 days -2 hours' '+%F %H:%M')")
	check "a removed day still answers from hourly summaries" grep -qE '[0-9]+ hourly or daily summaries' <<<"$r"
	local n; n=$(count 'SPACE')
	start
	local b; b=$(az brief)
	stopd
	check "the hook line mentions the disk limit" grep -q 'disk limit: samples of [0-9-]* removed early' <<<"$b"
	check "nothing more is removed below the limit ($n SPACE events)" test "$(count 'SPACE')" = "$n"
}

scenario_snapshot_config() {
	echo "== snapshot settings: no follow-up, fewer processes"
	EXTRA=$'alert.mem = 1\nsnapshot.followup = off\nsnapshot.top = 2' setup snapcfg; start
	waitfor "first snapshot" snap_has 'mem$'
	sleep 13
	stopd
	check "no follow-up snapshot when it is off" test "$(az snapshots | grep -c 'later')" = 0
	local l; l=$(az snapshot latest | grep 'CPU over 1 s')
	check "a snapshot was listed with its processes" test -n "$l"
	check "at most 2 processes listed ($(awk -F', ' '{print NF}' <<<"$l"))" test "$(awk -F', ' '{print NF}' <<<"$l")" -le 2
	local c; c=$(az config)
	check "config shows the snapshot settings" grep -qE 'snapshot.followup = off' <<<"$c"
	check "config shows the disk limit" grep -qE 'max_disk = 200 MB' <<<"$c"
}

scenario_journal() {
	echo "== journal: error counts with their coverage, health checks of the journal"
	MODULES="cpu, journal, health" setup journal; start; sleep 3; stopd
	local r; r=$(az range "$(date -d '-1 hour' '+%F %H:%M')" now)
	check "range counts journal errors" grep -q '^Journal errors' <<<"$r"
	check "range gives the journal coverage" grep -qE '^  journal coverage: (entries for [0-9]+% of the range|unknown)' <<<"$r"
	check "no log message text in the output" lacks 'MESSAGE|"msg' "$r"
	local h; h=$(az health)
	check "health has the journal access check" grep -qE '^  journal_access +(readable|only own entries)|Not available here:.*journal_access' <<<"$h"
	check "health has the journal kept check" grep -qE '^  journal_kept +(under 1 day|1 to 7 days|7 to 30 days|over 30 days)|Not available here:.*journal_kept' <<<"$h"
}

scenario_skill() {
	echo "== install-skill: a plain file, the same as the skill in the repository"
	setup skill
	local o; o=$(az install-skill)
	check "the skill file is installed" cmp -s "$H/.claude/skills/azor/SKILL.md" claude/azor/SKILL.md
	check "a second run changes nothing" grep -q 'up to date' <<<"$(az install-skill)"
	local hook; hook=$(sed -n '/"hooks": {/,/} ] } ] }/p' <<<"$o")
	check "the printed hook is valid JSON with the full path" python3 -c "import json,sys; h=json.loads('{'+sys.argv[1]+'}'); c=h['hooks']['SessionStart'][0]['hooks'][0]['command']; assert c.startswith('\"/') and c.endswith('brief 2>/dev/null || true'), c" "$hook"
	check "settings.json is not created" test ! -e "$H/.claude/settings.json"
}

json_ok() { python3 -c "import json,sys; d=json.load(sys.stdin); assert d['azor_json']==1 and d['kind']=='$1', d.get('kind'); $2"; }

scenario_json() {
	echo "== JSON views and export"
	setup json; start; sleep 6
	az json now | json_ok now "assert 'cpu' in d['data']['values']"; [ $? -eq 0 ] && ok "now has the CPU value" || bad "now has the CPU value"
	az json events 1h | json_ok events "assert any(e['kind']=='start' for e in d['data'])"; [ $? -eq 0 ] && ok "events lists the start" || bad "events lists the start"
	az json health | json_ok health "assert 'problems' in d['data'] and 'not_available' in d['data']"; [ $? -eq 0 ] && ok "health has checks and problems" || bad "health has checks and problems"
	stopd
	local n; n=$(az export 1h | python3 -c "import json,sys; print(sum(1 for l in sys.stdin if json.loads(l)))")
	check "export gives one JSON object per line ($n)" test "${n:-0}" -ge 3
	check "a bad view is an error" sh -c "! env XDG_STATE_HOME='$H/state' '$ROOT/azor' json nonsense"
}

scenarios=("$@")
[ ${#scenarios[@]} -eq 0 ] && scenarios=(stop_start interrupted boot suspend snapshot snapshot_config longterm space journal skill compare_raw json)
for s in "${scenarios[@]}"; do "scenario_$s"; done
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
