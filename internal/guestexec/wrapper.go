package guestexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
)

// Bash job control gives the command its own process group without the
// ignored INT/QUIT dispositions (or /dev/null stdin) of a POSIX async list.
// -p suppresses BASH_ENV, imported functions and inherited shell options.
const execWrapper = `token=$1
delay=$2
shift 2
set -m
child=
watchdog=
pending=
interrupted=0
forward() {
    interrupted=1
    pending=$1
    if [ -n "$child" ]; then
        kill -s "$1" -- "-$child" 2>/dev/null || :
        if [ -z "$watchdog" ]; then
            ( /bin/sleep "$delay"; kill -KILL -- "-$child" 2>/dev/null || : ) </dev/null >/dev/null 2>&1 &
            watchdog=$!
        fi
    fi
}
trap 'forward TERM' TERM
trap 'forward HUP' HUP
trap 'forward INT' INT
( exec "$@" ) <&0 &
child=$!
if [ -n "$pending" ]; then forward "$pending"; fi
while :; do
    interrupted=0
    wait -f "$child" 2>/dev/null
    status=$?
    [ "$interrupted" = 0 ] && break
done
if [ -n "$watchdog" ]; then
    while :; do
        interrupted=0
        wait -f "$watchdog" 2>/dev/null
        [ "$interrupted" = 0 ] && break
    done
fi
if [ "$status" = 129 ]; then
    ( umask 077; set -C; : > "/tmp/.chasm-exec-$token" ) 2>/dev/null || :
fi
exit "$status"
`

const markerReadback = `p=/tmp/.chasm-exec-$1; [ -f "$p" ] && [ ! -L "$p" ] && /bin/rm -- "$p"`

func (client *Client) wrappedArgv(token string, argv []string) []string {
	return append([]string{"/bin/bash", "--noprofile", "--norc", "-p", "-c", execWrapper, "chasm-exec", token,
		fmt.Sprintf("%.3f", (client.grace.Seconds() * 0.9))}, argv...)
}

// Readback has no websocket streams and one deadline for dispatch and wait.
// Its argv contains only a fixed script and our random token, never user data.
// Incus 6.0 VMs only run a stream-less exec with record-output, so the
// (empty) recorded logs are deleted afterwards on a best-effort basis.
func (client *Client) confirmMarker(name, token string, uid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	post := execPost{Command: []string{"/bin/bash", "--noprofile", "--norc", "-p", "-c", markerReadback, "chasm-status", token},
		User: uid, Group: uid, Cwd: "/", RecordOutput: true}
	reply, err := client.query(ctx, http.MethodPost, "/1.0/instances/"+name+"/exec", post)
	var op operation
	if err != nil || reply.Type != "async" || reply.StatusCode != 100 || json.Unmarshal(reply.Metadata, &op) != nil ||
		!operationID.MatchString(op.ID) || !validOperationPath(reply.Operation, op.ID) {
		return false
	}
	id := op.ID
	reply, err = client.query(ctx, http.MethodGet, "/1.0/operations/"+id+"/wait", nil)
	op = operation{}
	if err != nil || reply.Type != "sync" || reply.StatusCode != 200 || json.Unmarshal(reply.Metadata, &op) != nil || op.ID != id {
		return false
	}
	logs := regexp.MustCompile(`^/1\.0/instances/` + regexp.QuoteMeta(name) + `/logs/exec-output/exec_[0-9a-f-]{36}\.std(out|err)$`)
	for _, path := range op.Metadata.Output {
		if logs.MatchString(path) {
			_, _ = client.query(ctx, http.MethodDelete, path, nil)
		}
	}
	return op.StatusCode == 200 && op.Error == "" && op.Metadata.Return != nil && *op.Metadata.Return == 0
}
