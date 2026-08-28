// Demo: basic CRUD operations with Frigolite.
package main

import (
	"fmt"
	"log"

	"github.com/pijalu/frigolite"
)

func main() {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	fmt.Println("=== Basic CRUD Demo ===")

	// Create table
	res := db.Exec("CREATE TABLE products (id INTEGER, name TEXT, price REAL)")
	if res.Error != nil {
		log.Fatal(res.Error)
	}
	fmt.Println("Created table 'products'")

	// Insert rows
	products := []struct {
		id    int
		name  string
		price float64
	}{
		{1, "Widget", 9.99},
		{2, "Gadget", 24.99},
		{3, "Doohickey", 4.99},
		{4, "Thingamajig", 14.99},
	}

	for _, p := range products {
		sql := fmt.Sprintf("INSERT INTO products VALUES (%d, '%s', %.2f)", p.id, p.name, p.price)
		res := db.Exec(sql)
		if res.Error != nil {
			log.Fatal(res.Error)
		}
		fmt.Printf("Inserted: %s ($%.2f)\n", p.name, p.price)
	}

	// Select all
	res = db.Query("SELECT * FROM products")
	if res.Error != nil {
		log.Fatal(res.Error)
	}
	fmt.Println("\nAll products:")
	for _, row := range res.Rows {
		fmt.Printf("  ID=%v  Name=%v  Price=$%v\n", row[0], row[1], row[2])
	}

	// Select with WHERE
	res = db.Query("SELECT * FROM products WHERE price > 10")
	if res.Error != nil {
		log.Fatal(res.Error)
	}
	fmt.Println("\nProducts over $10:")
	for _, row := range res.Rows {
		fmt.Printf("  %v - $%v\n", row[1], row[2])
	}

	// LIKE query
	pattern := "%gad%"
	res = db.Query("SELECT * FROM products WHERE name LIKE '" + pattern + "'")
	if res.Error != nil {
		log.Fatal(res.Error)
	}
	fmt.Printf("\nProducts matching '%s':\n", pattern)
	for _, row := range res.Rows {
		fmt.Printf("  %v\n", row[1])
	}

	fmt.Println("\n=== Demo Complete ===")
}
