package memdb

import (
	"context"
	"errors"
	"testing"
)

func TestClonedTableDoesNotLeakMutableValues(t *testing.T) {
	type row struct {
		ID   string
		Data []byte
	}
	table := NewClonedTable("json", func(v row) row { v.Data = append([]byte(nil), v.Data...); return v }, func(v row) string { return v.ID })
	db := New()
	ctx := context.Background()
	original := row{"row", []byte("original")}
	if err := db.Update(ctx, "tenant", func(tx Tx) error { return table.Put(tx, original) }); err != nil {
		t.Fatal(err)
	}
	original.Data[0] = 'X'
	stop := errors.New("rollback")
	if err := db.Update(ctx, "tenant", func(tx Tx) error {
		v, _ := table.Get(tx, "row")
		v.Data[0] = 'Y'
		if err := table.Put(tx, v); err != nil {
			return err
		}
		return stop
	}); !errors.Is(err, stop) {
		t.Fatalf("rollback=%v", err)
	}
	if err := db.View(ctx, "tenant", func(tx Tx) error {
		table.Scan(tx, func(v row) bool { v.Data[0] = 'Z'; return true })
		v, _ := table.Get(tx, "row")
		if string(v.Data) != "original" {
			t.Fatalf("mutable value escaped isolation: %s", v.Data)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
