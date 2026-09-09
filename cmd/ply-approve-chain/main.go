package main

import (
	"os"
	"ply/internal/companion"
)

func main() { os.Exit(companion.Approve("chain", os.Args[1:])) }
