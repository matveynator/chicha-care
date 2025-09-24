package main

// The main package wires together storage, billing, mail, auth and scheduler.
// We keep each component small and connect them via channels as requested.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/chicha-care/internal/auth"
	"github.com/example/chicha-care/internal/billing"
	"github.com/example/chicha-care/internal/mail"
	"github.com/example/chicha-care/internal/scheduler"
	"github.com/example/chicha-care/internal/storage"
)

// main parses configuration, prepares services and starts background workers.
func main() {
	httpAddr := flag.String("http", ":80", "HTTP listen address")
	httpsAddr := flag.String("https", ":443", "HTTPS listen address")
	smtpAddr := flag.String("smtp", ":25", "SMTP listen address")
	enableTLS := flag.Bool("enable-https", false, "Enable HTTPS listener")
	domain := flag.String("domain", "chicha-care.local", "Domain for HTTPS certificate")
	invoiceInterval := flag.Duration("invoice-interval", 30*24*time.Hour, "Invoice generation interval")
	invoiceAmount := flag.Float64("invoice-amount", 37236, "Default invoice amount")
	flag.Parse()

	storage.Register()
	db, err := sql.Open("inmemory", "primary")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	authSvc := auth.NewService(db)
	if err := authSvc.EnsureSchema(ctx); err != nil {
		log.Fatalf("auth schema: %v", err)
	}
	if err := authSvc.Register(ctx, "admin", "admin"); err != nil {
		log.Printf("auth register: %v", err)
	}

	mailSvc := mail.NewService(32)
	if err := mailSvc.StartSMTPServer(*smtpAddr); err != nil {
		log.Fatalf("smtp server: %v", err)
	}

	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("rsa key: %v", err)
	}

	billingSvc := billing.NewService(db, signer, mailSvc.Deliveries())
	if err := billingSvc.EnsureSchema(ctx); err != nil {
		log.Fatalf("billing schema: %v", err)
	}

	sched := scheduler.New(billingSvc, *invoiceInterval, *invoiceAmount)
	sched.Start()
	defer sched.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", homeHandler)
	mux.Handle("/clients", authSvc.Middleware(http.HandlerFunc(clientsHandler(billingSvc))))
	mux.Handle("/invoices", authSvc.Middleware(http.HandlerFunc(invoicesHandler(billingSvc))))

	server := &http.Server{Addr: *httpAddr, Handler: mux}

	errChan := make(chan error, 2)
	go func() {
		log.Printf("HTTP listening on %s", *httpAddr)
		errChan <- server.ListenAndServe()
	}()

	var tlsServer *http.Server
	if *enableTLS {
		cert, err := generateCertificate(*domain)
		if err != nil {
			log.Fatalf("certificate: %v", err)
		}
		tlsServer = &http.Server{Addr: *httpsAddr, Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
		go func() {
			log.Printf("HTTPS listening on %s", *httpsAddr)
			errChan <- tlsServer.ListenAndServeTLS("", "")
		}()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigs:
		log.Printf("received signal %v, shutting down", sig)
	case err := <-errChan:
		log.Printf("server error: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	if tlsServer != nil {
		if err := tlsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("https shutdown: %v", err)
		}
	}
}

// homeHandler prints a quick status with helpful instructions.
func homeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "Chicha Care billing system is running. Use authenticated endpoints to manage clients and invoices.")
}

// clientsHandler provides GET and POST handling using JSON payloads.
func clientsHandler(bill *billing.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			clients, err := bill.ListClients(ctx)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			respondJSON(w, clients)
		case http.MethodPost:
			defer r.Body.Close()
			var payload struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Email string `json:"email"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if err := bill.AddClient(ctx, billing.Client{ID: payload.ID, Name: payload.Name, Email: payload.Email}); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// invoicesHandler handles listing and manual invoice creation.
func invoicesHandler(bill *billing.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			clientID := r.URL.Query().Get("client")
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			invoices, err := bill.ListInvoices(ctx, clientID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			respondJSON(w, mapInvoices(invoices))
		case http.MethodPost:
			defer r.Body.Close()
			var payload struct {
				ClientID string  `json:"client_id"`
				Amount   float64 `json:"amount"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			invoice, err := bill.CreateInvoice(ctx, payload.ClientID, payload.Amount)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			respondJSON(w, invoiceDTO(invoice))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// respondJSON writes the provided value as JSON.
func respondJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// mapInvoices converts invoices to DTOs that expose base64 encoded fields.
func mapInvoices(invoices []billing.Invoice) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(invoices))
	for _, inv := range invoices {
		result = append(result, invoiceDTO(inv))
	}
	return result
}

// invoiceDTO serializes the invoice document and signature in a friendly format.
func invoiceDTO(inv billing.Invoice) map[string]interface{} {
	return map[string]interface{}{
		"id":        inv.ID,
		"client_id": inv.ClientID,
		"amount":    inv.Amount,
		"issued_at": inv.IssuedAt.Format(time.RFC3339Nano),
		"document":  base64.StdEncoding.EncodeToString(inv.Document),
		"signature": base64.StdEncoding.EncodeToString(inv.Signature),
	}
}

// generateCertificate builds a self-signed certificate for the provided domain.
func generateCertificate(domain string) (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
}
