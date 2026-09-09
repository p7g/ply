package main

import (
	"os"
	"ply/internal/companion"
)

func main() { os.Exit(companion.Approve("yolo", os.Args[1:])) }
