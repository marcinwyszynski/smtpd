package smtpd

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

type cmdHandler func(*session, command)

var (
	cmdMap = map[string]cmdHandler{
		"HELO":     (*session).handleHELO,
		"EHLO":     (*session).handleEHLO,
		"MAIL":     (*session).handleMAIL,
		"RCPT":     (*session).handleRCPT,
		"STARTTLS": (*session).handleSTARTTLS,
		"DATA":     (*session).handleDATA,
		"RSET":     (*session).handleRSET,
		"NOOP":     (*session).handleNOOP,
		"QUIT":     (*session).handleQUIT,
		"AUTH":     (*session).handleAUTH,
		"XCLIENT":  (*session).handleXCLIENT,
	}

	authMap = map[string]cmdHandler{
		"LOGIN": (*session).authLOGIN,
		"PLAIN": (*session).authPLAIN,
	}
)

type command struct {
	line   string
	action string
	fields []string
	params []string
}

func parseLine(line string) (cmd command) {
	cmd.line = line
	cmd.fields = strings.Fields(line)
	if len(cmd.fields) > 0 {
		cmd.action = strings.ToUpper(cmd.fields[0])
		if len(cmd.fields) > 1 {
			cmd.params = strings.Split(cmd.fields[1], ":")
		}
	}
	return
}

func (s *session) handle(line string) {
	cmd := parseLine(line)
	action, exists := cmdMap[cmd.action]
	if !exists {
		s.reply(StatusCommandNotImplemented, "Unsupported command")
		return
	}
	action(s, cmd)
}

func (s *session) handleHELO(cmd command) {
	if len(cmd.fields) < 2 {
		s.reply(StatusParameterNotImplemented, "Missing parameter")
		return
	}
	if s.peer.HeloName != "" {
		s.reset() // Reset envelope in case of duplicate HELO
	}
	if s.server.HeloChecker != nil {
		err := s.server.HeloChecker(s.peer, cmd.fields[1])
		if err != nil {
			s.error(err)
			return
		}
	}
	s.peer.HeloName = cmd.fields[1]
	s.peer.Protocol = SMTP
	s.reply(StatusOK, "Go ahead")
}

func (s *session) handleEHLO(cmd command) {
	if len(cmd.fields) < 2 {
		s.reply(StatusParameterNotImplemented, "Missing parameter")
		return
	}
	if s.peer.HeloName != "" {
		// Reset envelope in case of duplicate EHLO
		s.reset()
	}
	if s.server.HeloChecker != nil {
		err := s.server.HeloChecker(s.peer, cmd.fields[1])
		if err != nil {
			s.error(err)
			return
		}
	}
	s.peer.HeloName = cmd.fields[1]
	s.peer.Protocol = ESMTP
	fmt.Fprintf(s.writer, "250-%s\r\n", s.server.Hostname)
	extensions := s.extensions()
	if len(extensions) > 1 {
		for _, ext := range extensions[:len(extensions)-1] {
			fmt.Fprintf(s.writer, "250-%s\r\n", ext)
		}
	}
	s.reply(StatusOK, extensions[len(extensions)-1])
}

func (s *session) handleMAIL(cmd command) {
	if s.peer.HeloName == "" {
		s.reply(StatusBadSequence, "Please introduce yourself first.")
		return
	}
	if !s.tls && s.server.ForceTLS {
		s.reply(
			StatusBadSequence,
			"Please turn on TLS by issuing a STARTTLS command.",
		)
		return
	}
	if s.envelope != nil {
		s.reply(StatusBadSequence, "Duplicate MAIL")
		return
	}
	addr, err := parseAddress(cmd.params[1])
	if err != nil {
		s.reply(
			StatusMailboxNameNotAllowed,
			"Ill-formatted e-mail address",
		)
		return
	}
	if s.server.SenderChecker != nil {
		err = s.server.SenderChecker(s.peer, addr)
		if err != nil {
			s.error(err)
			return
		}
	}
	s.envelope = &Envelope{Sender: addr}
	s.reply(StatusOK, "Go ahead")
}

