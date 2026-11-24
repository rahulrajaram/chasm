package guestexec

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	wsContinuation = 0
	wsText         = 1
	wsBinary       = 2
	wsClose        = 8
	wsPing         = 9
	wsPong         = 10
	wsMagic        = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

type socketDial func(context.Context) (net.Conn, error)

type websocket struct {
	conn         net.Conn
	reader       *bufio.Reader
	writeTimeout time.Duration
	mu           sync.Mutex
}

func hasHeaderToken(value, token string) bool {
	for _, element := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(element), token) {
			return true
		}
	}
	return false
}

func openWebsocket(ctx context.Context, dial socketDial, operation, secret string) (*websocket, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	target := "http://unix.socket" + operation + "/websocket?" + url.Values{"secret": {secret}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", key)
	if err := request.Write(conn); err != nil {
		return nil, err
	}
	handshake := &io.LimitedReader{R: conn, N: 16 << 10}
	reader := bufio.NewReader(handshake)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	digest := sha1.Sum([]byte(key + wsMagic)) // RFC 6455 handshake, not authentication.
	if response.StatusCode != http.StatusSwitchingProtocols ||
		!hasHeaderToken(response.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(response.Header.Get("Upgrade"), "websocket") ||
		response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(digest[:]) ||
		response.Header.Get("Sec-WebSocket-Extensions") != "" {
		return nil, errors.New("Incus websocket upgrade refused")
	}
	handshake.N = 1<<63 - 1
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	success = true
	return &websocket{conn: conn, reader: reader, writeTimeout: 3 * time.Second}, nil
}

func (ws *websocket) write(opcode byte, payload []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	var deadline time.Time
	if ws.writeTimeout > 0 {
		deadline = time.Now().Add(ws.writeTimeout)
	}
	if err := ws.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, 0x80|opcode)
	if len(payload) < 126 {
		frame = append(frame, 0x80|byte(len(payload)))
	} else {
		frame = append(frame, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	frame = append(frame, mask[:]...)
	for index, value := range payload {
		frame = append(frame, value^mask[index%4])
	}
	n, err := ws.conn.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

func (ws *websocket) read() (bool, byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(ws.reader, header[:]); err != nil {
		return false, 0, nil, err
	}
	final, opcode := header[0]&0x80 != 0, header[0]&0x0f
	size := uint64(header[1] & 0x7f)
	if header[0]&0x70 != 0 || header[1]&0x80 != 0 || (opcode >= 8 && (!final || size > 125)) {
		return false, 0, nil, errors.New("invalid Incus websocket frame")
	}
	switch size {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(ws.reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		size = uint64(binary.BigEndian.Uint16(extended[:]))
		if size < 126 {
			return false, 0, nil, errors.New("invalid Incus websocket frame length")
		}
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(ws.reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		size = binary.BigEndian.Uint64(extended[:])
		if size <= 65535 {
			return false, 0, nil, errors.New("invalid Incus websocket frame length")
		}
	}
	if size > MaxChunk {
		return false, 0, nil, errors.New("oversized Incus websocket frame")
	}
	payload := make([]byte, size)
	_, err := io.ReadFull(ws.reader, payload)
	return final, opcode, payload, err
}

func (ws *websocket) receive(consume func([]byte) error) error {
	fragmented := false
	for {
		final, opcode, payload, err := ws.read()
		if err != nil {
			return err
		}
		switch opcode {
		case wsPing:
			if err := ws.write(wsPong, payload); err != nil {
				return err
			}
		case wsPong:
		case wsClose:
			_ = ws.write(wsClose, payload)
			return io.ErrUnexpectedEOF
		case wsText:
			// Incus ends each byte stream with an empty text message.
			if final && !fragmented && len(payload) == 0 && consume != nil {
				return nil
			}
			return errors.New("unexpected Incus websocket text message")
		case wsBinary, wsContinuation:
			if consume == nil || (opcode == wsContinuation) != fragmented {
				return errors.New("unexpected Incus websocket data frame")
			}
			fragmented = !final
			if err := consume(payload); err != nil {
				return err
			}
		default:
			return errors.New("unsupported Incus websocket frame")
		}
	}
}
