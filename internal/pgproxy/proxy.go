package pgproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Proxy forwards Postgres connections to an upstream server and snapshots
// data inside the database before destructive statements run.
type Proxy struct {
	Upstream string
	Log      *log.Logger

	cfg      *pgconn.Config
	ln       net.Listener
	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	closed   bool
	OnChange func(StmtPlan)
}

// New validates the upstream URL.
func New(upstream string, logger *log.Logger) (*Proxy, error) {
	cfg, err := pgconn.ParseConfig(upstream)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Proxy{Upstream: upstream, Log: logger, cfg: cfg, conns: map[net.Conn]struct{}{}}, nil
}

// Setup installs the oops schema and functions in the upstream database.
func Setup(ctx context.Context, upstream string) error {
	conn, err := pgx.Connect(ctx, upstream)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7123007)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("installing the oops schema: %w", err)
	}
	return tx.Commit(ctx)
}

// Listen starts accepting connections on addr.
func (p *Proxy) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	p.ln = ln
	return nil
}

// Addr is the listening address.
func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

// Serve accepts connections until Close.
func (p *Proxy) Serve() error {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		p.mu.Lock()
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() {
				p.mu.Lock()
				delete(p.conns, c)
				p.mu.Unlock()
				c.Close()
			}()
			if err := p.handle(c); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				p.Log.Printf("connection ended: %v", err)
			}
		}()
	}
}

// Close stops the proxy and drops open connections.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	var err error
	if p.ln != nil {
		err = p.ln.Close()
	}
	p.wg.Wait()
	return err
}

func (p *Proxy) dialUpstream() (net.Conn, error) {
	host, port := p.cfg.Host, p.cfg.Port
	var c net.Conn
	var err error
	d := net.Dialer{Timeout: 10 * time.Second}
	if strings.HasPrefix(host, "/") {
		c, err = d.Dial("unix", fmt.Sprintf("%s/.s.PGSQL.%d", host, port))
	} else {
		c, err = d.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	}
	if err != nil {
		return nil, err
	}
	tlsCfg := p.cfg.TLSConfig
	if tlsCfg == nil {
		return c, nil
	}
	// SSLRequest
	req := make([]byte, 8)
	binary.BigEndian.PutUint32(req[0:4], 8)
	binary.BigEndian.PutUint32(req[4:8], 80877103)
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 1)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	if resp[0] == 'S' {
		tc := tls.Client(c, tlsCfg)
		if err := tc.Handshake(); err != nil {
			c.Close()
			return nil, err
		}
		return tc, nil
	}
	allowPlain := false
	for _, fb := range p.cfg.Fallbacks {
		if fb.TLSConfig == nil {
			allowPlain = true
		}
	}
	if !allowPlain {
		c.Close()
		return nil, errors.New("upstream server does not support SSL but sslmode requires it")
	}
	return c, nil
}

func readStartup(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr))
	if n < 8 || n > 1<<20 {
		return nil, errors.New("bad startup packet")
	}
	buf := make([]byte, n)
	copy(buf, hdr)
	if _, err := io.ReadFull(r, buf[4:]); err != nil {
		return nil, err
	}
	return buf, nil
}

func readMsg(r *bufio.Reader) (byte, []byte, error) {
	t, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr))
	if n < 4 || n > 1<<30 {
		return 0, nil, errors.New("bad message length")
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return t, body, nil
}

func frame(t byte, body []byte) []byte {
	out := make([]byte, 5+len(body))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)+4))
	copy(out[5:], body)
	return out
}

