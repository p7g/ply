package companion

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type skill struct{ name, path, description string }

func projectDirs() []string {
	cwd, _ := os.Getwd()
	var dirs []string
	for {
		dirs = append(dirs, filepath.Join(cwd, ".ply"))
		p := filepath.Dir(cwd)
		if p == cwd {
			break
		}
		cwd = p
	}
	return dirs
}
func Skill(args []string) int {
	found := map[string]skill{}
	dirs := append(projectDirs(), UserDir())
	for _, dir := range dirs {
		paths, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
		for _, p := range paths {
			name := filepath.Base(filepath.Dir(p))
			if _, ok := found[name]; ok {
				continue
			}
			b, e := os.ReadFile(p)
			if e != nil {
				continue
			}
			desc := ""
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "description:") {
					desc = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "description:")), "\"'")
					break
				}
			}
			found[name] = skill{name, p, desc}
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ply-skill list | show NAME")
		return 2
	}
	switch args[0] {
	case "list", "--ply-prompt":
		keys := []string{}
		for k := range found {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if args[0] == "--ply-prompt" && len(keys) > 0 {
			fmt.Println("Skills available via `ply-skill show NAME`:")
		}
		for _, k := range keys {
			fmt.Printf("%s: %s\n", k, found[k].description)
		}
		return 0
	case "show":
		if len(args) != 2 {
			break
		}
		s, ok := found[args[1]]
		if !ok {
			fmt.Fprintln(os.Stderr, "skill not found:", args[1])
			return 1
		}
		b, e := os.ReadFile(s.path)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		os.Stdout.Write(b)
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: ply-skill list | show NAME")
	return 2
}
