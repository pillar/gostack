// Command example is the reference GoStack application with GameKit enabled.
package main

import (
	"os"

	"github.com/pillar/gostack/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:], app.Hooks{}, app.Env{}))
}
