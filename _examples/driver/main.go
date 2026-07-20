// Package main shows usage of frigolite with the standard database/sql interface.
package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/pijalu/frigolite/frigodb"
)

func main() {
	// Open via database/sql driver
	db, err := sql.Open("frigolite", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create a table
	_, err = db.Exec(`CREATE TABLE users (
		id   INTEGER,
		name TEXT,
		age  INTEGER
	)`)
	if err != nil {
		log.Fatal(err)
	}

	// Insert data with placeholders
	for _, u := range []struct {
		id   int
		name string
		age  int
	}{
		{1, "Alice", 30},
		{2, "Bob", 25},
		{3, "Charlie", 35},
	} {
		_, err = db.Exec("INSERT INTO users VALUES (?, ?, ?)", u.id, u.name, u.age)
		if err != nil {
			log.Fatal(err)
		}
	}

	// Query with placeholder
	rows, err := db.Query("SELECT id, name, age FROM users WHERE age > ?", 25)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	fmt.Println("=== Users over 25 (database/sql) ===")
	for rows.Next() {
		var id int
		var name string
		var age int
		if err := rows.Scan(&id, &name, &age); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  id=%d name=%s age=%d\n", id, name, age)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}

	// Prepared statement
	stmt, err := db.Prepare("INSERT INTO users VALUES (?, ?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()

	_, err = stmt.Exec(4, "Diana", 28)
	if err != nil {
		log.Fatal(err)
	}

	// Count rows
	var count int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	fmt.Printf("\nTotal users: %d\n", count)
}
