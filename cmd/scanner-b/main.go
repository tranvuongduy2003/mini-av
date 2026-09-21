package main

import (
	"os"

	"miniav/pkg/worker"
)

func main() {
	os.Exit(worker.Main("scanner-b"))
}
