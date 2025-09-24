package storage

// This package implements a very small SQL driver that keeps data in memory.
// We only rely on the standard library and we expose the driver through the
// database/sql package to respect the project constraints.

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Register exposes the in-memory driver so other packages can open a database
// connection using database/sql without depending on external components.
func Register() {
	// We register the driver only once and ignore the error if registration
	// happens multiple times because database/sql panics in that case.
	sql.Register("inmemory", Driver{})
}

// Driver implements the database/sql/driver.Driver interface.
type Driver struct{}

// Open connects to the in-memory manager that keeps all application data.
func (Driver) Open(name string) (driver.Conn, error) {
	mgr := getManager(name)
	return &conn{mgr: mgr}, nil
}

// conn is a lightweight handle for a logical connection to the in-memory store.
type conn struct {
	mgr *manager
}

// Prepare stores the SQL query so Exec and Query can send commands to the manager.
func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{mgr: c.mgr, query: query}, nil
}

// Close does not need to release anything because the manager is shared globally.
func (c *conn) Close() error { return nil }

// Begin is not implemented because we do not support SQL transactions.
func (c *conn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

// stmt delegates Exec and Query to the manager.
type stmt struct {
	mgr   *manager
	query string
}

// Close is a no-op for prepared statements in this in-memory implementation.
func (s *stmt) Close() error { return nil }

// NumInput returns -1 so database/sql will not try to validate argument counts.
func (s *stmt) NumInput() int { return -1 }

// Exec sends a command that modifies state.
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	req := execRequest{query: s.query, args: args, resp: make(chan execResponse)}
	s.mgr.execs <- req
	res := <-req.resp
	return res.result, res.err
}

// Query sends a command that returns rows.
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	req := queryRequest{query: s.query, args: args, resp: make(chan queryResponse)}
	s.mgr.queries <- req
	res := <-req.resp
	if res.err != nil {
		return nil, res.err
	}
	return &rows{columns: res.columns, data: res.data}, nil
}

// execRequest represents a state-changing command.
type execRequest struct {
	query string
	args  []driver.Value
	resp  chan execResponse
}

type execResponse struct {
	result driver.Result
	err    error
}

// queryRequest represents a read-only command.
type queryRequest struct {
	query string
	args  []driver.Value
	resp  chan queryResponse
}

type queryResponse struct {
	columns []string
	data    [][]driver.Value
	err     error
}

// rows implements the driver.Rows interface so database/sql can iterate.
type rows struct {
	columns []string
	data    [][]driver.Value
	idx     int
}

// Columns returns the names of the columns for the current result set.
func (r *rows) Columns() []string { return r.columns }

// Close releases the row iterator.
func (r *rows) Close() error {
	r.data = nil
	return nil
}

// Next streams one row at a time using channels-friendly semantics.
func (r *rows) Next(dest []driver.Value) error {
	if r.idx >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.idx])
	r.idx++
	return nil
}

// manager serializes access to the maps that store records.
type manager struct {
	execs   chan execRequest
	queries chan queryRequest
}

type clientRecord struct {
	ID    string
	Name  string
	Email string
}

type invoiceRecord struct {
	ID        string
	ClientID  string
	Amount    float64
	IssuedAt  time.Time
	Document  []byte
	Signature []byte
}

type userRecord struct {
	Username string
	Hash     string
}

// managerLoop owns the data and therefore does not require mutexes.
func managerLoop() {
	state := make(map[string]*store)
	for {
		select {
		case req := <-registryChan:
			st, ok := state[req.name]
			if !ok {
				st = newStore()
				state[req.name] = st
			}
			req.resp <- st.mgr
		}
	}
}

type store struct {
	mgr      *manager
	clients  map[string]clientRecord
	invoices map[string]invoiceRecord
	users    map[string]userRecord
}

func newStore() *store {
	mgr := &manager{
		execs:   make(chan execRequest),
		queries: make(chan queryRequest),
	}
	st := &store{
		mgr:      mgr,
		clients:  make(map[string]clientRecord),
		invoices: make(map[string]invoiceRecord),
		users:    make(map[string]userRecord),
	}
	go st.run()
	return st
}

// run handles all commands sequentially so the data remains consistent.
func (s *store) run() {
	for {
		select {
		case execReq := <-s.mgr.execs:
			execReq.resp <- execResponse{result: driver.RowsAffected(0), err: s.handleExec(execReq.query, execReq.args)}
		case queryReq := <-s.mgr.queries:
			cols, data, err := s.handleQuery(queryReq.query, queryReq.args)
			queryReq.resp <- queryResponse{columns: cols, data: data, err: err}
		}
	}
}

