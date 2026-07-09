package main

import (
	"os"

	"hitman/internal/hitman"
)

func main() {
	os.Exit(hitman.Run(os.Args[1:]))
}
