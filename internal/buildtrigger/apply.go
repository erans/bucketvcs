package buildtrigger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// applyDoc is the top-level structure of a declarative trigger document.
type applyDoc struct {
	Triggers []applyTrigger `yaml:"triggers"`
}

// applyTrigger is one entry in the declarative apply document.
type applyTrigger struct {
	Tenant       string   `yaml:"tenant"`
	Repo         string   `yaml:"repo"`
	Name         string   `yaml:"name"`
	Kind         string   `yaml:"kind"`
	URL          string   `yaml:"url"`
	Secret       string   `yaml:"secret"`
	AWSRegion    string   `yaml:"aws_region"`
	AWSProject   string   `yaml:"aws_project"`
	AWSConnector string   `yaml:"aws_connector"`
	RefInclude   []string `yaml:"ref_include"`
	RefExclude   []string `yaml:"ref_exclude"`
	TokenMode    string   `yaml:"token_mode"`
	TokenScopes  []string `yaml:"token_scopes"`
	TokenTTL     string   `yaml:"token_ttl"`
}

// ApplyResult summarises what Apply changed.
type ApplyResult struct {
	Created int
	Updated int
	Pruned  int
}

// Apply reconciles the declarative trigger document against the live state in
// svc. For each declared trigger:
//   - if no trigger with (tenant, repo, name) exists → Create (Created++).
//   - if one already exists → Remove the old one then Create the new one
//     (Updated++). This resets created_at which is acceptable; triggers are
//     operator configuration, not append-only records.
//
// If prune is true, any trigger in a covered (tenant, repo) that is NOT
// present in the document is removed (Pruned++).
func Apply(ctx context.Context, svc *Service, data []byte, prune bool) (ApplyResult, error) {
	var doc applyDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ApplyResult{}, fmt.Errorf("buildtrigger: apply unmarshal: %w", err)
	}

	// covered maps (tenant+"/"+repo) -> set of declared trigger names.
	type repoKey struct{ tenant, repo string }
	covered := make(map[repoKey]map[string]struct{})
	var res ApplyResult

	for _, at := range doc.Triggers {
		in, err := toInput(at)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("buildtrigger: apply trigger %s/%s/%s: %w", at.Tenant, at.Repo, at.Name, err)
		}

		rk := repoKey{at.Tenant, at.Repo}
		if covered[rk] == nil {
			covered[rk] = make(map[string]struct{})
		}
		covered[rk][at.Name] = struct{}{}

		existing, err := svc.findByName(ctx, at.Tenant, at.Repo, at.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			if _, err := svc.Create(ctx, in); err != nil {
				return ApplyResult{}, fmt.Errorf("buildtrigger: apply create %s/%s/%s: %w", at.Tenant, at.Repo, at.Name, err)
			}
			res.Created++
		case err != nil:
			return ApplyResult{}, fmt.Errorf("buildtrigger: apply findByName %s/%s/%s: %w", at.Tenant, at.Repo, at.Name, err)
		default:
			// Trigger exists: replace it (remove + create).
			if err := svc.Remove(ctx, existing.ID); err != nil {
				return ApplyResult{}, fmt.Errorf("buildtrigger: apply remove old %s: %w", existing.ID, err)
			}
			if _, err := svc.Create(ctx, in); err != nil {
				return ApplyResult{}, fmt.Errorf("buildtrigger: apply create replacement %s/%s/%s: %w", at.Tenant, at.Repo, at.Name, err)
			}
			res.Updated++
		}
	}

	if prune {
		for rk, names := range covered {
			all, err := svc.List(ctx, rk.tenant, rk.repo)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("buildtrigger: apply prune list %s/%s: %w", rk.tenant, rk.repo, err)
			}
			for _, tr := range all {
				if _, keep := names[tr.Name]; !keep {
					if err := svc.Remove(ctx, tr.ID); err != nil {
						return ApplyResult{}, fmt.Errorf("buildtrigger: apply prune remove %s: %w", tr.ID, err)
					}
					res.Pruned++
				}
			}
		}
	}

	return res, nil
}

// toInput converts an applyTrigger into a TriggerInput, parsing duration and
// scope strings.
func toInput(at applyTrigger) (TriggerInput, error) {
	in := TriggerInput{
		Tenant: at.Tenant,
		Repo:   at.Repo,
		Name:   at.Name,
		Kind:   Kind(at.Kind),
		Config: Config{
			URL:          at.URL,
			Secret:       at.Secret,
			AWSRegion:    at.AWSRegion,
			AWSProject:   at.AWSProject,
			AWSConnector: at.AWSConnector,
		},
		RefInclude: at.RefInclude,
		RefExclude: at.RefExclude,
		TokenMode:  TokenMode(at.TokenMode),
	}

	if at.TokenTTL != "" {
		d, err := time.ParseDuration(at.TokenTTL)
		if err != nil {
			return TriggerInput{}, fmt.Errorf("token_ttl %q: %w", at.TokenTTL, err)
		}
		in.TokenTTL = d
	}

	if len(at.TokenScopes) > 0 {
		scopes, err := auth.ParseScopes(strings.Join(at.TokenScopes, ","))
		if err != nil {
			return TriggerInput{}, fmt.Errorf("token_scopes: %w", err)
		}
		in.TokenScopes = scopes
	}

	return in, nil
}
