#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
    cat <<'EOF'
Usage: ./server.sh [toggle|start|stop|restart|status|logs] [serve options]

  toggle   Start if stopped, stop if running (default)
  start    Start in the background; accepts serve options such as --listen
  stop     Stop the managed process gracefully
  restart  Stop and start; accepts serve options
  status   Show the managed process status (exit 3 when stopped)
  logs     Show the last 100 log lines

Environment:
  FNCONNECT_PROXY_BIN      Executable path (default: ./dist/fnconnect-proxy)
  FNCONNECT_PROXY_RUN_DIR  PID/log directory (default: ./.run)
  FNCONNECT_PROXY_CONFIG   Login configuration path, passed to the proxy

Requires Linux, Bash, flock, and a built fnconnect-proxy executable.
EOF
}

action=${1:-toggle}
if (($#)); then shift; fi
case "$action" in
    help|-h|--help) usage; exit 0 ;;
    toggle|start|restart) ;;
    stop|status|logs)
        if (($#)); then usage >&2; exit 2; fi
        ;;
    *) usage >&2; exit 2 ;;
esac

if [[ $(uname -s) != Linux ]] || ! command -v flock >/dev/null 2>&1; then
    echo 'This switch requires Linux and flock. Use fnconnect-proxy serve for foreground operation.' >&2
    exit 1
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
run_dir=${FNCONNECT_PROXY_RUN_DIR:-"$script_dir/.run"}
mkdir -p -- "$run_dir"
run_dir=$(cd -- "$run_dir" && pwd)
pid_file="$run_dir/server.pid"
log_file="$run_dir/server.log"
boot_id=$(cat /proc/sys/kernel/random/boot_id)

# The kernel releases this lock even if the switch is interrupted. The proxy
# must not inherit descriptor 9, or it would retain the management lock.
exec 9>"$run_dir/server.lock"
if ! flock -w 10 9; then
    echo 'Another start/stop operation is still running. Try again shortly.' >&2
    exit 1
fi

process_identity() {
    local pid=$1 stat_line rest
    local -a fields
    [[ $pid =~ ^[1-9][0-9]*$ ]] || return 1
    stat_line=$(cat -- "/proc/$pid/stat" 2>/dev/null) || return 1
    # The process name in parentheses may contain spaces or closing parentheses.
    rest=${stat_line##*) }
    read -r -a fields <<< "$rest"
    ((${#fields[@]} >= 20)) || return 1
    current_state=${fields[0]}
    current_start=${fields[19]}
    [[ $current_state != Z && $current_state != X ]]
}

is_running() {
    local saved_start saved_boot
    [[ -f $pid_file ]] || return 1
    read -r managed_pid saved_start saved_boot < "$pid_file" || return 1
    [[ $saved_start =~ ^[0-9]+$ ]] || return 1
    [[ $saved_boot == "$boot_id" ]] || return 1
    process_identity "$managed_pid" || return 1
    [[ $current_start == "$saved_start" ]] && kill -0 -- "$managed_pid" 2>/dev/null
}

start_server() {
    local binary child_pid child_start
    if is_running; then
        echo "running (PID $managed_pid)"
        return 0
    fi
    binary=${FNCONNECT_PROXY_BIN:-"$script_dir/dist/fnconnect-proxy"}
    if [[ ! -f $binary || ! -x $binary ]]; then
        echo "Executable not found: $binary" >&2
        echo 'Build in fnconnect-proxy/: go build -o dist/fnconnect-proxy ./cmd/fnconnect-proxy' >&2
        return 1
    fi
    rm -f -- "$pid_file"
    nohup "$binary" serve "$@" </dev/null >>"$log_file" 2>&1 9>&- &
    child_pid=$!
    if ! process_identity "$child_pid"; then
        wait "$child_pid" || true
        echo "Startup failed; see $log_file" >&2
        return 1
    fi
    child_start=$current_start
    if ! printf '%s %s %s\n' "$child_pid" "$child_start" "$boot_id" > "$pid_file.tmp.$$" ||
        ! mv -f -- "$pid_file.tmp.$$" "$pid_file"; then
        kill -TERM -- "$child_pid" 2>/dev/null || true
        rm -f -- "$pid_file.tmp.$$"
        return 1
    fi
    sleep 1
    if ! is_running; then
        rm -f -- "$pid_file"
        echo "Startup failed; see $log_file" >&2
        tail -n 20 -- "$log_file" >&2
        return 1
    fi
    echo "started (PID $managed_pid)"
    echo "log: $log_file"
}

stop_server() {
    local attempt
    if ! is_running; then
        rm -f -- "$pid_file"
        echo 'stopped'
        return 0
    fi
    if ! kill -TERM -- "$managed_pid" 2>/dev/null && is_running; then
        echo "Could not stop PID $managed_pid" >&2
        return 1
    fi
    for ((attempt = 0; attempt < 100; attempt++)); do
        if ! is_running; then
            rm -f -- "$pid_file"
            echo 'stopped'
            return 0
        fi
        sleep 0.1
    done
    echo "PID $managed_pid is still stopping; check $log_file" >&2
    return 1
}

case "$action" in
    toggle)
        if is_running; then stop_server; else start_server "$@"; fi
        ;;
    start) start_server "$@" ;;
    stop) stop_server ;;
    restart) stop_server; start_server "$@" ;;
    status)
        if is_running; then echo "running (PID $managed_pid)"; else echo 'stopped'; exit 3; fi
        ;;
    logs)
        if [[ -f $log_file ]]; then tail -n 100 -- "$log_file"; else echo 'No log yet.'; fi
        ;;
esac
