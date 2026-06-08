package buildtrigger

import (
	"encoding/json"
	"testing"
)

func TestRenderBody_GenericIncludesContext(t *testing.T) {
	p := BuildPayload{Tenant: "acme", Repo: "app", HeadOID: "abc",
		RefUpdate: RefUpdate{Refname: "refs/heads/main"}}
	body, err := RenderBody(KindGeneric, p, "")
	if err != nil { t.Fatal(err) }
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil { t.Fatal(err) }
	if m["repo"] != "app" || m["head_oid"] != "abc" {
		t.Fatalf("missing context: %v", m)
	}
	if _, ok := m["bvts_token"]; ok {
		t.Fatal("token must be absent when not injected")
	}
	// generic does NOT flatten ref/commit (only cloudbuild does)
	if _, ok := m["ref"]; ok {
		t.Fatal("generic must not flatten ref")
	}
}

func TestRenderBody_CloudBuildInjectsTokenAndFlattens(t *testing.T) {
	p := BuildPayload{Tenant: "acme", Repo: "app",
		RefUpdate: RefUpdate{Refname: "refs/heads/main", NewOID: "c0ffee"}}
	body, err := RenderBody(KindCloudBuild, p, "bvts_secret")
	if err != nil { t.Fatal(err) }
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil { t.Fatal(err) }
	if m["bvts_token"] != "bvts_secret" {
		t.Fatalf("expected injected token, got %v", m["bvts_token"])
	}
	if m["ref"] != "refs/heads/main" {
		t.Fatalf("expected flat ref, got %v", m["ref"])
	}
	if m["commit"] != "c0ffee" {
		t.Fatalf("expected flat commit, got %v", m["commit"])
	}
}

func TestRenderBody_GenericWithToken(t *testing.T) {
	p := BuildPayload{Repo: "app", RefUpdate: RefUpdate{Refname: "refs/heads/main"}}
	body, _ := RenderBody(KindGeneric, p, "tok")
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["bvts_token"] != "tok" {
		t.Fatalf("generic should still inject token when provided, got %v", m["bvts_token"])
	}
}
