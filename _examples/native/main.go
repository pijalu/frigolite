// Package main shows basic usage of frigolite with the native API.
package main

import (
	"fmt"
	"log"

	"github.com/pijalu/frigolite"
)

func main() {
	// Open an in-memory database
	db, err := frigolite.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create a table
	db.Exec("CREATE TABLE users (id INTEGER, name TEXT, age INTEGER)")

	// Insert data
	db.Exec("INSERT INTO users VALUES (1, 'Alice', 30)")
	db.Exec("INSERT INTO users VALUES (2, 'Bob', 25)")
	db.Exec("INSERT INTO users VALUES (3, 'Charlie', 35)")

	// Query with WHERE
	res := db.Query("SELECT id, name, age FROM users WHERE age > 25")
	if res.Error != nil {
		log.Fatal(res.Error)
	}

	fmt.Println("=== Users over 25 (native API) ===")
	for _, row := range res.Rows {
		fmt.Printf("  id=%v name=%v age=%v\n", row[0], row[1], row[2])
	}
}
