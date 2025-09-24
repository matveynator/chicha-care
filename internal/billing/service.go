package billing

// The billing package manages clients and invoices through an asynchronous
// service. We keep logic small and composable so it can work well with channels.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/example/chicha-care/internal/mail"
)

// Client represents an entity that receives invoices.
type Client struct {
	ID    string
	Name  string
	Email string
}

// Invoice stores the rendered invoice and its digital signature.
type Invoice struct {
	ID        string
	ClientID  string
	Amount    float64
	IssuedAt  time.Time
	Document  []byte
	Signature []byte
}

// Service coordinates DB access and document signing.
type Service struct {
	db        *sql.DB
	signer    *rsa.PrivateKey
	mailOut   chan<- mail.Message
	requests  chan interface{}
	responses chan interface{}
}

// NewService spins the event loop that serializes DB operations.
func NewService(db *sql.DB, signer *rsa.PrivateKey, mailOut chan<- mail.Message) *Service {
	svc := &Service{
		db:        db,
		signer:    signer,
		mailOut:   mailOut,
		requests:  make(chan interface{}),
		responses: make(chan interface{}),
	}
	go svc.loop()
	return svc
}

// loop keeps DB usage sequential so we can avoid mutexes.
func (s *Service) loop() {
	for {
		select {
		case req := <-s.requests:
			switch msg := req.(type) {
			case addClientRequest:
				s.responses <- s.handleAddClient(msg)
			case listClientsRequest:
				s.responses <- s.handleListClients()
			case createInvoiceRequest:
				s.responses <- s.handleCreateInvoice(msg)
			case listInvoicesRequest:
				s.responses <- s.handleListInvoices(msg)
			default:
				s.responses <- fmt.Errorf("unknown request %T", msg)
			}
		}
	}
}

type addClientRequest struct {
	Client Client
}

type listClientsRequest struct{}

type createInvoiceRequest struct {
	ClientID string
	Amount   float64
}

type listInvoicesRequest struct {
	ClientID string
}

// EnsureSchema creates the tables needed for billing.
func (s *Service) EnsureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS clients(id TEXT PRIMARY KEY, name TEXT, email TEXT)`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS invoices(id TEXT PRIMARY KEY, client_id TEXT, amount REAL, issued_at TEXT, document BLOB, signature BLOB)`)
	return err
}

// AddClient registers a new client.
func (s *Service) AddClient(ctx context.Context, client Client) error {
	select {
	case s.requests <- addClientRequest{Client: client}:
	case <-ctx.Done():
		return ctx.Err()
	}
	resp := <-s.responses
	if err, ok := resp.(error); ok {
		return err
	}
	return nil
}

func (s *Service) handleAddClient(req addClientRequest) error {
	_, err := s.db.Exec(`INSERT INTO clients(id,name,email) VALUES(?,?,?)`, req.Client.ID, req.Client.Name, req.Client.Email)
	return err
}