func (s *session) handleRCPT(cmd command) {
	if s.envelope == nil {
		s.reply(StatusSyntaxError, "Missing MAIL FROM command.")
		return
	}
	if len(s.envelope.Recipients) >= s.server.MaxRecipients {
		s.reply(StatusInsufficientStorage, "Too many recipients")
		return
	}
	addr, err := parseAddress(cmd.params[1])
	if err != nil {
		s.reply(StatusSyntaxError, "Ill-formatted e-mail address")
		return
	}
	if s.server.RecipientChecker != nil {
		err = s.server.RecipientChecker(s.peer, addr)
		if err != nil {
			s.error(err)
			return
		}
	}
	s.envelope.Recipients = append(s.envelope.Recipients, addr)
	s.reply(StatusOK, "Go ahead")
}

func (s *session) handleSTARTTLS(cmd command) {
	if s.tls {
		s.reply(StatusSyntaxError, "Already running in TLS")
		return
	}
	if s.server.TLSConfig == nil {
		s.reply(StatusCommandNotImplemented, "TLS not supported")
		return
	}
	tlsConn := tls.Server(s.conn, s.server.TLSConfig)
	s.reply(StatusServiceReady, "Go ahead")
	if err := tlsConn.Handshake(); err != nil {
		s.reply(StatusMailboxPermanentlyUnavailable, "Handshake error")
		return
	}

	// Reset envelope as a new EHLO/HELO is required after STARTTLS
	s.reset()

	// Reset deadlines on the underlying connection before I replace it
	// with a TLS connection
	s.conn.SetDeadline(time.Time{})

	// Replace connection with a TLS connection
	s.conn = tlsConn
	s.reader = bufio.NewReader(tlsConn)
	s.writer = bufio.NewWriter(tlsConn)
	s.scanner = bufio.NewScanner(s.reader)
	s.tls = true

	// Save connection state on peer
	state := tlsConn.ConnectionState()
	s.peer.TLS = &state

	// Flush the connection to set new timeout deadlines
	s.flush()
	return
}

func (s *session) handleDATA(cmd command) {
	if s.envelope == nil || len(s.envelope.Recipients) == 0 {
		s.reply(StatusSyntaxError, "Missing RCPT TO command.")
		return
	}
	s.reply(
		StatusStartMailInput,
		"Go ahead. End your data with <CR><LF>.<CR><LF>",
	)
	s.conn.SetDeadline(time.Now().Add(s.server.DataTimeout))
	data := &bytes.Buffer{}
	reader := textproto.NewReader(s.reader).DotReader()
	_, err := io.CopyN(data, reader, int64(s.server.MaxMessageSize))
	if err == io.EOF {
		// EOF was reached before MaxMessageSize
		// Accept and deliver message
		s.envelope.Data = data.Bytes()
		if err := s.deliver(); err != nil {
			s.error(err)
		} else {
			s.reply(StatusOK, "Thank you.")
		}
		s.reset()
	}
	if err != nil {
		// Network error, ignore
		return
	}
	// Discard the rest and report an error.
	_, err = io.Copy(ioutil.Discard, reader)
	if err != nil {
		// Network error, ignore
		return
	}
	s.reply(StatusExceededStorageAllocation, fmt.Sprintf(
		"Message exceeded max message size of %d bytes",
		s.server.MaxMessageSize,
	))
	s.reset()
}

func (s *session) handleRSET(cmd command) {
	s.reset()
	s.reply(StatusOK, "Go ahead")
}

func (s *session) handleNOOP(cmd command) {
	s.reply(StatusOK, "Go ahead")
}

func (s *session) handleQUIT(cmd command) {
	s.reply(StatusServiceClosing, "OK, bye")
	s.close()
}

func (s *session) handleAUTH(cmd command) {
	if s.server.Authenticator == nil {
		s.reply(StatusCommandNotImplemented, "AUTH not supported.")
		return
	}
	if s.peer.HeloName == "" {
		s.reply(StatusBadSequence, "Please introduce yourself first.")
		return
	}
	if !s.tls {
		s.reply(
			StatusSyntaxError,
			"Cannot AUTH in plain text mode. Use STARTTLS.",
		)
		return
	}
	mechanism := strings.ToUpper(cmd.fields[1])
	action, exists := authMap[mechanism]
	if !exists {
		s.reply(
			StatusCommandNotImplemented,
			"Unknown authentication mechanism",
		)
		return
	}
	action(s, cmd)
}

