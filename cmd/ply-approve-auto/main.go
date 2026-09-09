package main

import (
	"os"
	"ply/internal/companion"
)

func main() { os.Exit(companion.Approve("auto", os.Args[1:])) }
