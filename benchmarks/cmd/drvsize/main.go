// Command drvsize is a minimal program that opens one SQLite driver, used to
// compare binary sizes for ADR-0001. Default build uses modernc; -tags drv_mattn selects the CGO driver.
package main

import (
	"database/sql"
	"fmt"
	"log"
)

func main() {
	db, err := sql.Open(driverName, ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	var v string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&v); err != nil {
		log.Fatal(err)
	}
	fmt.Println(driverName, v)
}
