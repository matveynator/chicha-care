package mail

// Package mail manages outgoing messages and embeds a minimal SMTP server.
// We keep everything asynchronous and channel-based to respect the project rules.

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strings"
	"time"
)

// Message represents a simple email message body.
type Message struct {
	From       string
	To         string
	Subject    string
	Body       string
	ReceivedAt time.Time
}

// Service orchestrates deliveries and the SMTP listener.
type Service struct {
	deliveries chan Message
	requests   chan interface{}
	smtpConns  chan net.Conn
	smtpCtrl   chan interface{}
}

// NewService creates a new mail service.
func NewService(buffer int) *Service {
	svc := &Service{
		deliveries: make(chan Message, buffer),
		requests:   make(chan interface{}),
		smtpConns:  make(chan net.Conn),
		smtpCtrl:   make(chan interface{}),
	}
	go svc.loop()
	go svc.acceptLoop()
	go svc.smtpManager()
	return svc
}

// loop stores messages in mailboxes and handles queries.
func (s *Service) loop() {
	mailboxes := map[string][]Message{}
	for {
		select {
		case msg := <-s.deliveries:
			msg.ReceivedAt = time.Now().UTC()
			key := strings.ToLower(msg.To)
			mailboxes[key] = append(mailboxes[key], msg)
		case req := <-s.requests:
			switch msg := req.(type) {
			case listMailboxRequest:
				box := append([]Message(nil), mailboxes[strings.ToLower(msg.Address)]...)
				msg.Resp <- box
			}
		}
	}
}

// acceptLoop listens for new SMTP connections and delegates handling.
func (s *Service) acceptLoop() {
	for conn := range s.smtpConns {
		go s.handleSMTP(conn)
	}
}

type startSMTP struct {
	Addr string
	Resp chan error
}

type stopSMTP struct {
	Resp chan struct{}
}

// smtpManager owns the SMTP listener lifecycle so we avoid shared state.
func (s *Service) smtpManager() {
	var listener net.Listener
	var acceptDone chan struct{}
	for msg := range s.smtpCtrl {
		switch cmd := msg.(type) {
		case startSMTP:
			if listener != nil {
				cmd.Resp <- fmt.Errorf("SMTP server already running")
				continue
			}
			ln, err := net.Listen("tcp", cmd.Addr)
			if err != nil {
				cmd.Resp <- err
				continue
			}
			listener = ln
			acceptDone = make(chan struct{})
			go s.acceptConnections(ln, acceptDone)
			cmd.Resp <- nil
		case stopSMTP:
			if listener != nil {
				listener.Close()
				<-acceptDone
				listener = nil
				acceptDone = nil
			}
			close(cmd.Resp)
		}
	}
}

// acceptConnections hands accepted sockets to the processing loop.
func (s *Service) acceptConnections(ln net.Listener, done chan struct{}) {
	defer close(done)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s.smtpConns <- conn
	}
}

type listMailboxRequest struct {
	Address string
	Resp    chan []Message
}

// Deliveries exposes the channel where other services push outgoing mail.
func (s *Service) Deliveries() chan<- Message {
	return s.deliveries
}

// ListMailbox returns a copy of the mailbox for a given address.
func (s *Service) ListMailbox(address string) []Message {
	resp := make(chan []Message)
	s.requests <- listMailboxRequest{Address: address, Resp: resp}
	return <-resp
}

// StartSMTPServer begins listening on the provided address.
func (s *Service) StartSMTPServer(addr string) error {
	resp := make(chan error, 1)
	s.smtpCtrl <- startSMTP{Addr: addr, Resp: resp}
	return <-resp
}

// StopSMTPServer shuts down the listener so an updater can free the port.
func (s *Service) StopSMTPServer() {
	done := make(chan struct{})
	s.smtpCtrl <- stopSMTP{Resp: done}
	<-done
}

// handleSMTP implements a very small subset of SMTP.
func (s *Service) handleSMTP(conn net.Conn) {
	defer conn.Close()
	writer := bufio.NewWriter(conn)
	reader := bufio.NewReader(conn)
	fmt.Fprintln(writer, "220 chicha-care SMTP ready")
	writer.Flush()
	var from string
	recipients := []string{}
	var data bytes.Buffer
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if !inData {
			switch {
			case strings.HasPrefix(strings.ToUpper(line), "HELO") || strings.HasPrefix(strings.ToUpper(line), "EHLO"):
				fmt.Fprintln(writer, "250 Hello")
			case strings.HasPrefix(strings.ToUpper(line), "MAIL FROM:"):
				from = strings.Trim(strings.TrimPrefix(line, "MAIL FROM:"), "<>")
				fmt.Fprintln(writer, "250 Sender OK")
			case strings.HasPrefix(strings.ToUpper(line), "RCPT TO:"):
				rcpt := strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<>")
				recipients = append(recipients, rcpt)
				fmt.Fprintln(writer, "250 Recipient OK")
			case strings.EqualFold(line, "DATA"):
				inData = true
				data.Reset()
				fmt.Fprintln(writer, "354 End data with <CR><LF>.<CR><LF>")
			case strings.EqualFold(line, "QUIT"):
				fmt.Fprintln(writer, "221 Bye")
				writer.Flush()
				return
			default:
				fmt.Fprintln(writer, "250 OK")
			}
			writer.Flush()
		} else {
			if line == "." {
				for _, rcpt := range recipients {
					s.deliveries <- Message{From: from, To: rcpt, Subject: "Received via SMTP", Body: data.String()}
				}
				fmt.Fprintln(writer, "250 Message accepted")
				writer.Flush()
				inData = false
				recipients = nil
			} else {
				data.WriteString(line)
				data.WriteByte('\n')
			}
		}
	}
}

// Enqueue pushes an outbound message without blocking callers.
func (s *Service) Enqueue(msg Message) {
	select {
	case s.deliveries <- msg:
	default:
		// We drop the message to keep the system responsive under load.
	}
}

// EnqueueSMTPConnection lets tests inject fake SMTP connections.
func (s *Service) EnqueueSMTPConnection(conn net.Conn) {
	s.smtpConns <- conn
}
