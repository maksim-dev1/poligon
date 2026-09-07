package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/store"
)

// tokenCmd manages personal API tokens for scripted / CI callers.
//
//	poligon token create <email> [name]
//	poligon token list <email>
//	poligon token revoke <email> <hash-prefix>
func tokenCmd(cfgPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: poligon token <create|list|revoke> ...")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New("usage: poligon token create <email> [name]")
		}
		email := args[1]
		if _, err := st.User(email); err != nil {
			return fmt.Errorf("no such user %q (create it with: poligon user add)", email)
		}
		name := ""
		if len(args) > 2 {
			name = strings.Join(args[2:], " ")
		}
		raw := "plgn_" + randHex(32)
		if err := st.CreateAPIToken(sha256Hex(raw), email, name); err != nil {
			return err
		}
		fmt.Printf("token for %q created%s\n\n  %s\n\nStore it now — it is not shown again.\nUse it as:  Authorization: Bearer %s\n",
			email, labelSuffix(name), raw, raw)
		return nil

	case "list":
		if len(args) < 2 {
			return errors.New("usage: poligon token list <email>")
		}
		toks, err := st.APITokens(args[1])
		if err != nil {
			return err
		}
		if len(toks) == 0 {
			fmt.Println("(no tokens)")
			return nil
		}
		for _, t := range toks {
			last := "never"
			if t.LastUsedAt != nil {
				last = t.LastUsedAt.Format("2006-01-02 15:04")
			}
			fmt.Printf("%s  %-20s  created %s  last used %s\n",
				t.HashPrefix, dash(t.Name), t.CreatedAt.Format("2006-01-02"), last)
		}
		return nil

	case "revoke":
		if len(args) < 3 {
			return errors.New("usage: poligon token revoke <email> <hash-prefix>")
		}
		n, err := st.DeleteAPITokenByPrefix(args[1], args[2])
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("no token matched that prefix for this user")
		}
		fmt.Printf("revoked %d token(s)\n", n)
		return nil

	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func labelSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " (" + name + ")"
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
