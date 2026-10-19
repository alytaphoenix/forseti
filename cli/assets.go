package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// asset is one forseti-owned file that must match the checkout in the installed
// locations (the ttt plugin dir and the user-level pi extension).
type asset struct {
	Label string `json:"label"`
	Src   string `json:"src"`
	Dst   string `json:"dst"`
}

func assets(repo string) []asset {
	return []asset{
		{"ttt plugin", filepath.Join(repo, "ttt-plugin", "init.lua"), filepath.Join(tttPluginDir(), "init.lua")},
		{"ttt manifest", filepath.Join(repo, "ttt-plugin", "plugin.ttt.json"), filepath.Join(tttPluginDir(), "plugin.ttt.json")},
		{"pi extension", filepath.Join(repo, "pi-extension", "index.ts"), piExtensionPath()},
	}
}

type assetResult struct {
	Asset   asset `json:"asset"`
	Exists  bool  `json:"exists"`
	Changed bool  `json:"changed"`
}

// inspectAssets compares source and destination bytes.
func inspectAssets(repo string) []assetResult {
	var out []assetResult
	for _, a := range assets(repo) {
		src, serr := os.ReadFile(a.Src)
		dst, derr := os.ReadFile(a.Dst)
		r := assetResult{Asset: a, Exists: derr == nil}
		r.Changed = serr == nil && (derr != nil || !bytes.Equal(src, dst))
		out = append(out, r)
	}
	return out
}

// syncAssets copies changed assets. The installed copies are plain files (ttt
// does not load symlinked plugins) and are written 0644; nothing here touches
// the permission registry.
func syncAssets(repo string, dry bool) ([]assetResult, error) {
	results := inspectAssets(repo)
	for i := range results {
		r := &results[i]
		if !r.Changed {
			continue
		}
		if dry {
			continue
		}
		data, err := os.ReadFile(r.Asset.Src)
		if err != nil {
			return results, fmt.Errorf("read %s: %w", r.Asset.Src, err)
		}
		if err := os.MkdirAll(filepath.Dir(r.Asset.Dst), 0o755); err != nil {
			return results, fmt.Errorf("mkdir %s: %w", filepath.Dir(r.Asset.Dst), err)
		}
		tmp := r.Asset.Dst + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return results, fmt.Errorf("write %s: %w", r.Asset.Dst, err)
		}
		if err := os.Rename(tmp, r.Asset.Dst); err != nil {
			return results, fmt.Errorf("install %s: %w", r.Asset.Dst, err)
		}
		r.Exists = true
	}
	return results, nil
}

func assetDrift(repo string) bool {
	for _, r := range inspectAssets(repo) {
		if r.Changed {
			return true
		}
	}
	return false
}

// herdrPluginLinked reports whether herdr knows the forseti plugin (linked or
// installed). `herdr plugin list` needs a running server.
func herdrPluginLinked(c Config) bool {
	out, err := runCapture("", nil, c.Herdr.Bin, "plugin", "list")
	if err != nil {
		return false
	}
	return strings.Contains(out, "forseti")
}