func (s *session) handleXCLIENT(cmd command) {
	if !s.server.EnableXCLIENT {
		s.reply(
			StatusMailboxPermanentlyUnavailable,
			"XCLIENT not enabled",
		)
		return
	}
	var (
		newHeloName = ""
		newAddr     net.IP
		newTCPPort  uint64
		newUsername          = ""
		newProto    Protocol = ""
	)
	for _, item := range cmd.fields[1:] {
		parts := strings.Split(item, "=")
		if len(parts) != 2 {
			s.reply(
				StatusSyntaxError,
				"Couldn't decode the command.",
			)
			return
		}
		name, value := parts[0], parts[1]
		switch name {
		case "NAME":
			// Unused in smtpd package
			continue
		case "HELO":
			newHeloName = value
			continue
		case "ADDR":
			newAddr = net.ParseIP(value)
			continue
		case "PORT":
			var err error
			newTCPPort, err = strconv.ParseUint(value, 10, 16)
			if err != nil {
				s.reply(
					StatusSyntaxError,
					"Couldn't decode the command.",
				)
				return
			}
			continue
		case "LOGIN":
			newUsername = value
			continue
		case "PROTO":
			if value == "SMTP" {
				newProto = SMTP
			} else if value == "ESMTP" {
				newProto = ESMTP
			}
			continue
		default:
			s.reply(
				StatusSyntaxError,
				"Couldn't decode the command.",
			)
			return
		}
	}
	tcpAddr, ok := s.peer.Addr.(*net.TCPAddr)
	if !ok {
		s.reply(
			StatusCommandNotImplemented,
			"Unsupported network connection",
		)
		return
	}
	if newHeloName != "" {
		s.peer.HeloName = newHeloName
	}
	if newAddr != nil {
		tcpAddr.IP = newAddr
	}
	if newTCPPort != 0 {
		tcpAddr.Port = int(newTCPPort)
	}
	if newUsername != "" {
		s.peer.Username = newUsername
	}
	if newProto != "" {
		s.peer.Protocol = newProto
	}
	s.welcome()
}

func (s *session) authLOGIN(cmd command) {
	s.reply(StatusProvideCredentials, "VXNlcm5hbWU6")
	if !s.scanner.Scan() {
		return
	}
	byteUsername, err := base64.StdEncoding.DecodeString(s.scanner.Text())
	if err != nil {
		s.reply(StatusSyntaxError, "Couldn't decode your credentials")
		return
	}
	s.reply(StatusProvideCredentials, "UGFzc3dvcmQ6")
	if !s.scanner.Scan() {
		return
	}
	bytePassword, err := base64.StdEncoding.DecodeString(s.scanner.Text())
	if err != nil {
		s.reply(StatusSyntaxError, "Couldn't decode your credentials")
		return
	}
	s.authenticate(string(byteUsername), string(bytePassword))
}

func (s *session) authPLAIN(cmd command) {
	auth := ""
	if len(cmd.fields) < 3 {
		s.reply(StatusProvideCredentials, "Give me your credentials")
		if !s.scanner.Scan() {
			return
		}
		auth = s.scanner.Text()
	} else {
		auth = cmd.fields[2]
	}
	data, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		s.reply(StatusSyntaxError, "Couldn't decode your credentials")
		return
	}
	parts := bytes.Split(data, []byte{0})
	if len(parts) != 3 {
		s.reply(StatusSyntaxError, "Couldn't decode your credentials")
		return
	}
	s.authenticate(string(parts[1]), string(parts[2]))
}

func (s *session) authenticate(user, pass string) {
	if err := s.server.Authenticator(s.peer, user, pass); err != nil {
		s.error(err)
		return
	}
	s.peer.Username = user
	s.peer.Password = pass
	s.reply(StatusAuthenticated, "OK, you are now authenticated")
}
