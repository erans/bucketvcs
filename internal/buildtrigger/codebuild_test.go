package buildtrigger

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
)

type fakeStartBuild struct{ in *codebuild.StartBuildInput }

func (f *fakeStartBuild) StartBuild(ctx context.Context, in *codebuild.StartBuildInput, _ ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error) {
	f.in = in
	id := "b-1"
	return &codebuild.StartBuildOutput{Build: &cbtypes.Build{Id: &id}}, nil
}

func TestCodeBuildDeliverer_StartBuildInputs(t *testing.T) {
	fake := &fakeStartBuild{}
	d := &codeBuildDeliverer{
		clientFor: func(Trigger) (startBuildAPI, error) { return fake, nil },
		mintFn:    func(context.Context, Trigger, BuildPayload) (string, error) { return "bvts_x", nil },
	}
	tr := Trigger{Kind: KindCodeBuild, TokenMode: TokenInject,
		Config: Config{AWSRegion: "us-east-1", AWSProject: "app-release"}}
	p := BuildPayload{Tenant: "acme", Repo: "app", HeadOID: "abc123",
		RefUpdate: RefUpdate{Refname: "refs/tags/v1", NewOID: "abc123"}}
	if _, err := d.Deliver(context.Background(), tr, p); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if fake.in == nil || *fake.in.ProjectName != "app-release" || *fake.in.SourceVersion != "abc123" {
		t.Fatalf("bad StartBuild input: %+v", fake.in)
	}
	var sawToken, sawRef, sawRepo, sawCommit bool
	for _, ev := range fake.in.EnvironmentVariablesOverride {
		switch {
		case *ev.Name == "BVTS_TOKEN" && *ev.Value == "bvts_x":
			sawToken = true
		case *ev.Name == "BV_REF" && *ev.Value == "refs/tags/v1":
			sawRef = true
		case *ev.Name == "BV_REPO" && *ev.Value == "acme/app":
			sawRepo = true
		case *ev.Name == "BV_COMMIT" && *ev.Value == "abc123":
			sawCommit = true
		}
	}
	if !sawToken || !sawRef || !sawRepo || !sawCommit {
		t.Fatalf("missing env overrides: token=%v ref=%v repo=%v commit=%v", sawToken, sawRef, sawRepo, sawCommit)
	}
}

func TestCodeBuildDeliverer_NoTokenWhenModeNone(t *testing.T) {
	fake := &fakeStartBuild{}
	d := &codeBuildDeliverer{
		clientFor: func(Trigger) (startBuildAPI, error) { return fake, nil },
		mintFn:    func(context.Context, Trigger, BuildPayload) (string, error) { t.Fatal("must not mint"); return "", nil },
	}
	tr := Trigger{Kind: KindCodeBuild, TokenMode: TokenNone, Config: Config{AWSRegion: "us-east-1", AWSProject: "p"}}
	if _, err := d.Deliver(context.Background(), tr, BuildPayload{Repo: "app", HeadOID: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range fake.in.EnvironmentVariablesOverride {
		if *ev.Name == "BVTS_TOKEN" {
			t.Fatal("token must be absent in TokenNone mode")
		}
	}
}
