// Phase 0 WebSocket spike — throwaway diagnostic.
//
// Purpose: prove that a Bunny Magic Container CDN endpoint (with the Pull Zone
// WebSockets toggle enabled) passes a real WebSocket upgrade through to this
// container. This is the ONE thing the Bunny docs cannot confirm for our zone.
//
// It is deliberately self-contained: the WebSocket handshake and framing are
// implemented against the Go standard library (RFC 6455), so it adds NO new
// module dependency and builds with the existing Dockerfile unchanged. The real
// realtime hub will use github.com/coder/websocket — do NOT model production
// code on this file. Delete it once the edge path is confirmed.
//
// Gated behind WS_ECHO_ENABLED=1 so it is inert unless explicitly turned on.
//
//	GET /ws/echo   — upgrade to WebSocket, echo every text/binary frame back,
//	                 reply to ping with pong, send periodic server pings.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RFC 6455 magic GUID used to derive the Sec-WebSocket-Accept response value.
const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// handleWSEcho performs the WebSocket handshake by hand, then echoes frames.
// Registered in main() but inert unless WS_ECHO_ENABLED=1.
func handleWSEcho(w http.ResponseWriter, r *http.Request) {
	if getEnv("WS_ECHO_ENABLED", "") != "1" {
		http.NotFound(w, r)
		return
	}

	// Validate the upgrade request.
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}

	// Take over the raw TCP connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		log.Printf("ws/echo hijack failed: %v", err)
		return
	}
	defer conn.Close()

	// Complete the handshake.
	accept := computeAcceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		log.Printf("ws/echo handshake write failed: %v", err)
		return
	}
	if err := brw.Flush(); err != nil {
		log.Printf("ws/echo handshake flush failed: %v", err)
		return
	}
	log.Printf("ws/echo: client connected from %s", r.RemoteAddr)

	// All writes go through send(), serialized by a mutex: the keepalive
	// goroutine and the read loop both write, and concurrent writes to one
	// connection would race.
	var wmu sync.Mutex
	send := func(opcode byte, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := writeFrame(brw.Writer, opcode, payload); err != nil {
			return err
		}
		return brw.Flush()
	}

	// Greet so a browser test sees data immediately, confirming the edge path.
	_ = send(opText, []byte("connected: bunny edge passed the websocket upgrade"))

	// Server-initiated pings keep the connection alive through any idle timeout.
	stopPing := make(chan struct{})
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				if send(opPing, nil) != nil {
					return
				}
			}
		}
	}()
	defer close(stopPing)

	// Read loop.
	for {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		opcode, payload, err := readFrame(brw.Reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("ws/echo read closed: %v", err)
			}
			return
		}
		switch opcode {
		case opText, opBinary:
			if send(opcode, payload) != nil {
				return
			}
		case opPing:
			if send(opPong, payload) != nil {
				return
			}
		case opPong:
			// keepalive ack — nothing to do.
		case opClose:
			_ = send(opClose, payload)
			return
		}
	}
}

func computeAcceptKey(clientKey string) string {
	h := sha1.New()
	io.WriteString(h, clientKey+wsMagicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// readFrame reads a single (unfragmented-or-final) frame from a client.
// Client frames are always masked per RFC 6455 §5.3; we unmask in place.
func readFrame(r *bufio.Reader) (opcode byte, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err = io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	opcode = header[0] & 0x0F
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7F)

	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

// writeFrame writes a single final, unmasked server frame (RFC 6455 §5.1:
// server-to-client frames must not be masked).
func writeFrame(w *bufio.Writer, opcode byte, payload []byte) error {
	if err := w.WriteByte(0x80 | opcode); err != nil { // FIN=1
		return err
	}
	n := len(payload)
	switch {
	case n < 126:
		if err := w.WriteByte(byte(n)); err != nil {
			return err
		}
	case n <= 0xFFFF:
		if err := w.WriteByte(126); err != nil {
			return err
		}
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(n))
		if _, err := w.Write(ext); err != nil {
			return err
		}
	default:
		if err := w.WriteByte(127); err != nil {
			return err
		}
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		if _, err := w.Write(ext); err != nil {
			return err
		}
	}
	_, err := w.Write(payload)
	return err
}
