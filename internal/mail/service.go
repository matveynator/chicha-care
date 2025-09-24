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
}

// NewService creates a new mail service.
func NewService(buffer int) *Service {
	svc := &Service{
		deliveries: make(chan Message, buffer),
		requests:   make(chan interface{}),
		smtpConns:  make(chan net.Conn),
	}
	go svc.loop()
	go svc.acceptLoop()
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
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.smtpConns <- conn
		}
	}()
	return nil
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
