package main

import (
	"os"
	"ply/internal/companion"
)

func main() { os.Exit(companion.Skill(os.Args[1:])) }
