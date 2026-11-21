package execserver

import (
	"net"
	"syscall"
)

// peerCredentials mirrors _peer_credentials: SO_PEERCRED over the unix
// socket, returning (pid, uid, gid). The executor gates every connection
// on the peer's uid being the supervisor identity.
func PeerCredentials(connection *net.UnixConn) (int, int, int, error) {
	return peerCredentials(connection)
}

func peerCredentials(connection *net.UnixConn) (int, int, int, error) {
	file, err := connection.File()
	if err != nil {
		return 0, 0, 0, err
	}
	defer file.Close()
	credential, err := syscall.GetsockoptUcred(int(file.Fd()), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, 0, 0, err
	}
	return int(credential.Pid), int(credential.Uid), int(credential.Gid), nil
}
