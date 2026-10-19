package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func cmdConfig(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: forseti config list|get|set|path|edit|sync|init")
		os.Exit(2)
	}
	sub := args[0]
	rest := args[1:]
	cfg := loadConfig()

	switch sub {
	case "list", "ls":
		for _, k := range configKeys {
			v, _ := getKey(cfg, k)
			fmt.Printf("%-22s %s\n", k, v)
		}
		fmt.Println("\nfiles:")
		targets := configTargets(cfg)
		names := make([]string, 0, len(targets))
		for n := range targets {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			mark := " "
			if fileExists(targets[n]) {
				mark = "*"
			}
			fmt.Printf("  %s %-14s %s\n", mark, n, targets[n])
		}
	case "get":
		if len(rest) != 1 {
			die("usage: forseti config get KEY")
		}
		v, ok := getKey(cfg, rest[0])
		if !ok {
			die("unknown key %q", rest[0])
		}
		fmt.Println(v)
	case "set":
		if len(rest) != 2 {
			die("usage: forseti config set KEY VALUE")
		}
		if err := setKey(&cfg, rest[0], rest[1]); err != nil {
			die("%v", err)
		}
		if err := saveConfig(cfg); err != nil {
			die("save: %v", err)
		}
		fmt.Printf("%s = %s\n", rest[0], rest[1])
	case "path":
		targets := configTargets(cfg)
		if len(rest) == 0 {
			names := make([]string, 0, len(targets))
			for n := range targets {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Printf("%-14s %s\n", n, targets[n])
			}
			return
		}
		for _, name := range rest {
			p, ok := targets[name]
			if !ok {
				die("unknown target %q (try: forseti config path)", name)
			}
			fmt.Println(p)
		}
	case "edit":
		if len(rest) != 1 {
			die("usage: forseti config edit TARGET (try: forseti config path)")
		}
		targets := configTargets(cfg)
		p, ok := targets[rest[0]]
		if !ok {
			die("unknown target %q (try: forseti config path)", rest[0])
		}
		if rest[0] == "forseti" {
			if _, err := os.Stat(p); err != nil {
				if err := saveConfig(cfg); err != nil {
					die("create %s: %v", p, err)
				}
			}
		}
		if rest[0] == "tools" {
			if err := os.MkdirAll(p, 0o755); err != nil {
				die("mkdir %s: %v", p, err)
			}
			fmt.Printf("%s (directory)\n", p)
			return
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			die("mkdir: %v", err)
		}
		if _, err := os.Stat(p); err != nil {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				die("create %s: %v", p, err)
			}
			_ = f.Close()
		}
		editor := os.Getenv("VISUAL")
		if editor == "" {
			editor = os.Getenv("EDITOR")
		}
		if editor == "" {
			editor = "vi"
		}
		if err := runInteractive("", nil, editor, p); err != nil {
			die("%s: %v", editor, err)
		}
	case "sync":
		fs := flag.NewFlagSet("config sync", flag.ExitOnError)
		dry := fs.Bool("dry-run", false, "show what would change")
		_ = fs.Parse(rest)
		repo, err := findRepo(cfg)
		if err != nil {
			die("%v", err)
		}
		results, err := syncAssets(repo, *dry)
		if err != nil {
			die("sync: %v", err)
		}
		for _, r := range results {
			switch {
			case r.Changed && *dry:
				fmt.Printf("would sync  %-14s %s\n", r.Asset.Label, r.Asset.Dst)
			case r.Changed:
				fmt.Printf("synced      %-14s %s\n", r.Asset.Label, r.Asset.Dst)
			default:
				fmt.Printf("in sync     %-14s %s\n", r.Asset.Label, r.Asset.Dst)
			}
		}
	case "init":
		path := configPath()
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("exists      %s\n", path)
			return
		}
		if err := saveConfig(cfg); err != nil {
			die("write %s: %v", path, err)
		}
		fmt.Printf("created     %s\n", path)
	default:
		die("unknown config command %q (list|get|set|path|edit|sync|init)", sub)
	}
}
