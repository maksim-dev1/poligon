package install

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// XcodeProfileDirs are where Xcode keeps the provisioning profiles it manages
// (automatic signing writes the team's wildcard profile here when it builds
// WebDriverAgent) — Xcode 16+ first, then the older location.
func XcodeProfileDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, "Library/Developer/Xcode/UserData/Provisioning Profiles"),
		filepath.Join(home, "Library/MobileDevice/Provisioning Profiles"),
	}
}

// Identity is a codesigning identity in the keychain.
type Identity struct {
	Hash string // SHA-1 of the certificate, upper-case hex
	Name string // e.g. "Apple Development: Jane Doe (ABCDE12345)"
}

var identityRe = regexp.MustCompile(`^\s*\d+\)\s+([0-9A-F]{40})\s+"(.+)"`)

// ResolveIdentity finds the codesigning identity to re-sign with. spec may be
// a SHA-1, a full name or part of one; empty picks the keychain's only valid
// identity (a farm host normally has exactly one).
func ResolveIdentity(ctx context.Context, spec string) (Identity, error) {
	out, err := exec.CommandContext(ctx, "security", "find-identity", "-v", "-p", "codesigning").Output()
	if err != nil {
		return Identity{}, fmt.Errorf("security find-identity: %w", err)
	}
	ids := parseIdentities(string(out))
	if spec == "" {
		switch len(ids) {
		case 1:
			return ids[0], nil
		case 0:
			return Identity{}, errors.New("no valid codesigning identity in the keychain — install the team's Apple Development certificate on the host")
		default:
			names := make([]string, len(ids))
			for i, id := range ids {
				names[i] = id.Name
			}
			return Identity{}, fmt.Errorf("%d codesigning identities in the keychain — set POLIGON_SIGNING_IDENTITY to one of: %s", len(ids), strings.Join(names, "; "))
		}
	}
	for _, id := range ids {
		if strings.EqualFold(id.Hash, spec) || id.Name == spec {
			return id, nil
		}
	}
	var hits []Identity
	for _, id := range ids {
		if strings.Contains(id.Name, spec) {
			hits = append(hits, id)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	return Identity{}, fmt.Errorf("POLIGON_SIGNING_IDENTITY %q matches %d valid identities in the keychain", spec, len(hits))
}

func parseIdentities(out string) []Identity {
	var ids []Identity
	seen := map[string]bool{}
	for _, ln := range strings.Split(out, "\n") {
		if m := identityRe.FindStringSubmatch(ln); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, Identity{Hash: m[1], Name: m[2]})
		}
	}
	return ids
}

// profile is one decoded .mobileprovision.
type profile struct {
	path         string
	name         string
	appID        string // e.g. TEAMID.com.acme.app or TEAMID.*
	team         string
	expires      time.Time
	certs        map[string]bool // SHA-1 (upper hex) of each developer certificate
	devices      map[string]bool // provisioned UDIDs (upper-case)
	allDevices   bool            // enterprise: ProvisionsAllDevices
	entitlements []byte
}

func (p profile) covers(udid string) bool {
	return p.allDevices || udid == "" || p.devices[strings.ToUpper(udid)]
}

// loadProfiles reads every .mobileprovision in dirs (missing dirs are skipped)
// and keeps the ones usable to sign for udid with the given certificate: not
// expired, issued for that certificate, and listing the device. The error
// says which of those was missing, since that is what a person has to fix.
func loadProfiles(dirs []string, certHash, udid string, now time.Time) ([]profile, error) {
	var all []profile
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".mobileprovision") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := exec.Command("security", "cms", "-D", "-i", path).Output()
			if err != nil {
				continue
			}
			p, err := parseProfile(raw)
			if err != nil {
				continue
			}
			p.path = path
			all = append(all, p)
		}
	}
	return pickProfiles(all, dirs, certHash, udid, now)
}

// pickProfiles keeps the profiles usable to sign for udid with certHash.
func pickProfiles(all []profile, dirs []string, certHash, udid string, now time.Time) ([]profile, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("no provisioning profiles found (looked in %s) — build WebDriverAgent once with Xcode automatic signing, or drop a .mobileprovision into POLIGON_PROFILE_DIR", strings.Join(dirs, ", "))
	}
	var ok []profile
	expired, otherCert, otherDevice := 0, 0, 0
	for _, p := range all {
		switch {
		case !p.expires.IsZero() && p.expires.Before(now):
			expired++
		case certHash != "" && !p.certs[strings.ToUpper(certHash)]:
			otherCert++
		case !p.covers(udid):
			otherDevice++
		default:
			ok = append(ok, p)
		}
	}
	if len(ok) == 0 {
		return nil, fmt.Errorf("none of %d provisioning profiles fits: %d expired, %d issued for another certificate, %d without this device (UDID %s) — register the device in the Apple developer team and refresh the profile (Xcode: build WebDriverAgent for it once)",
			len(all), expired, otherCert, otherDevice, udid)
	}
	return ok, nil
}

// parseProfile reads the fields signing needs from a decoded profile plist.
func parseProfile(raw []byte) (profile, error) {
	top, err := plistDict(raw)
	if err != nil {
		return profile{}, err
	}
	p := profile{certs: map[string]bool{}, devices: map[string]bool{}}
	p.name, _ = top["Name"].(string)
	if s, ok := top["ExpirationDate"].(string); ok {
		p.expires, _ = time.Parse(time.RFC3339, s)
	}
	if a, ok := top["TeamIdentifier"].([]any); ok && len(a) > 0 {
		p.team, _ = a[0].(string)
	}
	if ents, ok := top["Entitlements"].(map[string]any); ok {
		p.appID, _ = ents["application-identifier"].(string)
	}
	if a, ok := top["DeveloperCertificates"].([]any); ok {
		for _, c := range a {
			if der, ok := c.([]byte); ok {
				sum := sha1.Sum(der)
				p.certs[strings.ToUpper(hex.EncodeToString(sum[:]))] = true
			}
		}
	}
	if a, ok := top["ProvisionedDevices"].([]any); ok {
		for _, d := range a {
			if s, ok := d.(string); ok {
				p.devices[strings.ToUpper(s)] = true
			}
		}
	}
	p.allDevices, _ = top["ProvisionsAllDevices"].(bool)
	p.entitlements = extractEntitlements(raw)
	return p, nil
}

// plistDict decodes an XML plist whose root is a dict. Values: string (also
// date, integer, real), bool, []byte (data), []any (array), map[string]any.
func plistDict(raw []byte) (map[string]any, error) {
	d := xml.NewDecoder(bytes.NewReader(raw))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			v, err := plistValue(d, se)
			if err != nil {
				return nil, err
			}
			return v.(map[string]any), nil
		}
	}
}

func plistValue(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		key := ""
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					var k string
					if err := d.DecodeElement(&k, &t); err != nil {
						return nil, err
					}
					key = k
					continue
				}
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
			case xml.EndElement:
				return m, nil
			}
		}
	case "array":
		var a []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			case xml.EndElement:
				return a, nil
			}
		}
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	case "data":
		var s string
		if err := d.DecodeElement(&s, &se); err != nil {
			return nil, err
		}
		return decodeB64(s)
	default: // string, date, integer, real
		var s string
		if err := d.DecodeElement(&s, &se); err != nil {
			return nil, err
		}
		return strings.TrimSpace(s), nil
	}
}

func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
}
