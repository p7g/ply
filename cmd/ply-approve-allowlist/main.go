package main

import (
	"os"
	"ply/internal/companion"
)

func main() { os.Exit(companion.Approve("allowlist", os.Args[1:])) }
