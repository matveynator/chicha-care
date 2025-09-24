package main

// The main package wires together storage, billing, mail, auth, scheduler, builds, and updates.
// We coordinate everything through goroutines and channels to follow the requested design rules.

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
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/example/chicha-care/internal/auth"
	"github.com/example/chicha-care/internal/billing"
	"github.com/example/chicha-care/internal/build"
	"github.com/example/chicha-care/internal/mail"
	"github.com/example/chicha-care/internal/scheduler"
	"github.com/example/chicha-care/internal/storage"
	"github.com/example/chicha-care/internal/update"
	"github.com/example/chicha-care/internal/upgrade"
)

// version is injected at build time; we default to dev for local runs.
var version = "dev"

// main simply delegates to run so we can return rich errors.
func main() {
	if err := run(); err != nil {
		log.Fatalf("run: %v", err)
	}
}

// run prepares services, starts background workers, and holds the main loop.
func run() error {
	httpAddr := flag.String("http", ":80", "HTTP listen address")
	httpsAddr := flag.String("https", ":443", "HTTPS listen address")
	smtpAddr := flag.String("smtp", ":25", "SMTP listen address")
	enableTLS := flag.Bool("enable-https", false, "Enable HTTPS listener")
	domain := flag.String("domain", "chicha-care.local", "Domain for HTTPS certificate")
	invoiceInterval := flag.Duration("invoice-interval", 30*24*time.Hour, "Invoice generation interval")
	invoiceAmount := flag.Float64("invoice-amount", 37236, "Default invoice amount")
	repoPath := flag.String("repo-path", ".", "Repository path for build automation")
	buildInterval := flag.Duration("build-interval", time.Minute, "How often to check commits for Stable Release tags")
	updateOwner := flag.String("update-owner", "example", "GitHub owner for update checks")
	updateRepo := flag.String("update-repo", "chicha-care", "GitHub repository for update checks")
	updateBinary := flag.String("update-binary", "chicha-care", "Binary name used by release assets")
	updateInterval := flag.Duration("update-interval", time.Minute, "How often to poll for new releases")
	flag.Parse()

	controlPath := os.Getenv("CHICHA_UPGRADE_CONTROL")
	var handshakeAcknowledged bool
	if controlPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := upgrade.WaitForGo(ctx, controlPath); err != nil {
			upgrade.WriteState(controlPath, upgrade.StateAbort)
			return fmt.Errorf("upgrade wait: %w", err)
		}
		defer func() {
			if !handshakeAcknowledged {
				upgrade.WriteState(controlPath, upgrade.StateAbort)
			}
		}()
	}

	storage.Register()
	db, err := sql.Open("inmemory", "primary")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	ctx := context.Background()

	authSvc := auth.NewService(db)
	if err := authSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("auth schema: %w", err)
	}
	if err := authSvc.Register(ctx, "admin", "admin"); err != nil {
		log.Printf("auth register: %v", err)
	}

	mailSvc := mail.NewService(32)
	if err := mailSvc.StartSMTPServer(*smtpAddr); err != nil {
		return fmt.Errorf("smtp server: %w", err)
	}
	defer mailSvc.StopSMTPServer()

	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("rsa key: %w", err)
	}

	billingSvc := billing.NewService(db, signer, mailSvc.Deliveries())
	if err := billingSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("billing schema: %w", err)
	}

	sched := scheduler.New(billingSvc, *invoiceInterval, *invoiceAmount)
	sched.Start()
	defer sched.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", homeHandler)
	mux.Handle("/clients", authSvc.Middleware(http.HandlerFunc(clientsHandler(billingSvc))))
	mux.Handle("/invoices", authSvc.Middleware(http.HandlerFunc(invoicesHandler(billingSvc))))

	buildSvc := build.NewService(build.Config{RepoPath: *repoPath, Binary: *updateBinary, Interval: *buildInterval})
	buildSvc.Start()
	defer buildSvc.Stop()
	go monitorBuilds(buildSvc.Events())

	updateSvc := update.NewService(update.Config{
		Owner:          *updateOwner,
		Repo:           *updateRepo,
		Binary:         *updateBinary,
		Interval:       *updateInterval,
		CurrentVersion: version,
	})
	updateSvc.Start()
	defer updateSvc.Stop()
	updates := updateSvc.Events()

	cfg := serverConfig{
		HTTPAddr:  *httpAddr,
		HTTPSAddr: *httpsAddr,
		EnableTLS: *enableTLS,
		Domain:    *domain,
		Handler:   mux,
	}

	server, tlsServer, errChan, err := startServers(cfg)
	if err != nil {
		return fmt.Errorf("start servers: %w", err)
	}
	if controlPath != "" {
		if err := upgrade.WriteState(controlPath, upgrade.StateRunning); err != nil {
			return fmt.Errorf("signal running: %w", err)
		}
		handshakeAcknowledged = true
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case sig := <-sigs:
			log.Printf("received signal %v, shutting down", sig)
			shutdownHTTPServers(server, tlsServer)
			return nil
		case err := <-errChan:
			if err == nil || errors.Is(err, http.ErrServerClosed) {
				continue
			}
			return fmt.Errorf("server error: %w", err)
		case evt, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			success, restart, uErr := handleUpdate(evt, errChan, server, tlsServer, mailSvc)
			if uErr != nil {
				log.Printf("update: %v", uErr)
			}
			if success {
				return nil
			}
			if restart {
				if err := mailSvc.StartSMTPServer(*smtpAddr); err != nil {
					return fmt.Errorf("restart smtp: %w", err)
				}
				server, tlsServer, errChan, err = startServers(cfg)
				if err != nil {
					return fmt.Errorf("restart http: %w", err)
				}
			}
		}
	}
}

