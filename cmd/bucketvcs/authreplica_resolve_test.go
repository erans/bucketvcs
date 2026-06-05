package main

import (
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/authreplica"
)

func TestResolveAuthDBReplica(t *testing.T) {
	cases := []struct {
		name      string
		replica   string // --auth-db-replica
		storeURL  string // --store
		authDB    string // --auth-db (DSN inference)
		isReplica bool   // M26 replica-serve mode
		wantErr   string // substring; "" = ok
		wantAuto  bool   // resolved to system store + DefaultPrefix
		wantURL   string // explicit StoreURL expected on the spec; "" = unchecked
	}{
		{name: "off is nil", replica: "off", storeURL: "localfs:/tmp/x"},
		{name: "empty is nil", replica: "", storeURL: "localfs:/tmp/x"},
		{name: "auto ok", replica: "auto", storeURL: "localfs:/tmp/x", wantAuto: true},
		{name: "auto needs store", replica: "auto", storeURL: "", wantErr: "--store"},
		{name: "explicit url ok", replica: "localfs:/tmp/replica", storeURL: "localfs:/tmp/x", wantURL: "localfs:/tmp/replica"},
		{name: "postgres dsn rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			authDB: "postgres://u@h/db", wantErr: "embedded sqlite"},
		{name: "libsql dsn rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			authDB: "libsql://db.turso.io", wantErr: "embedded sqlite"},
		{name: "http libsql dsn rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			authDB: "http://db.turso.io", wantErr: "embedded sqlite"},
		{name: "https libsql dsn rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			authDB: "https://db.turso.io", wantErr: "embedded sqlite"},
		{name: "replica-serve rejected", replica: "auto", storeURL: "localfs:/tmp/x",
			isReplica: true, wantErr: "replica"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := resolveAuthDBReplica(tc.replica, tc.storeURL, tc.authDB, tc.isReplica)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.replica == "off" || tc.replica == "" {
				if spec != nil {
					t.Fatal("want nil spec when off")
				}
				return
			}
			if spec == nil {
				t.Fatal("want non-nil spec")
			}
			if tc.wantAuto && (spec.UseSystemStore != true || spec.Prefix == "") {
				t.Fatalf("auto not resolved: %+v", spec)
			}
			if tc.wantURL != "" && (spec.StoreURL != tc.wantURL || spec.UseSystemStore || spec.Prefix != authreplica.DefaultPrefix) {
				t.Fatalf("explicit url not resolved: %+v", spec)
			}
		})
	}
}

func TestSQLitePathStripsScheme(t *testing.T) {
	cases := map[string]string{
		"/var/lib/bucketvcs.db":                   "/var/lib/bucketvcs.db",
		"sqlite:/var/lib/bucketvcs.db":            "/var/lib/bucketvcs.db",
		"file:/var/lib/bucketvcs.db":              "/var/lib/bucketvcs.db",
		"file:auth.db?_pragma=busy_timeout(5000)": "auth.db",
	}
	for in, want := range cases {
		if got := sqlitestore.SQLitePath(in); got != want {
			t.Fatalf("SQLitePath(%q) = %q, want %q", in, got, want)
		}
	}
}
