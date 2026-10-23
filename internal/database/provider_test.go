package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A small database/sql connector verifies pool ownership without a live server.
type poolProbe struct {
	closed  atomic.Int32
	pingErr error
}

func (p *poolProbe) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &probeConn{probe: p}, nil
}
func (p *poolProbe) Driver() driver.Driver { return probeDriver{} }

type probeDriver struct{}

func (probeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type probeConn struct{ probe *poolProbe }

func (*probeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*probeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (c *probeConn) Close() error                      { c.probe.closed.Add(1); return nil }
func (c *probeConn) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.probe.pingErr
}

func TestProviderReusesInvalidatesAndClosesPools(t *testing.T) {
	m, store := testManager(t)
	ctx := context.Background()
	in := validInput()
	c, err := m.Create(ctx, "admin", in)
	if err != nil {
		t.Fatal(err)
	}
	var probes []*poolProbe
	m.openPool = func(_ Connection, password string) (*sql.DB, error) {
		if password != *in.Password {
			t.Error("provider did not decrypt the exact password")
		}
		probe := &poolProbe{}
		probes = append(probes, probe)
		return sql.OpenDB(probe), nil
	}
	first, err := m.Pool(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Pool(ctx, c.ID)
	if err != nil || first != again || len(probes) != 1 {
		t.Fatal("pool not reused")
	}
	in.Name = "Changed"
	if _, err := m.Update(ctx, "admin", c.ID, in); err != nil {
		t.Fatal(err)
	}
	if probes[0].closed.Load() != 1 {
		t.Fatal("edit did not close old pool")
	}
	second, err := m.Pool(ctx, c.ID)
	if err != nil || second == first {
		t.Fatal("edit did not replace pool")
	}
	// Simulate another manager updating the row.
	r := store.rows[c.ID]
	r.revision++
	store.rows[c.ID] = r
	third, err := m.Pool(ctx, c.ID)
	if err != nil || third == second || probes[1].closed.Load() != 1 {
		t.Fatal("external edit did not replace pool")
	}
	if err := m.Delete(ctx, "admin", c.ID); err != nil {
		t.Fatal(err)
	}
	if probes[2].closed.Load() != 1 {
		t.Fatal("delete did not close pool")
	}
	if _, err := m.Pool(ctx, c.ID); err != ErrNotFound {
		t.Fatalf("deleted pool error = %v", err)
	}
	c, err = m.Create(ctx, "admin", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pool(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if probes[3].closed.Load() != 1 {
		t.Fatal("shutdown did not close pool")
	}
	if _, err := m.Pool(ctx, c.ID); err != ErrClosed {
		t.Fatalf("closed provider error = %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal("close is not idempotent")
	}
}

func TestProviderFailureDoesNotLeakOrCacheBrokenPools(t *testing.T) {
	m, store := testManager(t)
	ctx := context.Background()
	c, err := m.Create(ctx, "admin", validInput())
	if err != nil {
		t.Fatal(err)
	}
	probe := &poolProbe{pingErr: errors.New("password=secret, postgres://private")}
	m.openPool = func(Connection, string) (*sql.DB, error) { return sql.OpenDB(probe), nil }
	if _, err := m.Pool(ctx, c.ID); err != ErrConnection {
		t.Fatalf("connection error = %v", err)
	}
	if len(m.pools) != 0 || probe.closed.Load() != 1 {
		t.Fatal("failed pool retained")
	}
	probe = &poolProbe{}
	if _, err := m.Pool(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	probe.pingErr = errors.New("credential details")
	if _, err := m.Pool(ctx, c.ID); err != ErrConnection {
		t.Fatalf("cached pool failure = %v", err)
	}
	if len(m.pools) != 0 || probe.closed.Load() != 1 {
		t.Fatal("broken cached pool retained")
	}
	m.openPool = func(Connection, string) (*sql.DB, error) { return nil, errors.New("password=secret") }
	if _, err := m.Pool(ctx, c.ID); err != ErrConnection {
		t.Fatal("open error was not sanitized")
	}
	r := store.rows[c.ID]
	r.ciphertext[0] = 255
	store.rows[c.ID] = r
	m.openPool = func(Connection, string) (*sql.DB, error) {
		t.Fatal("corrupt credentials reached opener")
		return nil, nil
	}
	if _, err := m.Pool(ctx, c.ID); err != ErrCredentials {
		t.Fatalf("ciphertext error = %v", err)
	}
	if _, err := m.Pool(ctx, "bad-id"); err != ErrInvalidInput {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Pool(cancelled, c.ID); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestProviderConcurrentRequestsUseOnePool(t *testing.T) {
	m, _ := testManager(t)
	ctx := context.Background()
	c, err := m.Create(ctx, "admin", validInput())
	if err != nil {
		t.Fatal(err)
	}
	var opened atomic.Int32
	m.openPool = func(Connection, string) (*sql.DB, error) { opened.Add(1); return sql.OpenDB(&poolProbe{}), nil }
	results := make(chan *sql.DB, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := m.Pool(ctx, c.ID)
			if err != nil {
				t.Error(err)
			}
			results <- db
		}()
	}
	wg.Wait()
	close(results)
	var first *sql.DB
	for db := range results {
		if first == nil {
			first = db
		}
		if db != first {
			t.Error("concurrent requests got distinct pools")
		}
	}
	if opened.Load() != 1 {
		t.Fatalf("opened %d pools", opened.Load())
	}
}

func TestProviderEditDuringPoolCreationInvalidatesOpenedPool(t *testing.T) {
	m, _ := testManager(t)
	ctx := context.Background()
	c, err := m.Create(ctx, "admin", validInput())
	if err != nil {
		t.Fatal(err)
	}
	opening, release := make(chan struct{}), make(chan struct{})
	probe := &poolProbe{}
	m.openPool = func(Connection, string) (*sql.DB, error) {
		close(opening)
		<-release
		return sql.OpenDB(probe), nil
	}
	poolResult := make(chan error, 1)
	go func() {
		_, err := m.Pool(ctx, c.ID)
		poolResult <- err
	}()
	<-opening
	editResult := make(chan error, 1)
	go func() {
		in := validInput()
		in.Password = stringPointer("replacement")
		_, err := m.Update(ctx, "admin", c.ID, in)
		editResult <- err
	}()
	close(release)
	if err := <-poolResult; err != nil {
		t.Fatal(err)
	}
	if err := <-editResult; err != nil {
		t.Fatal(err)
	}
	if probe.closed.Load() != 1 || len(m.pools) != 0 {
		t.Fatal("concurrent edit left a pool with old credentials cached")
	}
}

func TestProviderExternalDeletionAndFailedMutation(t *testing.T) {
	m, store := testManager(t)
	ctx := context.Background()
	c, _ := m.Create(ctx, "admin", validInput())
	probe := &poolProbe{}
	m.openPool = func(Connection, string) (*sql.DB, error) { return sql.OpenDB(probe), nil }
	pool, err := m.Pool(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("database unavailable")
	if err := m.Delete(ctx, "admin", c.ID); err != ErrUnavailable {
		t.Fatal(err)
	}
	if _, err := m.Update(ctx, "admin", c.ID, validInput()); err != ErrUnavailable {
		t.Fatal(err)
	}
	if probe.closed.Load() != 0 {
		t.Fatal("failed mutation invalidated good pool")
	}
	store.err = nil
	if got, _ := m.Pool(ctx, c.ID); got != pool {
		t.Fatal("failed mutation replaced pool")
	}
	delete(store.rows, c.ID)
	if _, err := m.Pool(ctx, c.ID); err != ErrNotFound {
		t.Fatal(err)
	}
	if probe.closed.Load() != 1 {
		t.Fatal("external deletion left pool open")
	}
}

func TestTargetConfigEscapesCredentialsAndSetsLimits(t *testing.T) {
	t.Setenv("PGOPTIONS", "-c statement_timeout=0")
	password := " secret'@:#/$\\password "
	for _, mode := range []string{"disable", "require", "verify-full"} {
		c := Connection{Host: "::1", Port: 5433, Database: "db?name/one", Username: "user@name", SSLMode: mode}
		cfg, err := targetConfig(c, password)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != c.Host || int(cfg.Port) != c.Port || cfg.Database != c.Database || cfg.User != c.Username || cfg.Password != password {
			t.Fatal("connection fields changed")
		}
		if strings.Contains(cfg.ConnString(), password) {
			t.Fatal("pgx retained password in DSN")
		}
		if cfg.ConnectTimeout != operationTimeout || cfg.RuntimeParams["statement_timeout"] != "30000" || cfg.RuntimeParams["options"] != "" {
			t.Fatal("timeouts not configured")
		}
		if mode == "disable" && cfg.TLSConfig != nil {
			t.Fatal("unexpected TLS")
		}
		if mode != "disable" && cfg.TLSConfig == nil {
			t.Fatal("TLS not enabled")
		}
		if mode == "verify-full" && cfg.TLSConfig.InsecureSkipVerify {
			t.Fatal("TLS verification disabled")
		}
		for _, fallback := range cfg.Fallbacks {
			if mode != "disable" && fallback.TLSConfig == nil {
				t.Fatal("TLS mode permits plaintext fallback")
			}
		}
	}
}
