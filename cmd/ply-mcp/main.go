package main

import (
	"os"
	"ply/internal/companion"
)

func main() { os.Exit(companion.MCP(os.Args[1:])) }