func (p *Proxy) handle(client net.Conn) error {
	cr := bufio.NewReaderSize(client, 64<<10)
	var startup []byte
	for {
		pkt, err := readStartup(cr)
		if err != nil {
			return err
		}
		code := binary.BigEndian.Uint32(pkt[4:8])
		switch code {
		case 80877103, 80877104: // SSLRequest, GSSENCRequest: we speak plain text locally
			if _, err := client.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		case 80877102: // CancelRequest: pass it straight through
			up, err := p.dialUpstream()
			if err != nil {
				return err
			}
			_, err = up.Write(pkt)
			up.Close()
			return err
		}
		startup = pkt
		break
	}
	up, err := p.dialUpstream()
	if err != nil {
		p.writeFatal(client, "oopsy proxy could not reach the upstream database: "+err.Error())
		return err
	}
	defer up.Close()
	if _, err := up.Write(startup); err != nil {
		return err
	}
	s := &session{p: p, client: client, up: up, upR: bufio.NewReaderSize(up, 64<<10),
		stmts: map[string]*prepared{}, portals: map[string]*portal{}}
	errc := make(chan error, 2)
	go func() { errc <- s.serverLoop() }()
	go func() { errc <- s.clientLoop(cr) }()
	err = <-errc
	client.Close()
	up.Close()
	<-errc
	return err
}

func (p *Proxy) writeFatal(w io.Writer, msg string) {
	e := &pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "08006", Message: msg}
	b, _ := e.Encode(nil)
	w.Write(b)
}

type prepared struct {
	query string
	oids  []uint32
}

type portal struct {
	stmt    string
	formats []int16
	values  [][]byte
}

// unit is one request whose responses we expect from the server, in order.
type unit struct {
	kind    byte
	swallow bool // injected by the proxy: hide its responses
	hide    map[int]bool
	stmtIdx int
	done    chan []uint32 // closed or sent when a captured unit finishes
	oids    []uint32
}

type session struct {
	p       *Proxy
	client  net.Conn
	up      net.Conn
	upR     *bufio.Reader
	mu      sync.Mutex
	fifo    []*unit
	errMode bool
	cmu     sync.Mutex // guards writes to client
	stmts   map[string]*prepared
	portals map[string]*portal
	seq     int
}

func (s *session) toClient(b []byte) error {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	_, err := s.client.Write(b)
	return err
}

// push records an expected response unit, unless the server is discarding
// messages after an error until the next Sync.
func (s *session) push(u *unit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errMode && u.kind != 'S' {
		if u.done != nil {
			close(u.done)
		}
		return
	}
	s.fifo = append(s.fifo, u)
}

func rewriteError(body []byte) []byte {
	var e pgproto3.ErrorResponse
	if err := e.Decode(body); err != nil {
		return frame('E', body)
	}
	e.Message = "oopsy could not snapshot before your statement, so it did not run: " + e.Message
	b, _ := e.Encode(nil)
	return b
}