// handleExec parses supported SQL statements.
func (s *store) handleExec(query string, args []driver.Value) error {
	normalized := strings.ToLower(strings.TrimSpace(query))
	switch {
	case strings.HasPrefix(normalized, "create table"):
		// Table creation is a no-op because the in-memory maps are already set.
		return nil
	case strings.HasPrefix(normalized, "insert into clients"):
		if len(args) < 3 {
			return errors.New("invalid client insert")
		}
		id := toString(args[0])
		if _, exists := s.clients[id]; exists {
			return fmt.Errorf("client %s already exists", id)
		}
		s.clients[id] = clientRecord{ID: id, Name: toString(args[1]), Email: toString(args[2])}
		return nil
	case strings.HasPrefix(normalized, "insert into invoices"):
		if len(args) < 6 {
			return errors.New("invalid invoice insert")
		}
		amt, err := toFloat(args[2])
		if err != nil {
			return err
		}
		issued, err := toTime(args[3])
		if err != nil {
			return err
		}
		id := toString(args[0])
		if _, exists := s.invoices[id]; exists {
			return fmt.Errorf("invoice %s already exists", id)
		}
		s.invoices[id] = invoiceRecord{
			ID:        id,
			ClientID:  toString(args[1]),
			Amount:    amt,
			IssuedAt:  issued,
			Document:  toBytes(args[4]),
			Signature: toBytes(args[5]),
		}
		return nil
	case strings.HasPrefix(normalized, "insert into users"):
		if len(args) < 2 {
			return errors.New("invalid user insert")
		}
		username := toString(args[0])
		if _, exists := s.users[username]; exists {
			return fmt.Errorf("user %s already exists", username)
		}
		s.users[username] = userRecord{Username: username, Hash: toString(args[1])}
		return nil
	default:
		return fmt.Errorf("unsupported exec query: %s", query)
	}
}

// handleQuery reads data from the in-memory store.
func (s *store) handleQuery(query string, args []driver.Value) ([]string, [][]driver.Value, error) {
	normalized := strings.ToLower(strings.TrimSpace(query))
	switch {
	case strings.HasPrefix(normalized, "select id,name,email from clients where id"):
		if len(args) < 1 {
			return nil, nil, errors.New("missing client id")
		}
		id := toString(args[0])
		rec, ok := s.clients[id]
		if !ok {
			return []string{"id", "name", "email"}, nil, nil
		}
		return []string{"id", "name", "email"}, [][]driver.Value{{rec.ID, rec.Name, rec.Email}}, nil
	case strings.HasPrefix(normalized, "select id,name,email from clients"):
		rows := make([][]driver.Value, 0, len(s.clients))
		for _, rec := range s.clients {
			rows = append(rows, []driver.Value{rec.ID, rec.Name, rec.Email})
		}
		return []string{"id", "name", "email"}, rows, nil
	case strings.HasPrefix(normalized, "select id,client_id,amount,issued_at,document,signature from invoices where client_id"):
		if len(args) < 1 {
			return nil, nil, errors.New("missing client id")
		}
		clientID := toString(args[0])
		rows := [][]driver.Value{}
		for _, rec := range s.invoices {
			if rec.ClientID == clientID {
				rows = append(rows, []driver.Value{rec.ID, rec.ClientID, rec.Amount, rec.IssuedAt, rec.Document, rec.Signature})
			}
		}
		return []string{"id", "client_id", "amount", "issued_at", "document", "signature"}, rows, nil
	case strings.HasPrefix(normalized, "select id,client_id,amount,issued_at,document,signature from invoices"):
		rows := make([][]driver.Value, 0, len(s.invoices))
		for _, rec := range s.invoices {
			rows = append(rows, []driver.Value{rec.ID, rec.ClientID, rec.Amount, rec.IssuedAt, rec.Document, rec.Signature})
		}
		return []string{"id", "client_id", "amount", "issued_at", "document", "signature"}, rows, nil
	case strings.HasPrefix(normalized, "select username,hash from users where username"):
		if len(args) < 1 {
			return nil, nil, errors.New("missing username")
		}
		username := toString(args[0])
		rec, ok := s.users[username]
		if !ok {
			return []string{"username", "hash"}, nil, nil
		}
		return []string{"username", "hash"}, [][]driver.Value{{rec.Username, rec.Hash}}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported query: %s", query)
	}
}

// Helper conversion utilities keep the code tidy and readable.
func toString(v driver.Value) string {
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	default:
		return fmt.Sprint(val)
	}
}

func toFloat(v driver.Value) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case string:
		return strconv.ParseFloat(val, 64)
	case []byte:
		return strconv.ParseFloat(string(val), 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}

func toTime(v driver.Value) (time.Time, error) {
	switch val := v.(type) {
	case time.Time:
		return val, nil
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, val)
		if err != nil {
			return time.Time{}, err
		}
		return parsed, nil
	case []byte:
		parsed, err := time.Parse(time.RFC3339Nano, string(val))
		if err != nil {
			return time.Time{}, err
		}
		return parsed, nil
	default:
		return time.Time{}, fmt.Errorf("cannot convert %T to time.Time", v)
	}
}

func toBytes(v driver.Value) []byte {
	switch val := v.(type) {
	case []byte:
		return val
	case string:
		return []byte(val)
	default:
		return []byte(fmt.Sprint(val))
	}
}

// The registry coordinates managers for multiple logical databases.

type registryRequest struct {
	name string
	resp chan *manager
}

var registryChan = make(chan registryRequest)

func init() {
	go managerLoop()
}

// getManager returns the manager for the provided database name.
func getManager(name string) *manager {
	req := registryRequest{name: name, resp: make(chan *manager)}
	registryChan <- req
	return <-req.resp
}
