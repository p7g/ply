package main

import (
	"os"
	"ply/internal/companion"
)

func main() { os.Exit(companion.Approve("ask", os.Args[1:])) }