// serverConfig groups the knobs required to run HTTP and HTTPS listeners.
type serverConfig struct {
	HTTPAddr  string
	HTTPSAddr string
	EnableTLS bool
	Domain    string
	Handler   http.Handler
}

// startServers binds the listeners before spinning goroutines so we know they succeed.
func startServers(cfg serverConfig) (*http.Server, *http.Server, chan error, error) {
	errChan := make(chan error, 2)
	lnHTTP, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return nil, nil, nil, err
	}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: cfg.Handler}

	var lnHTTPS net.Listener
	var tlsServer *http.Server
	if cfg.EnableTLS {
		cert, err := generateCertificate(cfg.Domain)
		if err != nil {
			lnHTTP.Close()
			return nil, nil, nil, err
		}
		lnHTTPS, err = net.Listen("tcp", cfg.HTTPSAddr)
		if err != nil {
			lnHTTP.Close()
			return nil, nil, nil, err
		}
		tlsServer = &http.Server{Addr: cfg.HTTPSAddr, Handler: cfg.Handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	}

	go func() {
		log.Printf("HTTP listening on %s", cfg.HTTPAddr)
		errChan <- server.Serve(lnHTTP)
	}()

	if tlsServer != nil {
		go func() {
			log.Printf("HTTPS listening on %s", cfg.HTTPSAddr)
			errChan <- tlsServer.Serve(tls.NewListener(lnHTTPS, tlsServer.TLSConfig))
		}()
	}

	return server, tlsServer, errChan, nil
}

// shutdownHTTPServers gracefully stops both HTTP and HTTPS listeners.
func shutdownHTTPServers(server, tlsServer *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if server != nil {
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http shutdown: %v", err)
		}
	}
	if tlsServer != nil {
		if err := tlsServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("https shutdown: %v", err)
		}
	}
}