func (s *session) serverLoop() error {
	for {
		t, body, err := readMsg(s.upR)
		if err != nil {
			return err
		}
		raw := frame(t, body)
		if t == 'S' || t == 'A' { // ParameterStatus, NotificationResponse
			if err := s.toClient(raw); err != nil {
				return err
			}
			continue
		}
		s.mu.Lock()
		var head *unit
		if len(s.fifo) > 0 {
			head = s.fifo[0]
		}
		if head == nil {
			s.mu.Unlock()
			if err := s.toClient(raw); err != nil {
				return err
			}
			continue
		}
		if t == 'N' { // NoticeResponse
			s.mu.Unlock()
			if !head.swallow && !head.hide[head.stmtIdx] {
				if err := s.toClient(raw); err != nil {
					return err
				}
			}
			continue
		}
		out := raw
		forward := !head.swallow
		ends := false
		switch head.kind {
		case 'Q':
			switch t {
			case 'Z':
				ends, forward = true, true
			case 'C', 'I':
				forward = !head.hide[head.stmtIdx]
				head.stmtIdx++
			case 'E':
				if head.hide[head.stmtIdx] {
					out = rewriteError(body)
				}
				forward = true
			default:
				forward = !head.hide[head.stmtIdx]
			}
		case 'P':
			ends = t == '1' || t == 'E'
		case 'B':
			ends = t == '2' || t == 'E'
		case 'C':
			ends = t == '3' || t == 'E'
		case 'D':
			if t == 't' {
				var pd pgproto3.ParameterDescription
				if pd.Decode(body) == nil {
					head.oids = pd.ParameterOIDs
				}
			}
			ends = t == 'T' || t == 'n' || t == 'E'
		case 'E':
			ends = t == 'C' || t == 'I' || t == 's' || t == 'E'
		case 'F':
			ends = t == 'V' || t == 'E'
		case 'S':
			ends = t == 'Z'
			forward = true
		}
		if t == 'E' && head.kind != 'Q' && head.kind != 'S' {
			if head.swallow {
				out = rewriteError(body)
				forward = true
			}
			// The server now skips everything until Sync.
			s.errMode = true
			rest := s.fifo[1:]
			var keep []*unit
			for i, u := range rest {
				if u.kind == 'S' {
					keep = rest[i:]
					break
				}
				if u.done != nil {
					close(u.done)
				}
			}
			s.fifo = append([]*unit{head}, keep...)
		}
		if t == 'Z' {
			s.errMode = false
		}
		if ends {
			s.fifo = s.fifo[1:]
			if head.done != nil {
				head.done <- head.oids
				close(head.done)
			}
		}
		s.mu.Unlock()
		if forward {
			if err := s.toClient(out); err != nil {
				return err
			}
		}
	}
}

func (s *session) clientLoop(cr *bufio.Reader) error {
	for {
		t, body, err := readMsg(cr)
		if err != nil {
			return err
		}
		switch t {
		case 'Q':
			err = s.handleQuery(body)
		case 'P':
			var m pgproto3.Parse
			if m.Decode(body) == nil {
				s.stmts[m.Name] = &prepared{query: m.Query, oids: m.ParameterOIDs}
			}
			err = s.send(&unit{kind: 'P'}, frame(t, body))
		case 'B':
			var m pgproto3.Bind
			if m.Decode(body) == nil {
				vals := make([][]byte, len(m.Parameters))
				for i, v := range m.Parameters {
					if v != nil {
						vals[i] = append([]byte{}, v...)
					}
				}
				s.portals[m.DestinationPortal] = &portal{stmt: m.PreparedStatement, formats: append([]int16{}, m.ParameterFormatCodes...), values: vals}
			}
			err = s.send(&unit{kind: 'B'}, frame(t, body))
		case 'E':
			var m pgproto3.Execute
			if m.Decode(body) == nil {
				if err := s.beforeExecute(m.Portal); err != nil {
					return err
				}
			}
			err = s.send(&unit{kind: 'E'}, frame(t, body))
		case 'D':
			err = s.send(&unit{kind: 'D'}, frame(t, body))
		case 'C':
			var m pgproto3.Close
			if m.Decode(body) == nil {
				if m.ObjectType == 'S' {
					delete(s.stmts, m.Name)
				} else {
					delete(s.portals, m.Name)
				}
			}
			err = s.send(&unit{kind: 'C'}, frame(t, body))
		case 'S':
			err = s.send(&unit{kind: 'S'}, frame(t, body))
		case 'F':
			err = s.send(&unit{kind: 'F'}, frame(t, body))
		case 'X':
			_, _ = s.up.Write(frame(t, body))
			return io.EOF
		default: // password, SASL, Flush, COPY data
			_, err = s.up.Write(frame(t, body))
		}
		if err != nil {
			return err
		}
	}
}

func (s *session) send(u *unit, msg []byte) error {
	s.push(u)
	_, err := s.up.Write(msg)
	return err
}

