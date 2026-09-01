// Package install turns an uploaded build artifact into an app running on a
// device. Android: apk direct, aab via bundletool. iOS: re-sign the ipa with
// the farm's ad-hoc profiles, then deploy.
package install

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/shogo82148/androidbinary/apk"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/model"
)

// Options configures an install run.
type Options struct {
	// BundletoolJar is the path to bundletool for .aab expansion.
	BundletoolJar string
	// SigningIdentity is the codesign identity for iOS re-signing,
	// e.g. "Apple Distribution: Company (TEAMID)".
	SigningIdentity string
	// ProfileDir holds the farm ad-hoc *.mobileprovision files, one per bundle id.
	ProfileDir string
	// WorkDir is a scratch directory for expansion / re-signing.
	WorkDir string
}

// Installer performs installs.
type Installer struct {
	adb  *adb.ADB
	ios  ios.Tools
	opts Options
}

// New builds an Installer.
func New(a *adb.ADB, it ios.Tools, opts Options) *Installer {
	return &Installer{adb: a, ios: it, opts: opts}
}

// Result reports what happened.
type Result struct {
	Package string
	Version string
	Output  string
}

// Run installs artifactPath (as uploaded, keeping its extension) onto dev,
// then launches the app.
func (in *Installer) Run(ctx context.Context, dev model.Device, artifactPath string) (Result, error) {
	ext := strings.ToLower(filepath.Ext(artifactPath))
	switch {
	case dev.Platform == model.Android && ext == ".apk":
		pkg, ver := apkInfo(artifactPath)
		out, err := in.adb.Install(ctx, dev.Serial, artifactPath, true, true)
		if err == nil && pkg != "" {
			_ = in.adb.Launch(ctx, dev.Serial, pkg)
		}
		return Result{Output: out, Package: pkg, Version: ver}, err

	case dev.Platform == model.Android && (ext == ".aab" || ext == ".apks"):
		apks, err := in.expandAAB(ctx, artifactPath)
		if err != nil {
			return Result{}, err
		}
		pkg, ver := apkInfo(apks[0])
		out, err := in.adb.InstallMultiple(ctx, dev.Serial, apks, true)
		if err == nil && pkg != "" {
			_ = in.adb.Launch(ctx, dev.Serial, pkg)
		}
		return Result{Output: out, Package: pkg, Version: ver}, err

	case dev.Platform == model.IOS && ext == ".ipa":
		appBundle, err := in.resignIPA(ctx, artifactPath)
		if err != nil {
			return Result{}, err
		}
		out, err := in.ios.Install(ctx, dev.UDID, appBundle)
		return Result{Output: out}, err

	default:
		return Result{}, fmt.Errorf("cannot install %s on %s device", ext, dev.Platform)
	}
}

// expandAAB produces a universal split set for the connected device.
func (in *Installer) expandAAB(ctx context.Context, aab string) ([]string, error) {
	if in.opts.BundletoolJar == "" {
		return nil, fmt.Errorf("bundletool jar not configured")
	}
	outDir, err := os.MkdirTemp(in.opts.WorkDir, "aab-*")
	if err != nil {
		return nil, err
	}
	apks := filepath.Join(outDir, "out.apks")
	cmd := exec.CommandContext(ctx, "java", "-jar", in.opts.BundletoolJar,
		"build-apks", "--mode=universal", "--bundle="+aab, "--output="+apks)
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("bundletool: %v: %s", err, b)
	}
	// universal.apk lives inside the .apks zip
	universal := filepath.Join(outDir, "universal.apk")
	if err := unzipOne(apks, "universal.apk", universal); err != nil {
		return nil, err
	}
	return []string{universal}, nil
}

// resignIPA re-signs the ipa with the farm identity + profiles and returns the
// path to the extracted, re-signed .app bundle ready for ios-deploy.
//
// This shells out to `fastlane run resign`, which handles the app bundle and
// every embedded extension. ProfileDir must contain one <bundleid>.mobileprovision
// per binary.
func (in *Installer) resignIPA(ctx context.Context, ipa string) (string, error) {
	if in.opts.SigningIdentity == "" || in.opts.ProfileDir == "" {
		return "", fmt.Errorf("iOS re-signing not configured (signing_identity / profile_dir)")
	}
	work, err := os.MkdirTemp(in.opts.WorkDir, "resign-*")
	if err != nil {
		return "", err
	}
	staged := filepath.Join(work, "app.ipa")
	if err := copyFile(ipa, staged); err != nil {
		return "", err
	}

	bundleIDs, err := ipaBundleIDs(staged)
	if err != nil {
		return "", err
	}
	args := []string{"run", "resign", "ipa:" + staged, "signing_identity:" + in.opts.SigningIdentity}
	for _, id := range bundleIDs {
		prof := filepath.Join(in.opts.ProfileDir, id+".mobileprovision")
		if _, err := os.Stat(prof); err != nil {
			return "", fmt.Errorf("no farm profile for bundle id %q (%s)", id, prof)
		}
		args = append(args, fmt.Sprintf("provisioning_profile:%s:%s", id, prof))
	}
	cmd := exec.CommandContext(ctx, "fastlane", args...)
	cmd.Dir = work
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("resign: %v: %s", err, tail(string(b), 2000))
	}

	appDir := filepath.Join(work, "Payload")
	if err := unzipDir(staged, "Payload/", work); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(appDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".app") {
			return filepath.Join(appDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no .app in re-signed ipa")
}

// ipaBundleIDs returns the CFBundleIdentifier of the app and every .appex.
func ipaBundleIDs(ipa string) ([]string, error) {
	zr, err := zip.OpenReader(ipa)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var ids []string
	for _, f := range zr.File {
		// Payload/App.app/Info.plist and Payload/App.app/PlugIns/*.appex/Info.plist
		if !strings.HasSuffix(f.Name, "/Info.plist") {
			continue
		}
		rel := strings.TrimPrefix(f.Name, "Payload/")
		depth := strings.Count(rel, "/")
		if depth != 1 && !strings.Contains(rel, ".appex/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data := make([]byte, f.UncompressedSize64)
		_, _ = rc.Read(data)
		rc.Close()
		if id := plistString(data, "CFBundleIdentifier"); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no bundle ids found in ipa")
	}
	return ids, nil
}

// plistString does a crude extraction of <key>NAME</key><string>VALUE</string>
// from a binary or xml plist. Good enough for CFBundleIdentifier; swap for a
// real plist parser if this proves fragile.
func plistString(data []byte, key string) string {
	s := string(data)
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func unzipOne(zipPath, member, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != member {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = out.ReadFrom(rc)
		return err
	}
	return fmt.Errorf("%s not found in %s", member, zipPath)
}

func unzipDir(zipPath, prefix, dstRoot string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		target := filepath.Join(dstRoot, filepath.Clean(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = out.ReadFrom(rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// apkInfo reads the package name and version name from an APK's manifest.
// Returns empty strings if the APK cannot be parsed.
func apkInfo(path string) (pkg, version string) {
	pkgApk, err := apk.OpenFile(path)
	if err != nil {
		return "", ""
	}
	defer pkgApk.Close()
	m := pkgApk.Manifest()
	pkg, _ = m.Package.String()
	version, _ = m.VersionName.String()
	return pkg, version
}