// shutdownHTTPServersForUpdate wraps shutdownHTTPServers so we can bubble errors up during upgrades.
func shutdownHTTPServersForUpdate(server, tlsServer *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	if server != nil {
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		}
	}
	if tlsServer != nil {
		if err := tlsServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("https shutdown: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// handleUpdate orchestrates the handover to a freshly downloaded binary.
func handleUpdate(evt update.Event, errChan chan error, server *http.Server, tlsServer *http.Server, mailSvc *mail.Service) (bool, bool, error) {
	if evt.BinaryPath == "" {
		return false, false, errors.New("update event missing binary path")
	}
	exe, err := os.Executable()
	if err != nil {
		return false, false, err
	}
	dir := filepath.Dir(exe)
	handshakePath := filepath.Join(dir, fmt.Sprintf(".upgrade-%d", time.Now().UnixNano()))
	if err := upgrade.WriteState(handshakePath, upgrade.StatePending); err != nil {
		return false, false, err
	}
	defer os.Remove(handshakePath)

	cmd := exec.Command(evt.BinaryPath, os.Args[1:]...)
	cmd.Env = append(os.Environ(),
		"CHICHA_UPGRADE_CONTROL="+handshakePath,
		"CHICHA_UPGRADE_PREVIOUS="+exe,
		"CHICHA_UPGRADE_VERSION="+evt.Version,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return false, false, err
	}

	exitChan := make(chan error, 1)
	go func() {
		exitChan <- cmd.Wait()
	}()

	standbyCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	standby := make(chan error, 1)
	go func() {
		_, err := upgrade.WaitForState(standbyCtx, handshakePath, upgrade.StateStandby)
		standby <- err
	}()

	select {
	case err := <-standby:
		if err != nil {
			upgrade.WriteState(handshakePath, upgrade.StateAbort)
			waitProcessExit(exitChan, cmd)
			return false, false, err
		}
	case err := <-exitChan:
		upgrade.WriteState(handshakePath, upgrade.StateAbort)
		return false, false, fmt.Errorf("new version exited before standby: %w", err)
	}

	mailSvc.StopSMTPServer()

	if err := shutdownHTTPServersForUpdate(server, tlsServer); err != nil {
		upgrade.WriteState(handshakePath, upgrade.StateAbort)
		waitProcessExit(exitChan, cmd)
		return false, true, err
	}
	drainErrChan(errChan)

	if err := upgrade.WriteState(handshakePath, upgrade.StateGo); err != nil {
		waitProcessExit(exitChan, cmd)
		return false, true, err
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelRun()
	stateCh := make(chan struct {
		state string
		err   error
	}, 1)
	go func() {
		state, err := upgrade.WaitForState(runCtx, handshakePath, upgrade.StateRunning, upgrade.StateAbort)
		stateCh <- struct {
			state string
			err   error
		}{state: state, err: err}
	}()

	select {
	case res := <-stateCh:
		if res.err != nil {
			upgrade.WriteState(handshakePath, upgrade.StateAbort)
			waitProcessExit(exitChan, cmd)
			return false, true, res.err
		}
		if res.state == upgrade.StateRunning {
			return true, false, nil
		}
		upgrade.WriteState(handshakePath, upgrade.StateAbort)
		waitProcessExit(exitChan, cmd)
		return false, true, fmt.Errorf("new version reported abort")
	case err := <-exitChan:
		upgrade.WriteState(handshakePath, upgrade.StateAbort)
		return false, true, fmt.Errorf("new version exited during startup: %w", err)
	}
}

// waitProcessExit tries to reap the child process so we do not leak zombies.
func waitProcessExit(exit <-chan error, cmd *exec.Cmd) {
	select {
	case <-exit:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}

// drainErrChan removes stale shutdown events so restarts get a clean channel.
func drainErrChan(ch chan error) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// monitorBuilds logs build events so operators can trace automation decisions.
func monitorBuilds(events <-chan build.Event) {
	for evt := range events {
		if evt.Err != nil {
			log.Printf("build trigger failed: %v", evt.Err)
			continue
		}
		log.Printf("build triggered for %s: %s", evt.Commit, evt.Message)
		for _, res := range evt.Results {
			if res.Err != nil {
				log.Printf("build %s/%s failed: %v", res.Target.GOOS, res.Target.GOARCH, res.Err)
			} else {
				log.Printf("build %s/%s artifact at %s", res.Target.GOOS, res.Target.GOARCH, res.Artifact)
			}
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
