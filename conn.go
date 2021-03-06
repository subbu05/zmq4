// Copyright 2018 The go-zeromq Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zmq4

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"sync"

	"github.com/pkg/errors"
)

// Conn implements the ZeroMQ Message Transport Protocol as defined
// in https://rfc.zeromq.org/spec:23/ZMTP/.
type Conn struct {
	typ    SocketType
	id     SocketIdentity
	rw     io.ReadWriteCloser
	sec    Security
	Server bool
	Meta   Metadata
	Peer   struct {
		Server bool
		Meta   Metadata
	}

	mu     sync.RWMutex
	topics map[string]struct{} // set of subscribed topics
}

func (c *Conn) Close() error {
	return c.rw.Close()
}

func (c *Conn) Read(p []byte) (int, error) {
	return io.ReadFull(c.rw, p)
}

func (c *Conn) Write(p []byte) (int, error) {
	return c.rw.Write(p)
}

// Open opens a ZMTP connection over rw with the given security, socket type and identity.
// Open performs a complete ZMTP handshake.
func Open(rw io.ReadWriteCloser, sec Security, sockType SocketType, sockID SocketIdentity, server bool) (*Conn, error) {
	if rw == nil {
		return nil, errors.Errorf("zmq4: invalid nil read-writer")
	}

	if sec == nil {
		return nil, errors.Errorf("zmq4: invalid nil security")
	}

	conn := &Conn{
		typ:    sockType,
		id:     sockID,
		rw:     rw,
		sec:    sec,
		Server: server,
		Meta:   make(Metadata),
		topics: make(map[string]struct{}),
	}
	conn.Meta[sysSockType] = string(conn.typ)
	conn.Meta[sysSockID] = conn.id.String()
	conn.Peer.Meta = make(Metadata)

	err := conn.init(sec)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// init performs a ZMTP handshake over an io.ReadWriter
func (conn *Conn) init(sec Security) error {
	var err error

	err = conn.greet(conn.Server)
	if err != nil {
		return errors.Wrapf(err, "zmq4: could not exchange greetings")
	}

	err = conn.sec.Handshake(conn, conn.Server)
	if err != nil {
		return errors.Wrapf(err, "zmq4: could not perform security handshake")
	}

	peer := SocketType(conn.Peer.Meta[sysSockType])
	if !peer.IsCompatible(conn.typ) {
		return errors.Errorf("zmq4: peer=%q not compatible with %q", peer, conn.typ)
	}

	// FIXME(sbinet): if security mechanism does not define a client/server
	// topology, enforce that p.server == p.peer.server == 0
	// as per:
	//  https://rfc.zeromq.org/spec:23/ZMTP/#topology

	return nil
}

func (conn *Conn) greet(server bool) error {
	var err error
	send := greeting{Version: defaultVersion}
	send.Sig.Header = sigHeader
	send.Sig.Footer = sigFooter
	kind := string(conn.sec.Type())
	if len(kind) > len(send.Mechanism) {
		return errSecMech
	}
	copy(send.Mechanism[:], kind)

	err = send.write(conn.rw)
	if err != nil {
		return errors.Wrapf(err, "zmq4: could not send greeting")
	}

	var recv greeting
	err = recv.read(conn.rw)
	if err != nil {
		return errors.Wrapf(err, "zmq4: could not recv greeting")
	}

	peerKind := asString(recv.Mechanism[:])
	if peerKind != kind {
		return errBadSec
	}

	conn.Peer.Server, err = asBool(recv.Server)
	if err != nil {
		return errors.Wrapf(err, "zmq4: could not get peer server flag")
	}

	return nil
}

// SendCmd sends a ZMTP command over the wire.
func (c *Conn) SendCmd(name string, body []byte) error {
	cmd := Cmd{Name: name, Body: body}
	buf, err := cmd.marshalZMTP()
	if err != nil {
		return err
	}
	return c.send(true, buf, 0)
}

// SendMsg sends a ZMTP message over the wire.
func (c *Conn) SendMsg(msg Msg) error {
	nframes := len(msg.Frames)
	for i, frame := range msg.Frames {
		var flag byte
		if i < nframes-1 {
			flag ^= hasMoreBitFlag
		}
		err := c.send(false, frame, flag)
		if err != nil {
			return errors.Wrapf(err, "zmq4: error sending frame %d/%d", i+1, nframes)
		}
	}
	return nil
}

// RecvMsg receives a ZMTP message from the wire.
func (c *Conn) RecvMsg() (Msg, error) {
	msg := c.read()
	if msg.err != nil {
		return msg, errors.WithStack(msg.err)
	}

	if !msg.isCmd() {
		return msg, nil
	}

	switch len(msg.Frames) {
	case 0:
		msg.err = errors.Errorf("zmq4: empty command")
		return msg, msg.err
	case 1:
		// ok
	default:
		msg.err = errors.Errorf("zmq4: invalid length command")
		return msg, msg.err
	}

	var cmd Cmd
	msg.err = cmd.unmarshalZMTP(msg.Frames[0])
	if msg.err != nil {
		return msg, errors.WithStack(msg.err)
	}

	switch cmd.Name {
	case CmdPing:
		// send back a PONG immediately.
		msg.err = c.SendCmd(CmdPong, nil)
		if msg.err != nil {
			return msg, msg.err
		}
	}

	switch len(cmd.Body) {
	case 0:
		msg.Frames = nil
	default:
		msg.Frames = msg.Frames[:1]
		msg.Frames[0] = cmd.Body
	}
	return msg, nil
}

func (c *Conn) RecvCmd() (Cmd, error) {
	var cmd Cmd
	msg := c.read()
	if msg.err != nil {
		return cmd, errors.WithStack(msg.err)
	}

	if !msg.isCmd() {
		return cmd, ErrBadFrame
	}

	switch len(msg.Frames) {
	case 0:
		msg.err = errors.Errorf("zmq4: empty command")
		return cmd, msg.err
	case 1:
		// ok
	default:
		msg.err = errors.Errorf("zmq4: invalid length command")
		return cmd, msg.err
	}

	err := cmd.unmarshalZMTP(msg.Frames[0])
	if err != nil {
		return cmd, errors.WithStack(err)
	}

	return cmd, nil
}

func (c *Conn) send(isCommand bool, body []byte, flag byte) error {
	// Long flag
	size := len(body)
	isLong := size > 255
	if isLong {
		flag ^= isLongBitFlag
	}

	if isCommand {
		flag ^= isCommandBitFlag
	}

	var (
		hdr = [8 + 1]byte{flag}
		hsz int
	)

	// Write out the message itself
	if isLong {
		hsz = 9
		binary.BigEndian.PutUint64(hdr[1:], uint64(size))
	} else {
		hsz = 2
		hdr[1] = uint8(size)
	}
	if _, err := c.rw.Write(hdr[:hsz]); err != nil {
		return err
	}

	if _, err := c.sec.Encrypt(c.rw, body); err != nil {
		return err
	}

	return nil
}

// read returns the isCommand flag, the body of the message, and optionally an error
func (c *Conn) read() Msg {
	var (
		header  [2]byte
		longHdr [8]byte
		msg     Msg

		hasMore = true
		isCmd   = false
	)

	for hasMore {

		// Read out the header
		_, msg.err = io.ReadFull(c.rw, header[:])
		if msg.err != nil {
			return msg
		}

		fl := flag(header[0])

		hasMore = fl.hasMore()
		isCmd = isCmd || fl.isCommand()

		// Determine the actual length of the body
		size := uint64(header[1])
		if fl.isLong() {
			// We read 2 bytes of the header already
			// In case of a long message, the length is bytes 2-8 of the header
			// We already have the first byte, so assign it, and then read the rest
			longHdr[0] = header[1]

			_, msg.err = io.ReadFull(c.rw, longHdr[1:])
			if msg.err != nil {
				return msg
			}

			size = binary.BigEndian.Uint64(longHdr[:])
		}

		if size > uint64(maxInt64) {
			msg.err = errOverflow
			return msg
		}

		body := make([]byte, size)
		_, msg.err = io.ReadFull(c.rw, body)
		if msg.err != nil {
			return msg
		}

		// fast path for NULL security: we bypass the bytes.Buffer allocation.
		switch c.sec.Type() {
		case NullSecurity: // FIXME(sbinet): also do that for non-encrypted PLAIN?
			msg.Frames = append(msg.Frames, body)
			continue
		}

		buf := new(bytes.Buffer)
		if _, msg.err = c.sec.Decrypt(buf, body); msg.err != nil {
			return msg
		}
		msg.Frames = append(msg.Frames, buf.Bytes())
	}
	if isCmd {
		msg.Type = CmdMsg
	}
	return msg
}

func (conn *Conn) subscribe(msg Msg) {
	conn.mu.Lock()
	v := msg.Frames[0]
	k := string(v[1:])
	switch v[0] {
	case 0:
		delete(conn.topics, k)
	case 1:
		conn.topics[k] = struct{}{}
	}
	conn.mu.Unlock()
}

func (conn *Conn) subscribed(topic string) bool {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	for k := range conn.topics {
		switch {
		case k == "":
			// subscribed to everything
			return true
		case strings.HasPrefix(topic, k):
			return true
		}
	}
	return false
}
