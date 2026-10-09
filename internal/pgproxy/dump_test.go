package pgproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToolConnMovesPasswordToEnv(t *testing.T) {
	conn, env, err := toolConn("postgres://app:s3cret@db.example.com:5433/main?sslmode=require", "other db")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(conn, "s3cret") || !strings.Contains(conn, "/other%20db") || !strings.Contains(conn, "sslmode=require") {
		t.Fatalf("conn %q", conn)
	}
	if len(env) != 1 || env[0] != "PGPASSWORD=s3cret" {
		t.Fatalf("env %v", env)
	}
	conn, env, _ = toolConn("host=/tmp user=me dbname=main", "it's")
	if conn != `host=/tmp user=me dbname=main dbname='it\'s'` || env != nil {
		t.Fatalf("keyword form: %q %v", conn, env)
	}
}

func TestDumpJournal(t *testing.T) {
	t.Setenv("OOPS_HOME", t.TempDir())
	if err := os.MkdirAll(DumpDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, age time.Duration) Dump {
		f := filepath.Join(DumpDir(), name+".dump")
		os.WriteFile(f, []byte("x"), 0o600)
		d, err := addDump(Dump{Database: name, File: f, Created: time.Now().Add(-age)})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	old := mk("old", 10*24*time.Hour)
	fresh := mk("fresh", time.Minute)
	if old.Label() != "d1" || fresh.Label() != "d2" {
		t.Fatalf("labels %s %s", old.Label(), fresh.Label())
	}
	list, err := ListDumps()
	if err != nil || len(list) != 2 || list[0].Database != "fresh" {
		t.Fatalf("list %v %v", list, err)
	}
	if d, err := FindDump("D2"); err != nil || d.Database != "fresh" {
		t.Fatalf("find %v %v", d, err)
	}
	if err := markDumpRestored(fresh.ID); err != nil {
		t.Fatal(err)
	}
	n, err := PruneDumps(7*24*time.Hour, false)
	if err != nil || n != 1 {
		t.Fatalf("prune old: %d %v", n, err)
	}
	if _, err := os.Stat(old.File); !os.IsNotExist(err) {
		t.Fatal("old dump file kept")
	}
	if n, _ := PruneDumps(7*24*time.Hour, true); n != 1 {
		t.Fatal("restored dump not pruned")
	}
	if list, _ := ListDumps(); len(list) != 0 {
		t.Fatalf("left %v", list)
	}
}
