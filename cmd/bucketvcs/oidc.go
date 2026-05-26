package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

const oidcUsage = `Usage: bucketvcs oidc <object> <action> [flags]

Objects + actions:
  issuer add    --auth-db=<path> --alias=<name> --url=<issuer-url>
  issuer list   --auth-db=<path> [--format=text|json]
  issuer remove --auth-db=<path> --alias=<name>
  rule add      --auth-db=<path> --issuer=<alias> --audience=<aud>
                --tenant=<t> --repo=<r> --scopes=<csv> --ttl=<dur>
                [--claim name=value ...]
  rule list     --auth-db=<path> [--issuer=<alias> | --repo=<t>/<r>] [--format=text|json]
  rule remove   --auth-db=<path> --id=<bvor_...>

Exit codes: 0 ok | 1 operational | 2 usage.
TTL is a Go duration (e.g. 15m). Maximum 1h. --audience is required.
Claim matching is exact string equality; repeat --claim for multiple
constraints; omit --claim entirely for an issuer-wide (wildcard) rule.`

const oidcTTLCeiling = time.Hour

func runOIDC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, oidcUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "issuer":
		return runOIDCIssuer(ctx, args[1:], stdout, stderr)
	case "rule":
		return runOIDCRule(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "oidc: unknown object %q\n%s", args[0], oidcUsage)
		return 2
	}
}

func openOIDCStore(authDB string) (*sqlitestore.Store, error) {
	if authDB == "" {
		return nil, fmt.Errorf("--auth-db required")
	}
	return sqlitestore.Open(authDB)
}

