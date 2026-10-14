// Package main is the planted defect for the unwired fixture of setec: an
// exported field that is written and never read. make lint-unwired requires
// the pinned unwired to report it (gibson#807 condition 7).
package main

import "fmt"

// Config holds one read field and one unread field.
type Config struct {
	Read   int
	Unread int
}

func main() {
	c := Config{Read: 1, Unread: 2}
	fmt.Println(c.Read)
}
