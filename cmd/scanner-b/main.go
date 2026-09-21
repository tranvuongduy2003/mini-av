package main

import (
	"os"

	"miniav/pkg/worker"
	"miniav/pkg/workertransport"
)

func main() {
	os.Exit(worker.Main("scanner-b", workertransport.Socket))
}
