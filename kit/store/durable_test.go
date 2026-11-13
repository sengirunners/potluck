package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
)

func TestDurableReservationUsesFullAndRollsBack(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	st, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.Registry()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE durable_fixture(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("fixture rollback")
	err = DurableTx(ctx, db, func(tx *sql.Tx) error {
		var mode int
		if err := tx.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil || mode < 2 {
			t.Fatalf("reservation was not FULL: %d %v", mode, err)
		}
		if _, err := tx.Exec(`INSERT INTO durable_fixture VALUES (1)`); err != nil {
			return err
		}
		return fail
	})
	if !errors.Is(err, fail) {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM durable_fixture`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if err := db.QueryRow("PRAGMA synchronous").Scan(&n); err != nil || n != 1 {
		t.Fatal("did not restore normal connection", n, err)
	}
}