// handleQuery rewrites a simple query so the snapshot statements run inside
// the same implicit or explicit transaction, right before each destructive
// statement. Their results are hidden from the client.
func (s *session) handleQuery(body []byte) error {
	q := strings.TrimSuffix(string(body), "\x00")
	locs, plans, err := PlanAll(q)
	if err != nil || len(plans) == 0 {
		return s.send(&unit{kind: 'Q'}, frame('Q', body))
	}
	var b strings.Builder
	last := 0
	for _, p := range plans {
		b.WriteString(q[last:p.Location])
		for _, sn := range p.Snaps {
			b.WriteString(sn.SQL)
			b.WriteString(";\n")
		}
		last = p.Location
		s.p.logPlan(p)
	}
	b.WriteString(q[last:])
	// Each statement answers with exactly one CommandComplete, so hide the
	// ones that belong to our snapshot statements by position.
	hide := map[int]bool{}
	pos, pi := 0, 0
	for _, loc := range locs {
		for pi < len(plans) && plans[pi].Location == loc {
			for range plans[pi].Snaps {
				hide[pos] = true
				pos++
			}
			pi++
		}
		pos++
	}
	nb := append([]byte(b.String()), 0)
	return s.send(&unit{kind: 'Q', hide: hide}, frame('Q', nb))
}

// beforeExecute injects snapshot statements, using the client's own bound
// parameters, before a destructive portal runs.
func (s *session) beforeExecute(portalName string) error {
	pt := s.portals[portalName]
	if pt == nil {
		return nil
	}
	st := s.stmts[pt.stmt]
	if st == nil {
		return nil
	}
	// Plans carry group names, so each execution gets fresh ones.
	plans, _ := Plan(st.query)
	if len(plans) == 0 {
		return nil
	}
	oids := st.oids
	needParams := false
	for _, p := range plans {
		for _, sn := range p.Snaps {
			if sn.UsesParams {
				needParams = true
			}
		}
	}
	if needParams && len(pt.values) > 0 {
		complete := len(oids) >= len(pt.values)
		for _, o := range oids {
			if o == 0 {
				complete = false
			}
		}
		if !complete {
			// Ask the server for the statement's parameter types.
			u := &unit{kind: 'D', swallow: true, done: make(chan []uint32, 1)}
			d := &pgproto3.Describe{ObjectType: 'S', Name: pt.stmt}
			db, _ := d.Encode(nil)
			fl, _ := (&pgproto3.Flush{}).Encode(nil)
			s.push(u)
			if _, err := s.up.Write(append(db, fl...)); err != nil {
				return err
			}
			select {
			case got, ok := <-u.done:
				if !ok || got == nil {
					return nil // server is skipping until Sync; nothing will run
				}
				oids = got
			case <-time.After(30 * time.Second):
				return errors.New("timed out waiting for parameter types")
			}
		}
	}
	var buf []byte
	var units []*unit
	for _, p := range plans {
		s.p.logPlan(p)
		for _, sn := range p.Snaps {
			s.seq++
			name := fmt.Sprintf("oopsy_%d", s.seq)
			parse := &pgproto3.Parse{Name: name, Query: sn.SQL}
			bind := &pgproto3.Bind{DestinationPortal: name, PreparedStatement: name}
			if sn.UsesParams {
				parse.ParameterOIDs = oids
				bind.ParameterFormatCodes = pt.formats
				bind.Parameters = pt.values
			}
			for _, m := range []pgproto3.FrontendMessage{parse, bind, &pgproto3.Execute{Portal: name}, &pgproto3.Close{ObjectType: 'S', Name: name}} {
				buf, _ = m.Encode(buf)
			}
			units = append(units, &unit{kind: 'P', swallow: true}, &unit{kind: 'B', swallow: true}, &unit{kind: 'E', swallow: true}, &unit{kind: 'C', swallow: true})
		}
	}
	for _, u := range units {
		s.push(u)
	}
	_, err := s.up.Write(buf)
	return err
}

func (p *Proxy) logPlan(sp StmtPlan) {
	p.Log.Printf("snapshot before %s on %s", sp.Kind, strings.Join(sp.Targets, ", "))
	if p.OnChange != nil {
		p.OnChange(sp)
	}
}