// ListClients returns all clients.
func (s *Service) ListClients(ctx context.Context) ([]Client, error) {
	select {
	case s.requests <- listClientsRequest{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp := <-s.responses
	switch val := resp.(type) {
	case []Client:
		return val, nil
	case error:
		return nil, val
	default:
		return nil, fmt.Errorf("unexpected response %T", resp)
	}
}

func (s *Service) handleListClients() interface{} {
	rows, err := s.db.Query(`SELECT id,name,email FROM clients`)
	if err != nil {
		return err
	}
	defer rows.Close()
	clients := []Client{}
	for rows.Next() {
		var c Client
		if err := rows.Scan(&c.ID, &c.Name, &c.Email); err != nil {
			return err
		}
		clients = append(clients, c)
	}
	return clients
}

// CreateInvoice builds, signs and stores a new invoice.
func (s *Service) CreateInvoice(ctx context.Context, clientID string, amount float64) (Invoice, error) {
	select {
	case s.requests <- createInvoiceRequest{ClientID: clientID, Amount: amount}:
	case <-ctx.Done():
		return Invoice{}, ctx.Err()
	}
	resp := <-s.responses
	switch val := resp.(type) {
	case Invoice:
		return val, nil
	case error:
		return Invoice{}, val
	default:
		return Invoice{}, fmt.Errorf("unexpected response %T", resp)
	}
}

func (s *Service) handleCreateInvoice(req createInvoiceRequest) interface{} {
	client, err := s.findClient(req.ClientID)
	if err != nil {
		return err
	}
	inv, err := s.buildInvoice(client, req.Amount)
	if err != nil {
		return err
	}
	if err := s.storeInvoice(inv); err != nil {
		return err
	}
	s.dispatchInvoice(client, inv)
	return inv
}

// findClient returns a client by ID.
func (s *Service) findClient(id string) (Client, error) {
	rows, err := s.db.Query(`SELECT id,name,email FROM clients WHERE id = ?`, id)
	if err != nil {
		return Client{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Client{}, errors.New("client not found")
	}
	var c Client
	if err := rows.Scan(&c.ID, &c.Name, &c.Email); err != nil {
		return Client{}, err
	}
	return c, nil
}

// buildInvoice creates the document and signature.
func (s *Service) buildInvoice(client Client, amount float64) (Invoice, error) {
	id, err := randomID()
	if err != nil {
		return Invoice{}, err
	}
	issued := time.Now().UTC()
	payload := map[string]interface{}{
		"invoice_id":  id,
		"client_id":   client.ID,
		"client_name": client.Name,
		"amount":      amount,
		"issued_at":   issued.Format(time.RFC3339Nano),
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return Invoice{}, err
	}
	signature, err := signBlob(s.signer, blob)
	if err != nil {
		return Invoice{}, err
	}
	return Invoice{
		ID:        id,
		ClientID:  client.ID,
		Amount:    amount,
		IssuedAt:  issued,
		Document:  blob,
		Signature: signature,
	}, nil
}

// storeInvoice writes the invoice to the database.
func (s *Service) storeInvoice(inv Invoice) error {
	_, err := s.db.Exec(`INSERT INTO invoices(id,client_id,amount,issued_at,document,signature) VALUES(?,?,?,?,?,?)`, inv.ID, inv.ClientID, inv.Amount, inv.IssuedAt.Format(time.RFC3339Nano), inv.Document, inv.Signature)
	return err
}

// dispatchInvoice notifies the mail subsystem.
func (s *Service) dispatchInvoice(client Client, inv Invoice) {
	msg := mail.Message{
		From:    "billing@chicha-care.local",
		To:      client.Email,
		Subject: fmt.Sprintf("Invoice %s", inv.ID),
		Body:    fmt.Sprintf("Attached invoice: %s\nSignature: %s", string(inv.Document), base64.StdEncoding.EncodeToString(inv.Signature)),
	}
	select {
	case s.mailOut <- msg:
	default:
		// If the channel is full we drop the message to avoid blocking the scheduler.
	}
}

// ListInvoices returns invoices optionally filtered by client.
func (s *Service) ListInvoices(ctx context.Context, clientID string) ([]Invoice, error) {
	select {
	case s.requests <- listInvoicesRequest{ClientID: clientID}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp := <-s.responses
	switch val := resp.(type) {
	case []Invoice:
		return val, nil
	case error:
		return nil, val
	default:
		return nil, fmt.Errorf("unexpected response %T", resp)
	}
}

func (s *Service) handleListInvoices(req listInvoicesRequest) interface{} {
	query := `SELECT id,client_id,amount,issued_at,document,signature FROM invoices`
	args := []interface{}{}
	if req.ClientID != "" {
		query += ` WHERE client_id = ?`
		args = append(args, req.ClientID)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	result := []Invoice{}
	for rows.Next() {
		var inv Invoice
		var issued string
		if err := rows.Scan(&inv.ID, &inv.ClientID, &inv.Amount, &issued, &inv.Document, &inv.Signature); err != nil {
			return err
		}
		t, err := time.Parse(time.RFC3339Nano, issued)
		if err != nil {
			return err
		}
		inv.IssuedAt = t
		result = append(result, inv)
	}
	return result
}

// randomID builds a unique identifier from crypto/rand.
func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// signBlob creates an RSA signature for the invoice payload.
func signBlob(key *rsa.PrivateKey, blob []byte) ([]byte, error) {
	hash := sha256.Sum256(blob)
	return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
}