func runOIDCIssuer(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "oidc issuer: action required (add|list|remove)")
		return 2
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("oidc issuer add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		alias := fs.String("alias", "", "")
		urlF := fs.String("url", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *alias == "" || *urlF == "" {
			fmt.Fprintln(stderr, "oidc issuer add: --auth-db, --alias, --url required")
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer add: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.AddOIDCIssuer(ctx, *alias, *urlF); err != nil {
			fmt.Fprintf(stderr, "oidc issuer add: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "alias=%s  url=%s\n", *alias, *urlF)
		return 0
	case "list":
		fs := flag.NewFlagSet("oidc issuer list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		format := fs.String("format", "text", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer list: %v\n", err)
			return 1
		}
		defer st.Close()
		issuers, err := st.ListOIDCIssuers(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer list: %v\n", err)
			return 1
		}
		for _, i := range issuers {
			if *format == "json" {
				b, _ := json.Marshal(map[string]any{"alias": i.Alias, "url": i.IssuerURL, "created_at": i.CreatedAt})
				fmt.Fprintln(stdout, string(b))
			} else {
				fmt.Fprintf(stdout, "alias=%s  url=%s\n", i.Alias, i.IssuerURL)
			}
		}
		return 0
	case "remove":
		fs := flag.NewFlagSet("oidc issuer remove", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		alias := fs.String("alias", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *alias == "" {
			fmt.Fprintln(stderr, "oidc issuer remove: --auth-db, --alias required")
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc issuer remove: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.RemoveOIDCIssuer(ctx, *alias); err != nil {
			fmt.Fprintf(stderr, "oidc issuer remove: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed alias=%s\n", *alias)
		return 0
	default:
		fmt.Fprintf(stderr, "oidc issuer: unknown action %q\n", args[0])
		return 2
	}
}

// claimFlags collects repeated --claim name=value pairs.
type claimFlags map[string]string

func (c claimFlags) String() string { return "" }
func (c claimFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("claim must be name=value")
	}
	c[k] = val
	return nil
}

func runOIDCRule(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "oidc rule: action required (add|list|remove)")
		return 2
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("oidc rule add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		issuer := fs.String("issuer", "", "")
		audience := fs.String("audience", "", "")
		tenant := fs.String("tenant", "", "")
		repo := fs.String("repo", "", "")
		scopesF := fs.String("scopes", "", "")
		ttl := fs.Duration("ttl", 15*time.Minute, "")
		claims := claimFlags{}
		fs.Var(claims, "claim", "repeatable name=value claim constraint")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *issuer == "" || *audience == "" || *tenant == "" || *repo == "" || *scopesF == "" {
			fmt.Fprintln(stderr, "oidc rule add: --auth-db, --issuer, --audience, --tenant, --repo, --scopes required")
			return 2
		}
		if *ttl <= 0 || *ttl > oidcTTLCeiling {
			fmt.Fprintf(stderr, "oidc rule add: --ttl must be > 0 and <= %s\n", oidcTTLCeiling)
			return 2
		}
		scopes, err := auth.ParseScopes(*scopesF)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule add: %v\n", err)
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule add: %v\n", err)
			return 1
		}
		defer st.Close()
		id, err := st.AddOIDCRule(ctx, auth.OIDCTrustRule{
			IssuerAlias: *issuer, Audience: *audience, Tenant: *tenant, Repo: *repo,
			Scopes: scopes, TTLSeconds: int64(ttl.Seconds()), Claims: map[string]string(claims),
		})
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule add: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "id=%s  issuer=%s  tenant=%s  repo=%s  scopes=%s  ttl=%s  claims=%d\n",
			id, *issuer, *tenant, *repo, auth.FormatScopes(scopes), *ttl, len(claims))
		return 0
	case "list":
		fs := flag.NewFlagSet("oidc rule list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		issuer := fs.String("issuer", "", "")
		repoF := fs.String("repo", "", "tenant/repo")
		format := fs.String("format", "text", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule list: %v\n", err)
			return 1
		}
		defer st.Close()
		var rules []auth.OIDCTrustRule
		switch {
		case *repoF != "":
			tn, rp, ok := strings.Cut(*repoF, "/")
			if !ok {
				fmt.Fprintln(stderr, "oidc rule list: --repo must be tenant/repo")
				return 2
			}
			rules, err = st.ListOIDCRulesForRepo(ctx, tn, rp)
		case *issuer != "":
			rules, err = st.ListOIDCRulesForIssuer(ctx, *issuer)
		default:
			fmt.Fprintln(stderr, "oidc rule list: --issuer or --repo required")
			return 2
		}
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule list: %v\n", err)
			return 1
		}
		for _, r := range rules {
			if *format == "json" {
				b, _ := json.Marshal(map[string]any{
					"id": r.ID, "issuer": r.IssuerAlias, "audience": r.Audience,
					"tenant": r.Tenant, "repo": r.Repo, "scopes": auth.FormatScopes(r.Scopes),
					"ttl_seconds": r.TTLSeconds, "claims": r.Claims, "wildcard": len(r.Claims) == 0,
				})
				fmt.Fprintln(stdout, string(b))
			} else {
				wc := ""
				if len(r.Claims) == 0 {
					wc = "  [WILDCARD: matches any token from issuer]"
				}
				fmt.Fprintf(stdout, "id=%s  issuer=%s  aud=%s  tenant=%s  repo=%s  scopes=%s  ttl=%ds  claims=%d%s\n",
					r.ID, r.IssuerAlias, r.Audience, r.Tenant, r.Repo,
					auth.FormatScopes(r.Scopes), r.TTLSeconds, len(r.Claims), wc)
			}
		}
		return 0
	case "remove":
		fs := flag.NewFlagSet("oidc rule remove", flag.ContinueOnError)
		fs.SetOutput(stderr)
		authDB := fs.String("auth-db", "", "")
		id := fs.String("id", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *authDB == "" || *id == "" {
			fmt.Fprintln(stderr, "oidc rule remove: --auth-db, --id required")
			return 2
		}
		st, err := openOIDCStore(*authDB)
		if err != nil {
			fmt.Fprintf(stderr, "oidc rule remove: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.RemoveOIDCRule(ctx, *id); err != nil {
			fmt.Fprintf(stderr, "oidc rule remove: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed id=%s\n", *id)
		return 0
	default:
		fmt.Fprintf(stderr, "oidc rule: unknown action %q\n", args[0])
		return 2
	}
}
