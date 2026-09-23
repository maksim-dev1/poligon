package runner

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A Maestro workspace is a zipped .maestro/ directory: config.yaml, flows/,
// subflows/ and any files the flows reference (runFlow, runScript, addMedia).
// A single uploaded .yaml cannot call `runFlow: ../subflows/login.yaml`, so
// real suites have to arrive as a workspace.

const (
	maxWorkspaceFiles = 2000
	maxWorkspaceBytes = 512 << 20 // uncompressed, whole archive
)

// IsZip reports whether the file at path starts with the zip magic — flow_url
// downloads carry no trustworthy extension.
func IsZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	return n == 4 && bytes.Equal(head, []byte("PK\x03\x04"))
}

// ExtractWorkspace unpacks the zip at zipPath into dest and returns the
// Maestro target inside it: the workspace root, or flowPath resolved against
// that root when given. A zip holding one top-level directory (".maestro/",
// say) is treated as that directory. Entries escaping dest, symlinks and
// archives over the size/count caps are rejected.
func ExtractWorkspace(zipPath, dest, flowPath string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("flow workspace: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > maxWorkspaceFiles {
		return "", fmt.Errorf("flow workspace: more than %d files", maxWorkspaceFiles)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}

	var total int64
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		if strings.HasPrefix(f.Name, "__MACOSX/") || filepath.Base(name) == ".DS_Store" {
			continue
		}
		target, err := insideDir(dest, name)
		if err != nil {
			return "", err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			return "", fmt.Errorf("flow workspace: %s is not a regular file", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		n, err := copyZipFile(f, target, maxWorkspaceBytes-total)
		if err != nil {
			return "", err
		}
		total += n
	}

	root := workspaceRoot(dest)
	if flowPath == "" {
		if !hasYAML(root) {
			return "", errors.New("flow workspace has no .yaml flows")
		}
		return root, nil
	}
	target, err := insideDir(root, filepath.FromSlash(flowPath))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("flow_path %q not found in the workspace (have: %s)", flowPath, strings.Join(listYAML(root), ", "))
	}
	return target, nil
}

// insideDir joins name onto dir and fails if the result leaves dir.
func insideDir(dir, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("flow workspace: absolute path %q", name)
	}
	target := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("flow workspace: path %q escapes the workspace", name)
	}
	return target, nil
}

func copyZipFile(f *zip.File, target string, budget int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out, err := os.Create(target)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(rc, budget+1))
	if err != nil {
		return n, err
	}
	if n > budget {
		return n, fmt.Errorf("flow workspace: larger than %d MiB unpacked", maxWorkspaceBytes>>20)
	}
	return n, nil
}

// workspaceRoot descends through a lone top-level directory, the shape a
// zipped ".maestro" folder has.
func workspaceRoot(dir string) string {
	for {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 || !entries[0].IsDir() {
			return dir
		}
		dir = filepath.Join(dir, entries[0].Name())
	}
}

var yamlExt = regexp.MustCompile(`(?i)\.ya?ml$`)

func hasYAML(root string) bool { return len(listYAML(root)) > 0 }

func listYAML(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && yamlExt.MatchString(p) {
			if rel, e := filepath.Rel(root, p); e == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}
