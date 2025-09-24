package scheduler

// Package scheduler periodically triggers invoice generation.
// We rely on goroutines, channels and select statements for coordination.

import (
	"context"
	"log"
	"time"

	"github.com/example/chicha-care/internal/billing"
)

// Scheduler owns the ticker that drives invoice runs.
type Scheduler struct {
	billing  *billing.Service
	interval time.Duration
	amount   float64
	stop     chan struct{}
	done     chan struct{}
}

// New creates a scheduler with the provided billing service and interval.
func New(bill *billing.Service, interval time.Duration, amount float64) *Scheduler {
	return &Scheduler{
		billing:  bill,
		interval: interval,
		amount:   amount,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the scheduler loop.
func (s *Scheduler) Start() {
	go s.loop()
}

// loop waits on the ticker and stop channels.
func (s *Scheduler) loop() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.runCycle()
		case <-s.stop:
			close(s.done)
			return
		}
	}
}

// runCycle fetches clients and produces invoices for each of them.
func (s *Scheduler) runCycle() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clients, err := s.billing.ListClients(ctx)
	if err != nil {
		log.Printf("scheduler: list clients failed: %v", err)
		return
	}
	for _, client := range clients {
		if _, err := s.billing.CreateInvoice(ctx, client.ID, s.amount); err != nil {
			log.Printf("scheduler: invoice for %s failed: %v", client.ID, err)
		}
	}
}

// Stop asks the scheduler loop to halt and waits for confirmation.
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
}
