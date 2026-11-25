package guestexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
func (client *Client) confirmMarker(name, token string, uid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	post := execPost{Command: []string{"/bin/bash", "--noprofile", "--norc", "-p", "-c", markerReadback, "chasm-status", token}, User: uid, Group: uid, Cwd: "/"}
	reply, err := client.query(ctx, http.MethodPost, "/1.0/instances/"+name+"/exec", post)
	var op operation
	if err != nil || reply.Type != "async" || reply.StatusCode != 100 || json.Unmarshal(reply.Metadata, &op) != nil ||
		!operationID.MatchString(op.ID) || !validOperationPath(reply.Operation, op.ID) {
		return false
	}
	result := client.wait(ctx, op.ID)
	return result.GuestStatus != nil && *result.GuestStatus == 0
}
